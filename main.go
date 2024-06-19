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
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	LogseqRoot string `json:"logseq_root"`
	CacheRoot  string `json:"cache_root"`
	Jira       struct {
		Instances []*JiraConfig `json:"instances"` // Jira instances to process
		Users     []struct {
			AccountID   string `json:"account_id"`   // Account ID to match
			DisplayName string `json:"display_name"` // Display name to print in place
		} `json:"users"`
	} `json:"jira"`
}

var (
	config                      = Config{}
	jiraApiCalls, jiraCacheHits *mpb.Bar
	progress                    *mpb.Progress
	calendar                    *bool
	calendarPath                *string
	logFile                     *string
	calendarLookahead           *string
	calendarLookaheadTime       *time.Time
	debug                       *bool
	verbose                     *bool
)

func init() {
	progress = mpb.New(
		mpb.WithOutput(color.Output),
		mpb.WithAutoRefresh(),
	)
	jiraApiCalls = progress.AddBar(0,
		mpb.PrependDecorators(
			decor.Name("API Calls", decor.WC{C: decor.DindentRight | decor.DextraSpace}),
			decor.CountersNoUnit("%d / %d", decor.WCSyncWidth),
		),
	)
	jiraCacheHits = progress.AddBar(0,
		mpb.PrependDecorators(
			decor.Name("Cache Hits", decor.WC{C: decor.DindentRight | decor.DextraSpace}),
			decor.CountersNoUnit("%d / %d", decor.WCSyncWidth),
		),
	)
}

func main() {

	slog.SetLogLoggerLevel(slog.LevelWarn)

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	var err error

	configFile := flag.String("config-path", "./config.json", "Config file to use")
	// LogseqRoot:=

	calendar = flag.Bool("calendar", false, "Whether to just parse calendar tags (into Markwhen)")
	calendarPath = flag.String("calendar-path", "./calendar.mw", "Where to parse the calendar to")
	calendarLookahead = flag.String("calendar-lookahead", "", "How far to look ahead")
	debug = flag.Bool("debug", false, "Whether to create debug files")
	verbose = flag.Bool("verbose", false, "Whether to print more info")
	logFile = flag.String("log-file", "./logfile", "Log file to use")

	flag.Parse()

	f, err := os.OpenFile(*logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		slog.Error("error opening file: ", err)
		return
	}
	defer f.Close()

	log.SetOutput(f)

	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}

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

	if jiraApiCalls.Current() > 0 {
		IssueMap()
		slog.Info("Jira API calls: " + strconv.Itoa(int(jiraApiCalls.Current())))
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
