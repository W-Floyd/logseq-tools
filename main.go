package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/cloud"
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
	recent                      *bool
	ignoreCache                 *bool
	startTime                   = time.Now()
	lastRun                     *time.Time
	lastRunPath                 string
	knownIssues                 = map[string]*jira.Issue{}
	knownIssuePath              string
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

	calendar = flag.Bool("calendar", false, "Whether to just parse calendar tags (into Markwhen)")
	calendarPath = flag.String("calendar-path", "./calendar.mw", "Where to parse the calendar to")
	calendarLookahead = flag.String("calendar-lookahead", "", "How far to look ahead")
	debug = flag.Bool("debug", false, "Whether to create debug files")
	verbose = flag.Bool("verbose", false, "Whether to print more info")
	logFile = flag.String("log-file", "./logfile", "Log file to use")
	recent = flag.Bool("recent", true, "Whether to only check recent issues")
	ignoreCache = flag.Bool("ignore-cache", false, "Whether to ignore cached issues")

	flag.Parse()

	if *verbose {
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}

	if *debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	f, err := os.OpenFile(*logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		slog.Error("error opening file: ", err)
		return
	}
	defer f.Close()

	if *ignoreCache && *recent {
		slog.Error("Cannot look for recent only without cache")
		return
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

	lastRunPath = strings.Join([]string{config.CacheRoot, "lastRun"}, "/") + ".json"

	if *recent {

		jsonFile, err := os.Open(lastRunPath)

		if err != nil {
			slog.Error("Failed to find or open file for last run timing, running as if you didn't specify -recent")
			*recent = false
		} else {

			byteValue, _ := io.ReadAll(jsonFile)

			err = json.Unmarshal(byteValue, &lastRun)
			if err != nil {
				slog.Error("Failed to unmarshal ", err)
				return
			}

			jsonFile.Close()
		}
	}

	knownIssuePath = strings.Join([]string{config.CacheRoot, "knownIssues"}, "/") + ".json"

	if !*ignoreCache {

		jsonFile, err := os.Open(knownIssuePath)

		if err != nil {
			slog.Warn("Failed to find or open file for known issues, assuming it hasn't been created yet")
		} else {

			byteValue, _ := io.ReadAll(jsonFile)

			err = json.Unmarshal(byteValue, &knownIssues)
			if err != nil {
				slog.Error("Failed to unmarshal ", err)
				return
			}

			jsonFile.Close()
		}
	}

	ctx := context.Background()
	errs, _ := errgroup.WithContext(ctx)

	log.SetOutput(f)

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

	if jiraApiCalls.Current() > 0 && !*calendar {
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
	if *calendar {
		err = WriteCalendar()
	}

	if err != nil {
		slog.Error("Failed in WriteCalendar", err)
	}

	////

	jsonBytes, err := json.MarshalIndent(knownIssues, "", "  ")
	if err != nil {
		slog.Error("Failed in json.Marshal")
		return
	}

	err = WriteFile(knownIssuePath, jsonBytes)
	if err != nil {
		slog.Error("Failed in write file " + knownIssuePath)
		return
	}

	////

	if !*calendar {
		jsonBytes, err = json.MarshalIndent(startTime, "", "  ")
		if err != nil {
			slog.Error("Failed in json.Marshal")
			return
		}

		err = WriteFile(lastRunPath, jsonBytes)
		if err != nil {
			slog.Error("Failed in write file " + lastRunPath)
			return
		}
	}

	////

	slog.Info("exiting")

}

func WritePage(title string, contents []byte) error {

	return WriteFile(path.Join(config.LogseqRoot, "pages", "jira", PageNameToFileName(title)+".md"), contents)

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
