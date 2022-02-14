package main

import (
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/etag"
)

var monitored_instances = []Instance{}

var number_re = regexp.MustCompile(`(?m)(\d+)`)

type Instance struct {
	Name        string `json:"name"`
	ApiUrl      string `json:"api_url"`
	Locations   string `json:"locations"`
	Cdn         bool   `json:"cdn"`
	Registered  int    `json:"registered"`
	LastChecked int64  `json:"last_checked"`
}

func monitorInstances() {
	// do forever
	for {
		// send a request to get markdown from GitHub
		resp, err := http.Get("https://raw.githubusercontent.com/wiki/TeamPiped/Piped-Frontend/Instances.md")
		if err != nil {
			log.Print(err)
			continue
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

			skipped := 0
			for _, line := range lines {
				split := strings.Split(line, "|")
				if len(split) >= 5 {
					if skipped < 2 {
						skipped++
						continue
					}
					ApiUrl := strings.TrimSpace(split[1])
					resp, err := http.Get(ApiUrl + "/healthcheck")
					if err != nil {
						log.Print(err)
						continue
					}
					LastChecked := time.Now().Unix()
					if resp.StatusCode != 200 {
						continue
					}
					resp, err = http.Get(ApiUrl + "/registered/badge")
					if err != nil {
						log.Print(err)
						continue
					}
					registered, err := strconv.ParseInt(number_re.FindString(resp.Request.URL.Path), 10, 32)
					if err != nil {
						log.Print(err)
						continue
					}

					instances = append(instances, Instance{
						Name:        strings.TrimSpace(split[0]),
						ApiUrl:      ApiUrl,
						Locations:   strings.TrimSpace(split[2]),
						Cdn:         strings.TrimSpace(split[3]) == "Yes",
						Registered:  int(registered),
						LastChecked: LastChecked,
					})

				}
			}

			// update the global instances variable
			monitored_instances = instances
		}
		resp.Body.Close()
		time.Sleep(time.Minute)
	}
}

func main() {
	go monitorInstances()

	app := fiber.New(
		fiber.Config{
			Prefork: true,
		},
	)
	app.Use(etag.New())

	app.Get("/", func(c *fiber.Ctx) error {
		return c.JSON(monitored_instances)
	})

	app.Listen(":3000")
}
