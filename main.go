package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/etag"
	"github.com/google/go-github/v54/github"
	"golang.org/x/oauth2"
)

var monitored_instances = []Instance{}

var number_re = regexp.MustCompile(`(?m)(\d+)`)

type Instance struct {
	Name                 string `json:"name"`
	ApiUrl               string `json:"api_url"`
	Locations            string `json:"locations"`
	Version              string `json:"version"`
	UpToDate             bool   `json:"up_to_date"`
	Cdn                  bool   `json:"cdn"`
	Registered           int    `json:"registered"`
	LastChecked          int64  `json:"last_checked"`
	Cache                bool   `json:"cache"`
	S3Enabled            bool   `json:"s3_enabled"`
	ImageProxyUrl        string `json:"image_proxy_url"`
	RegistrationDisabled bool   `json:"registration_disabled"`
}

type FrontendConfig struct {
	S3Enabled            bool   `json:"s3Enabled"`
	ImageProxyUrl        string `json:"imageProxyUrl"`
	RegistrationDisabled bool   `json:"registrationDisabled"`
}

type PipedStreams struct {
	Url string `json:"url"`
}

type Streams struct {
	VideoStreams []PipedStreams `json:"videoStreams"`
}

var client = http.Client{
	Timeout: 10 * time.Second,
}

func testUrl(url string) (*http.Response, error) {
	resp, err := client.Get(url)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode != 200 {
		return resp, errors.New(fmt.Sprintf("Invalid response code at %s: %d", url, resp.StatusCode))
	}
	return resp, err
}

func testCaching(ApiUrl string) (bool, error) {
	resp, err := testUrl(ApiUrl + "/trending?region=US")
	if err != nil {
		return false, err
	}
	oldTiming := resp.Header.Get("Server-Timing")
	resp, err = testUrl(ApiUrl + "/trending?region=US")
	if err != nil {
		return false, err
	}
	newTiming := resp.Header.Get("Server-Timing")
	cacheWorking := oldTiming == newTiming
	return cacheWorking, nil
}

func getConfig(ApiUrl string) (FrontendConfig, error) {
	resp, err := testUrl(ApiUrl + "/config")
	if err != nil {
		return FrontendConfig{}, err
	}
	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return FrontendConfig{}, err
	}
	var config FrontendConfig
	err = json.Unmarshal(bytes, &config)
	if err != nil {
		return FrontendConfig{}, err
	}
	return config, nil
}

func getStreams(ApiUrl string, VideoId string) (Streams, error) {
	resp, err := testUrl(ApiUrl + "/streams/" + VideoId)
	if err != nil {
		return Streams{}, err
	}
	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return Streams{}, err
	}
	var streams Streams
	err = json.Unmarshal(bytes, &streams)
	if err != nil {
		return Streams{}, err
	}
	return streams, nil
}

