package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/tj/go-naturaldate"
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
	config                = Config{}
	jiraApiCallCount      = 0
	progress              *mpb.Progress
	calendar              *bool
	calendarPath          *string
	calendarLookahead     *string
	calendarLookaheadTime *time.Time
)

func init() {
	progress = mpb.New(
		mpb.WithOutput(color.Output),
		mpb.WithAutoRefresh(),
	)
}

func main() {

	slog.SetLogLoggerLevel(slog.LevelWarn)

	var err error

	configFile := flag.String("config-path", "./config.json", "Config file to use")
	// LogseqRoot:=

	calendar = flag.Bool("calendar", false, "Whether to just parse calendar tags (into Markwhen)")
	calendarPath = flag.String("calendar-path", "./calendar.mw", "Where to parse the calendar to")
	calendarLookahead = flag.String("calendar-lookahead", "", "How far to look ahead")

	flag.Parse()

	if *calendar && calendarLookahead != nil && *calendarLookahead != "" {
		now := time.Now()
		t, err := naturaldate.Parse(*calendarLookahead, now)
		if err != nil {
			slog.Error(err.Error())
		}
		if t == now {
			slog.Error("Unrecognized natural date string")
		}
		calendarLookaheadTime = &t
	}

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	configRaw, err := os.ReadFile(*configFile)
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

	err = WriteCalendar()

	if err != nil {
		slog.Error("Failed in WriteCalendar", err)
	}

	slog.Info("exiting")

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
