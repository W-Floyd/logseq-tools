package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	LogseqRoot string `json:"logseq_root"`
	Jira       struct {
		Instances []*JiraConfig `json:"instances"` // Jira instances to process
		Users     []struct {
			AccountID   string `json:"account_id"`   // Account ID to match
			DisplayName string `json:"display_name"` // Display name to print in place
		} `json:"users"`
	} `json:"jira"`
}

var (
	config           = Config{}
	jiraApiCallCount = 0
)

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

	ctx := context.Background()
	errs, _ := errgroup.WithContext(ctx)

	for _, instance := range config.Jira.Instances {
		if instance.Enabled {
			instance := instance
			errs.Go(
				func() error {
					return instance.Process(errs)
				},
			)
		}
	}

	err = errs.Wait()

	if jiraApiCallCount > 0 {
		IssueMap()
		log.Println("Jira API calls: " + strconv.Itoa(jiraApiCallCount))
	}

	if err != nil {
		if err, ok := err.(stackTracer); ok {
			for _, f := range err.StackTrace() {
				fmt.Printf("%+s:%d\n", f, f)
			}
		}
		log.Fatalln(err)
	}

}

func WritePage(title string, contents []byte) error {

	outputFile := path.Join(config.LogseqRoot, "pages", PageNameToFileName(title)+".md")

	log.Println("Attempting to create file: " + outputFile)

	dir := regexp.MustCompile("[^/]*$").ReplaceAllString(outputFile, "")

	err := os.MkdirAll(dir, os.ModeDir)
	if err != nil {
		return errors.Wrap(err, "Couldn't make directory "+dir)
	}

	return os.WriteFile(outputFile, contents, 0644)
}

func DateFormat(input time.Time) string {
	return input.Format("Jan") + " " + humanize.Ordinal(input.Day()) + ", " + input.Format("2006")
}
