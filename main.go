package main

import (
	"encoding/json"
	"log"
	"os"
	"path"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
)

type Config struct {
	LogseqRoot string `json:"logseq_root"`
	Jira       struct {
		Instances []*JiraConfig `json:"instances"` // Jira instances to process
	} `json:"jira"`
}

var (
	config           = Config{}
	jiraApiCallCount = 0
)

func main() {

	var wg sync.WaitGroup

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	configFile := "./config.json"

	configRaw, err := os.ReadFile(configFile)
	if err != nil {
		log.Fatalln(err)
	}

	err = json.Unmarshal(configRaw, &config)
	if err != nil {
		log.Fatalln(err)
	}

	for _, instance := range config.Jira.Instances {
		if instance.Enabled {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err = instance.Process(&wg)
				if err != nil {
					log.Fatalln(err)
				}
			}()
		}
	}

	wg.Wait()

	if jiraApiCallCount > 0 {
		log.Println("Jira API calls: " + strconv.Itoa(jiraApiCallCount))
	}

}

func WritePage(title string, contents []byte) error {

	outputFile := path.Join(config.LogseqRoot, "pages", PageNameToFileName(title)+".md")

	log.Println("Attempting to create file: " + outputFile)

	err := os.MkdirAll(regexp.MustCompile("[^/]*$").ReplaceAllString(outputFile, ""), os.ModeDir)
	if err != nil {
		return err
	}

	return os.WriteFile(outputFile, contents, 0644)
}

func DateFormat(input time.Time) string {
	return input.Format("Jan") + " " + humanize.Ordinal(input.Day()) + ", " + input.Format("2006")
}
