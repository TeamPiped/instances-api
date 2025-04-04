package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/InfluxCommunity/influxdb3-go/influxdb3"
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
	"github.com/google/go-github/v62/github"
)

const INTERVAL_WEEK_IN_HOURS = 7 * 24
const INTERVAL_MONTH_IN_HOURS = 30 * 24

var monitored_instances = []Instance{}
var inactive_instances = map[int][]Instance{}

var number_re = regexp.MustCompile(`(?m)(\d+)`)

type Instance struct {
	Name                 string  `json:"name"`
	ApiUrl               string  `json:"api_url"`
	Locations            string  `json:"locations"`
	Version              string  `json:"version"`
	UpToDate             bool    `json:"up_to_date"`
	Cdn                  bool    `json:"cdn"`
	Registered           int     `json:"registered"`
	LastChecked          int64   `json:"last_checked"`
	Cache                bool    `json:"cache"`
	S3Enabled            bool    `json:"s3_enabled"`
	ImageProxyUrl        string  `json:"image_proxy_url"`
	RegistrationDisabled bool    `json:"registration_disabled"`
	Uptime24h            float32 `json:"uptime_24h"`
	Uptime7d             float32 `json:"uptime_7d"`
	Uptime30d            float32 `json:"uptime_30d"`
}

type FrontendConfig struct {
	S3Enabled            bool   `json:"s3Enabled"`
	ImageProxyUrl        string `json:"imageProxyUrl"`
	RegistrationDisabled bool   `json:"registrationDisabled"`
}

var client = http.Client{
	Timeout: 10 * time.Second,
}

var influxdbClient *influxdb3.Client

