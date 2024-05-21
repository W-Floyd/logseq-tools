package main

import (
	"encoding/json"
	"log"
	"os"
	"path"
	"regexp"
	"time"

	"github.com/dustin/go-humanize"
)

type Config struct {
	LogseqRoot string `json:"logseq_root"`
	Jira       struct {
		Enabled    bool         `json:"enabled"`     // Whether to process Jira
		IncludeURL bool         `json:"include_url"` // Whether to include the URL in the page name to disambiguate instances
		Instances  []JiraConfig `json:"instances"`   // Jira instances to process
	} `json:"jira"`
}

var config = Config{}

func main() {

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
			err := instance.Process()
			if err != nil {
				log.Fatalln(err)
			}
		}

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
