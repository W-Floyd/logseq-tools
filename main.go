package main

import (
	"encoding/json"
	"log"
	"os"
	"path"
	"regexp"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
)

type Config struct {
	LogseqRoot string `json:"logseq_root"`
	Jira       struct {
		Enabled          bool     `json:"enabled"`            // Whether to process Jira
		IncludeWatchers  bool     `json:"include_watchers"`   // This can be slow, so you may want to disable it
		IncludeComments  bool     `json:"include_comments"`   // This can be slow, so you may want to disable it
		ExcludeFromGraph bool     `json:"exclude_from_graph"` // If you have a lot of these, it can easily polute your graph
		IncludeDone      bool     `json:"include_done"`       // Whether to include done items to help clean up the list
		DoneStatus       []string `json:"done_status"`        // Names to consider as done
		// TODO - Implement
		// IncludeURL       bool         `json:"include_url"`        // Whether to include the URL in the page name to disambiguate instances
		Instances []JiraConfig `json:"instances"` // Jira instances to process
	} `json:"jira"`
}

var config = Config{}

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

	if config.Jira.Enabled {

		for _, instance := range config.Jira.Instances {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err = instance.Process(&wg)
			}()
			if err != nil {
				log.Fatalln(err)
			}
		}

	}

	wg.Wait()

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
