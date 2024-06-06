package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strconv"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/vbauerster/mpb/v8"
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
	progress         *mpb.Progress
	red, green       = color.New(color.FgRed), color.New(color.FgGreen)
)

func init() {
	progress = mpb.New(
		mpb.WithOutput(color.Output),
		mpb.WithAutoRefresh(),
	)
}

func main() {

	slog.SetLogLoggerLevel(slog.LevelWarn)

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	configFile := "./config.json"

	configRaw, err := os.ReadFile(configFile)
	if err != nil {
		slog.Error(err.Error())
	}

	err = json.Unmarshal(configRaw, &config)
	if err != nil {
		slog.Error(err.Error())
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
		slog.Info("Jira API calls: " + strconv.Itoa(jiraApiCallCount))
	}

	if err != nil {
		if err, ok := err.(stackTracer); ok {
			for _, f := range err.StackTrace() {
				fmt.Printf("%+s:%d\n", f, f)
			}
		}
		slog.Error(err.Error())
	}

}

func WritePage(title string, contents []byte) error {

	return WriteFile(path.Join(config.LogseqRoot, "pages", PageNameToFileName(title)+".md"), contents)

}

func WriteFile(path string, contents []byte) error {

	slog.Info("Attempting to create file: " + path)

	dir := regexp.MustCompile("[^/]*$").ReplaceAllString(path, "")

	err := os.MkdirAll(dir, os.ModeDir)
	if err != nil {
		return errors.Wrap(err, "Couldn't make directory "+dir)
	}

	return os.WriteFile(path, contents, 0644)
}
