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

func getInstanceDetails(line string, latest string) (Instance, error) {
	split := strings.Split(line, "|")
	if len(split) >= 5 {
		ApiUrl := strings.TrimSpace(split[1])

		resp, err := http.Get(ApiUrl + "/healthcheck")
		if err != nil {
			return Instance{}, err
		}
		LastChecked := time.Now().Unix()
		if resp.StatusCode != 200 {
			return Instance{}, errors.New("Invalid response code")
		}
		resp, err = http.Get(ApiUrl + "/registered/badge")
		if err != nil {
			return Instance{}, err
		}
		registered, err := strconv.ParseInt(number_re.FindString(resp.Request.URL.Path), 10, 32)
		if err != nil {
			return Instance{}, err
		}
		resp, err = http.Get(ApiUrl + "/version")
		if err != nil {
			return Instance{}, err
		}
		if resp.StatusCode != 200 {
			return Instance{}, errors.New("Invalid response code")
		}
		buf := new(strings.Builder)
		_, err = io.Copy(buf, resp.Body)
		if err != nil {
			return Instance{}, err
		}
		version := strings.TrimSpace(buf.String())
		version_split := strings.Split(version, "-")
		hash := version_split[len(version_split)-1]

		resp, err = http.Get(ApiUrl + "/config")
		if err != nil {
			return Instance{}, err
		}
		if resp.StatusCode != 200 {
			return Instance{}, errors.New("Invalid response code")
		}

		bytes, err := io.ReadAll(resp.Body)

		if err != nil {
			return Instance{}, err
		}

		var config FrontendConfig

		err = json.Unmarshal(bytes, &config)

		if err != nil {
			return Instance{}, err
		}

		cache_working := false

		resp, err = http.Get(ApiUrl + "/trending?region=US")
		if err != nil {
			return Instance{}, err
		}
		if resp.StatusCode == 200 {
			old_timing := resp.Header.Get("Server-Timing")
			resp, err = http.Get(ApiUrl + "/trending?region=US")
			if err != nil {
				return Instance{}, err
			}
			if resp.StatusCode == 200 {
				new_timing := resp.Header.Get("Server-Timing")
				if old_timing == new_timing {
					cache_working = true
				}
			}
		}

		// check if instance can fetch videos
		resp, err = http.Get(ApiUrl + "/streams/jNQXAC9IVRw")
		if err != nil {
			return Instance{}, err
		}
		if resp.StatusCode != 200 {
			return Instance{}, errors.New("Invalid status code")
		}

		return Instance{
			Name:        strings.TrimSpace(split[0]),
			ApiUrl:      ApiUrl,
			Locations:   strings.TrimSpace(split[2]),
			Cdn:         strings.TrimSpace(split[3]) == "Yes",
			Registered:  int(registered),
			LastChecked: LastChecked,
			Version:     version,
			UpToDate:    strings.Contains(latest, hash),
			Cache:       cache_working,
			S3Enabled:   config.S3Enabled,
		}, nil
	}
	return Instance{}, errors.New("Invalid line")
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
				if index < 3 {
					fmt.Println(line)
					continue
				}

				wg.Add(1)
				go func(line string) {
					instance, err := getInstanceDetails(line, latest)
					if err == nil {
						instances = append(instances, instance)
					} else {
						log.Print(err)
					}
					wg.Done()
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

	app.Listen(":3000")
}
