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
	"github.com/google/go-github/v53/github"
	"golang.org/x/oauth2"
)

var monitored_instances = []Instance{}

var number_re = regexp.MustCompile(`(?m)(\d+)`)

type Instance struct {
	Name        string `json:"name"`
	ApiUrl      string `json:"api_url"`
	Locations   string `json:"locations"`
	Version     string `json:"version"`
	UpToDate    bool   `json:"up_to_date"`
	Cdn         bool   `json:"cdn"`
	Registered  int    `json:"registered"`
	LastChecked int64  `json:"last_checked"`
	Cache       bool   `json:"cache"`
	S3Enabled   bool   `json:"s3_enabled"`
}

type FrontendConfig struct {
	S3Enabled bool `json:"s3Enabled"`
}

func testUrl(url string) (*http.Response, error) {
	resp, err := http.Get(url)
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

func getInstanceDetails(line string, latest string) (Instance, error) {
	split := strings.Split(line, "|")
	if len(split) < 5 {
		return Instance{}, errors.New(fmt.Sprintf("Invalid line: %s", line))
	}
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
		if _, err := testUrl(ApiUrl + "/streams/jNQXAC9IVRw"); err != nil {
			errorChannel <- err
		}
	}()

	for err := range errorChannel {
		return Instance{}, err
	}

	return Instance{
		Name:        strings.TrimSpace(split[0]),
		ApiUrl:      ApiUrl,
		Locations:   strings.TrimSpace(split[2]),
		Cdn:         strings.TrimSpace(split[3]) == "Yes",
		Registered:  int(registered),
		LastChecked: lastChecked,
		Version:     version,
		UpToDate:    strings.Contains(latest, hash),
		Cache:       cacheWorking,
		S3Enabled:   config.S3Enabled,
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

			instances := []Instance{}

			wg := sync.WaitGroup{}
			for index, line := range lines {
				// skip first two and last line
				if index < 2 || index == len(lines)-1 {
					continue
				}

				wg.Add(1)
				go func(line string) {
					defer wg.Done()
					instance, err := getInstanceDetails(line, latest)
					if err == nil {
						instances = append(instances, instance)
					} else {
						log.Print(err)
					}
				}(line)
			}
			wg.Wait()

			// update the global instances variable
			monitored_instances = instances
		}
		resp.Body.Close()
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

	fmt.Println("Listening on http://localhost:3000")
	app.Listen(":3000")
}
