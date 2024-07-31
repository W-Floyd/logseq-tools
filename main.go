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

	"dario.cat/mergo"
	jira "github.com/andygrunwald/go-jira/v2/cloud"
	"github.com/fatih/color"
	"github.com/pkg/errors"
	"github.com/tj/go-naturaldate"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/errgroup"
)

type Config struct {
	Jira struct {
		Instances []*JiraConfig `json:"instances"` // Jira instances to process
		Users     []struct {
			AccountID   string `json:"account_id"`   // Account ID to match
			DisplayName string `json:"display_name"` // Display name to print in place
		} `json:"users"`
		Options JiraOptions `json:"options"`
	} `json:"jira"`
	Calendar struct {
		Instances []*CalendarConfig `json:"instances"` // Calendar instances to process
	}
}

var (
	config                      = Config{}
	jiraApiCalls, jiraCacheHits *mpb.Bar
	progress                    *mpb.Progress
	timeline                    *bool
	timelinePath                *string
	logFile                     *string
	timelineLookahead           *string
	timelineLookaheadTime       *time.Time
	debug                       *bool
	verbose                     *bool
	recent                      *bool
	ignoreCache                 *bool
	startTime                   = time.Now()
	lastRun                     *time.Time
	lastRunPath                 string
	knownIssues                 = map[string]*jira.Issue{}
	knownIssuePath              string
	issueUrlMatchers            = []*regexp.Regexp{}
	defaultOptions              = struct {
		Jira JiraOptions `json:"jira"`
	}{}
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
	defaultOptionsFile := flag.String("default-options-file", "./default_options.json", "Default options to use")

	timeline = flag.Bool("timeline", false, "Whether to just parse timeline tags (into Markwhen)")
	timelinePath = flag.String("timeline-path", "./timeline.mw", "Where to parse the timeline to")
	timelineLookahead = flag.String("timeline-lookahead", "", "How far to look ahead")
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
		slog.Error("error opening file: " + err.Error())
		return
	}
	defer f.Close()

	if *ignoreCache && *recent {
		slog.Error("Cannot look for recent only without cache")
		return
	}

	if *timeline && timelineLookahead != nil && *timelineLookahead != "" {
		now := time.Now()
		t, err := naturaldate.Parse(*timelineLookahead, now)
		if err != nil {
			slog.Error(err.Error())
		}
		if t == now {
			slog.Error("Unrecognized natural date string")
		}
		timelineLookaheadTime = &t
	}

	configRaw, err := os.ReadFile(*configFile)
	if err != nil {
		slog.Error(err.Error())
		return
	}

	err = json.Unmarshal(configRaw, &config)
	if err != nil {
		if ute, ok := err.(*json.UnmarshalTypeError); ok {
			fmt.Printf("UnmarshalTypeError %v - %v - %v\n", ute.Value, ute.Type, ute.Offset)
		} else {
			fmt.Println("Other error:", err)
		}
		slog.Error(err.Error())
		return
	}

	optionsRaw, err := os.ReadFile(*defaultOptionsFile)
	if err != nil {
		slog.Error(err.Error())
		return
	}

	err = json.Unmarshal(optionsRaw, &defaultOptions)
	if err != nil {
		if ute, ok := err.(*json.UnmarshalTypeError); ok {
			fmt.Printf("UnmarshalTypeError %v - %v - %v\n", ute.Value, ute.Type, ute.Offset)
		} else {
			fmt.Println("Other error:", err)
		}
		slog.Error(err.Error())
		return
	}

	err = mergo.Merge(&config.Jira.Options, defaultOptions.Jira, mergo.WithAppendSlice, mergo.WithOverrideEmptySlice, mergo.WithSliceDeepCopy)
	if err != nil {
		slog.Error(err.Error())
		return
	}

	lastRunPath = strings.Join([]string{config.Jira.Options.Paths.CacheRoot, "lastRun"}, "/") + ".json"

	if *recent {

		jsonFile, err := os.Open(lastRunPath)

		if err != nil {
			slog.Error("Failed to find or open file for last run timing, running as if you didn't specify -recent")
			*recent = false
		} else {

			byteValue, _ := io.ReadAll(jsonFile)

			err = json.Unmarshal(byteValue, &lastRun)
			if err != nil {
				slog.Error("Failed to unmarshal: " + err.Error())
				return
			}

			jsonFile.Close()
		}
	}

	knownIssuePath = strings.Join([]string{config.Jira.Options.Paths.CacheRoot, "knownIssues"}, "/") + ".json"

	if !*ignoreCache {

		jsonFile, err := os.Open(knownIssuePath)

		if err != nil {
			slog.Warn("Failed to find or open file for known issues, assuming it hasn't been created yet")
		} else {

			byteValue, _ := io.ReadAll(jsonFile)

			err = json.Unmarshal(byteValue, &knownIssues)
			if err != nil {
				slog.Error("Failed to unmarshal: " + err.Error())
				return
			}

			jsonFile.Close()
		}
	}

	ctx := context.Background()
	errs, _ := errgroup.WithContext(ctx)

	log.SetOutput(f)

	// Issue links
	for _, instance := range config.Jira.Instances {
		for _, project := range instance.Projects {
			urlPattern := regexp.QuoteMeta(*instance.Connection.BaseURL) + `browse/(` + regexp.QuoteMeta(*project.Key) + `-[0-9]+)`
			matcher := ""
			for _, pair := range [][2]string{{`[`, `]`}, {`(`, `)`}} {
				start := regexp.QuoteMeta(pair[0])
				end := regexp.QuoteMeta(pair[1])

				matcher = matcher + start + urlPattern + `(` + end + `|[^` + end + `]+` + end + `)`
			}
			issueUrlMatchers = append(issueUrlMatchers, regexp.MustCompile(matcher))
		}
	}

	for _, instance := range config.Jira.Instances {
		instance := instance
		errs.Go(
			func() error {
				return instance.Process(errs)
			},
		)
	}

	for _, instance := range config.Calendar.Instances {
		instance := instance
		errs.Go(
			func() error {
				return instance.Process(errs)
			},
		)
	}

	err = errs.Wait()

	if jiraApiCalls.Current() > 0 && !*timeline {
		err = WriteIssueMap()
		if err != nil {
			slog.Error("Failed in WriteIssueMap: " + err.Error())
			return
		}
		slog.Info("Jira API calls: " + strconv.Itoa(int(jiraApiCalls.Current())))
	}

	if err != nil {
		if err, ok := err.(stackTracer); ok {
			for _, f := range err.StackTrace() {
				fmt.Printf("%+s:%d\n", f, f)
			}
		}
		slog.Error("Failed in instance.Process: " + err.Error())
		return
	}
	if *timeline {
		err = WriteTimeline()
	}

	if err != nil {
		slog.Error("Failed in WriteTimeline: " + err.Error())
		return
	}

	////

	err = config.ProcessTables()
	if err != nil {
		slog.Error("Failed in ProcessTables: " + err.Error())
		return
	}

	////

	for _, i := range knownIssues {
		i.Fields.Unknowns = nil
		for _, si := range i.Fields.Subtasks {
			si.Fields.Unknowns = nil
		}
		for _, l := range i.Fields.IssueLinks {
			if l.OutwardIssue != nil {
				l.OutwardIssue.Fields.Unknowns = nil
			}
			if l.InwardIssue != nil {
				l.InwardIssue.Fields.Unknowns = nil
			}
		}
	}

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

	if !*timeline {
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

	return WriteFile(path.Join(config.Jira.Options.Paths.LogseqRoot, "pages", "jira", PageNameToFileName(title)+".md"), contents)

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