func getInstanceDetails(split []string, latest string) (Instance, error) {
	ApiUrl := strings.TrimSpace(split[1])

	wg := sync.WaitGroup{}
	errorChannel := make(chan error)
	// the amount of tests to do
	wg.Add(6)

	var lastChecked int64
	var registered int64
	var config FrontendConfig
	var hash string
	var version string
	var cacheWorking bool

	go func() {
		wg.Wait()
		close(errorChannel)
	}()

	go func() {
		defer wg.Done()
		if _, err := testUrl(ApiUrl + "/healthcheck"); err != nil {
			errorChannel <- err
			return
		}
		lastChecked = time.Now().Unix()
	}()

	go func() {
		defer wg.Done()
		resp, err := testUrl(ApiUrl + "/registered/badge")
		if err != nil {
			errorChannel <- err
			return
		}
		registered, err = strconv.ParseInt(number_re.FindString(resp.Request.URL.Path), 10, 32)
		if err != nil {
			errorChannel <- err
		}
	}()

	go func() {
		defer wg.Done()
		resp, err := testUrl(ApiUrl + "/version")
		if err != nil {
			errorChannel <- err
			return
		}
		buf := new(strings.Builder)
		_, err = io.Copy(buf, resp.Body)
		if err != nil {
			errorChannel <- err
			return
		}
		version = strings.TrimSpace(buf.String())
		version_split := strings.Split(version, "-")
		hash = version_split[len(version_split)-1]
	}()

	go func() {
		defer wg.Done()
		var err error
		config, err = getConfig(ApiUrl)
		if err != nil {
			errorChannel <- err
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		cacheWorking, err = testCaching(ApiUrl)
		if err != nil {
			errorChannel <- err
		}
	}()

	go func() {
		defer wg.Done()
		// check if instance can fetch videos
		streams, err := getStreams(ApiUrl, "jNQXAC9IVRw")
		if err != nil {
			errorChannel <- err
			return
		}
		if len(streams.VideoStreams) == 0 {
			errorChannel <- errors.New("no streams")
		}
		// head request to check first stream
		if _, err := testUrl(streams.VideoStreams[0].Url); err != nil {
			errorChannel <- err
		}
	}()

	for err := range errorChannel {
		return Instance{}, err
	}

	return Instance{
		Name:                 strings.TrimSpace(split[0]),
		ApiUrl:               ApiUrl,
		Locations:            strings.TrimSpace(split[2]),
		Cdn:                  strings.TrimSpace(split[3]) == "Yes",
		Registered:           int(registered),
		LastChecked:          lastChecked,
		Version:              version,
		UpToDate:             strings.Contains(latest, hash),
		Cache:                cacheWorking,
		S3Enabled:            config.S3Enabled,
		ImageProxyUrl:        config.ImageProxyUrl,
		RegistrationDisabled: config.RegistrationDisabled,
	}, nil
}

func monitorInstances() {
	ctx := context.Background()
	var tc *http.Client
	if os.Getenv("GITHUB_TOKEN") != "" {
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: os.Getenv("GITHUB_TOKEN")},
		)
		tc = oauth2.NewClient(ctx, ts)
	}
	gh_client := github.NewClient(tc)
	// do forever
	for {
		// send a request to get markdown from GitHub
		resp, err := http.Get("https://raw.githubusercontent.com/wiki/TeamPiped/Piped-Frontend/Instances.md")
		if err != nil {
			log.Print(err)
			continue
		}

		// Find Latest Commit from GitHub
		var latest string
		{
			commits, _, err := gh_client.Repositories.ListCommits(ctx, "TeamPiped", "Piped-Backend", &github.CommitsListOptions{
				ListOptions: github.ListOptions{
					PerPage: 1,
				},
			})
			if err != nil {
				log.Print(err)
				time.Sleep(time.Second * 5)
				continue
			}
			latest = commits[0].GetSHA()
		}

		if resp.StatusCode == 200 {
			// parse the response
			buf := new(strings.Builder)
			_, err := io.Copy(buf, resp.Body)

			if err != nil {
				log.Print(err)
				continue
			}

			lines := strings.Split(buf.String(), "\n")

			instancesMap := make(map[int]Instance)

			wg := sync.WaitGroup{}

			skipped := 0
			checking := 0
			for _, line := range lines {
				split := strings.Split(line, "|")

				if len(split) < 5 {
					continue
				}

				// skip first two table lines
				if skipped < 2 {
					skipped++
					continue
				}

				wg.Add(1)
				go func(i int, split []string) {
					defer wg.Done()
					instance, err := getInstanceDetails(split, latest)
					if err == nil {
						instancesMap[i] = instance
					} else {
						log.Print(err)
					}
				}(checking, split)
				checking++
			}
			wg.Wait()

			// Map to ordered array
			var instances []Instance
			for i := 0; i < checking; i++ {
				instance, ok := instancesMap[i]
				if ok {
					instances = append(instances, instance)
				}
			}

			// update the global instances variable
			monitored_instances = instances
		}
		_ = resp.Body.Close()
		time.Sleep(time.Minute)
	}
}

func main() {
	go monitorInstances()

	app := fiber.New()
	app.Use(cors.New())
	app.Use(etag.New())

	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(monitored_instances)
	})

	err := app.Listen(":3000")
	if err != nil {
		panic(err)
	}
}