func testUrl(url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Piped-Instances-API/(https://github.com/TeamPiped/instances-api)")
	resp, err := client.Do(req)
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

func storeUptimeHistory(apiUrl string, uptimeStatus bool) {
	// Create a new point
	p := influxdb3.NewPointWithMeasurement("uptime").
		SetTag("apiUrl", apiUrl).
		SetField("status", uptimeStatus).
		SetTimestamp(time.Now())

	points := []*influxdb3.Point{p}

	// Write the point
	err := influxdbClient.WritePoints(context.Background(), points)
	if err != nil {
		log.Print(err)
	}
}

func getUptimePercentage(apiUrl string, hours int) (float32, error) {
	// Fetch uptime history from InfluxDB for the given period
	query := fmt.Sprintf(`SELECT "status" FROM "uptime" WHERE "apiUrl" = '%s' AND time >= now() - interval '%d hours'`, apiUrl, hours)
	result, err := influxdbClient.Query(context.Background(), query)
	if err != nil {
		return 0, err
	}

	// Calculate uptime percentage
	successCount, failureCount := 0, 0
	for result.Next() {
		uptimeStatus := result.Value()["status"].(bool)
		if uptimeStatus {
			successCount++
		} else {
			failureCount++
		}
	}
	uptimePercentage := float32(successCount) / float32(successCount+failureCount) * 100

	return uptimePercentage, nil
}

func getInstanceDetails(instanceBaseInfo Instance, latest string) (Instance, error) {
	wg := sync.WaitGroup{}
	errorChannel := make(chan error, 9)
	// the amount of tests to do
	wg.Add(5)
	// Add 3 more for uptime history
	wg.Add(3)

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
		if _, err := testUrl(instanceBaseInfo.ApiUrl + "/healthcheck"); err != nil {
			errorChannel <- err
			return
		}
		lastChecked = time.Now().Unix()
	}()

	go func() {
		defer wg.Done()
		resp, err := testUrl(instanceBaseInfo.ApiUrl + "/registered/badge")
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
		resp, err := testUrl(instanceBaseInfo.ApiUrl + "/version")
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
		config, err = getConfig(instanceBaseInfo.ApiUrl)
		if err != nil {
			errorChannel <- err
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		cacheWorking, err = testCaching(instanceBaseInfo.ApiUrl)
		if err != nil {
			errorChannel <- err
		}
	}()

	var uptime24h, uptime7d, uptime30d float32

	go func() {
		defer wg.Done()
		var err error
		uptime24h, err = getUptimePercentage(instanceBaseInfo.ApiUrl, 24)
		if err != nil {
			errorChannel <- err
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		uptime7d, err = getUptimePercentage(instanceBaseInfo.ApiUrl, 24*7)
		if err != nil {
			errorChannel <- err
		}
	}()

	go func() {
		defer wg.Done()
		var err error
		uptime30d, err = getUptimePercentage(instanceBaseInfo.ApiUrl, 24*30)
		if err != nil {
			errorChannel <- err
		}
	}()

	for err := range errorChannel {
		go func() {
			// Store the uptime history
			storeUptimeHistory(instanceBaseInfo.ApiUrl, err == nil)
		}()
		return Instance{}, err
	}

	go func() {
		// Store the uptime history
		storeUptimeHistory(instanceBaseInfo.ApiUrl, true)
	}()

	return Instance{
		Name:                 instanceBaseInfo.Name,
		ApiUrl:               instanceBaseInfo.ApiUrl,
		Locations:            instanceBaseInfo.Locations,
		Cdn:                  instanceBaseInfo.Cdn,
		Registered:           int(registered),
		LastChecked:          lastChecked,
		Version:              version,
		UpToDate:             strings.Contains(latest, hash),
		Cache:                cacheWorking,
		S3Enabled:            config.S3Enabled,
		ImageProxyUrl:        config.ImageProxyUrl,
		RegistrationDisabled: config.RegistrationDisabled,
		Uptime24h:            uptime24h,
		Uptime7d:             uptime7d,
		Uptime30d:            uptime30d,
	}, nil
}

func getInstancesBaseList() ([]Instance, error) {
	req, err := http.NewRequest("GET", "https://raw.githubusercontent.com/TeamPiped/documentation/refs/heads/main/content/docs/public-instances/index.md", nil)
	if err != nil {
		log.Print(err)
		return []Instance{}, err
	}
	req.Header.Set("User-Agent", "Piped-Instances-API/(https://github.com/TeamPiped/instances-api)")
	resp, err := client.Do(req)
	if err != nil {
		log.Print(err)
		return []Instance{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return []Instance{}, errors.New("Invalid response code when fetching instances!")
	}
	// parse the response
	buf := new(strings.Builder)
	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		log.Print(err)
		return []Instance{}, err
	}

	lines := strings.Split(buf.String(), "\n")

	skipped := 0
	var instances []Instance
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

		instance := Instance{
			Name:      split[0],
			ApiUrl:    strings.TrimSpace(split[1]),
			Locations: split[2],
			Cdn:       split[3] == "Yes",
		}
		instances = append(instances, instance)
	}

	return instances, nil
}

func monitorInstances() {
	ctx := context.Background()
	ghClient := github.NewClient(nil)

	if os.Getenv("GITHUB_TOKEN") != "" {
		ghClient = ghClient.WithAuthToken(os.Getenv("GITHUB_TOKEN"))
	}

	// do forever
	for {
		// send a request to get markdown from GitHub

		// Find Latest Commit from GitHub
		var latest string
		{
			commits, _, err := ghClient.Repositories.ListCommits(ctx, "TeamPiped", "Piped-Backend", &github.CommitsListOptions{
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
		instancesMap := make(map[int]Instance)

		wg := sync.WaitGroup{}

		instanceBaseInfos, err := getInstancesBaseList()
		if err != nil {
			continue
		}
		for i, instance := range instanceBaseInfos {
			wg.Add(1)

			go func(i int, instance Instance) {
				defer wg.Done()
				instance, err := getInstanceDetails(instance, latest)
				if err == nil {
					instancesMap[i] = instance
				} else {
					log.Print(err)
				}
			}(i, instance)
		}
		wg.Wait()

		// Map to ordered array
		var instances []Instance
		for i := 0; i < len(instanceBaseInfos); i++ {
			instance, ok := instancesMap[i]
			if ok {
				instances = append(instances, instance)
			}
		}

		// update the global instances variable
		monitored_instances = instances

		// update the list of inactive instances
		updateInactiveInstances(instanceBaseInfos, INTERVAL_WEEK_IN_HOURS)
		updateInactiveInstances(instanceBaseInfos, INTERVAL_MONTH_IN_HOURS)

		time.Sleep(time.Minute)
	}
}

func updateInactiveInstances(instances []Instance, intervalHours int) {
	var inactive []Instance
	for _, instance := range instances {
		uptime, err := getUptimePercentage(instance.ApiUrl, intervalHours)
		if err == nil && uptime == 0 {
			inactive = append(inactive, instance)
		}
	}
	inactive_instances[intervalHours] = inactive
}

func getInactiveInstances(intervalHours int) any {
	instances, ok := inactive_instances[intervalHours]
	if !ok {
		return fiber.Map{"error": fmt.Sprintf("No data for time interval %d hours yet!", intervalHours)}
	}

	return instances
}

func main() {
	// create influxdb client
	{
		client, err := influxdb3.New(influxdb3.ClientConfig{
			Host:     os.Getenv("INFLUXDB_URL"),
			Token:    os.Getenv("INFLUXDB_TOKEN"),
			Database: os.Getenv("INFLUXDB_DATABASE"),
		})
		if err != nil {
			panic(err)
		}

		influxdbClient = client
	}

	go monitorInstances()

	app := fiber.New()
	app.Use(cors.New())
	app.Use(etag.New())

	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(monitored_instances)
	})
	app.Get("/inactive", func(c *fiber.Ctx) error {
		return c.JSON(getInactiveInstances(INTERVAL_MONTH_IN_HOURS))
	})
	app.Get("/inactive/7", func(c *fiber.Ctx) error {
		return c.JSON(getInactiveInstances(INTERVAL_WEEK_IN_HOURS))
	})
	app.Get("/inactive/30", func(c *fiber.Ctx) error {
		return c.JSON(getInactiveInstances(INTERVAL_MONTH_IN_HOURS))
	})

	err := app.Listen(":3000")
	if err != nil {
		panic(err)
	}
}
