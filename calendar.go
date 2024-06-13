package main

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"io"
	"log/slog"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/cloud"
	colorful "github.com/lucasb-eyer/go-colorful"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

var (
	events struct {
		mu   sync.Mutex
		data []Event
	}
	milestones struct {
		mu   sync.Mutex
		data []Milestone
	}
)

type CalendarEntry interface {
	MarkwhenLineItem() string
	GetTags() []string
	GetEarliestDate() time.Time
}

type Event struct {
	Title string

	Start time.Time
	End   time.Time

	Tags []string

	c     *JiraConfig
	issue *jira.Issue
}

type Milestone struct {
	Title string

	Date time.Time

	Tags []string

	c     *JiraConfig
	issue *jira.Issue
}

func (e Event) MarkwhenLineItem() string {
	format := "2006-01-02"
	return e.Start.Format(format) + " / " + e.End.Format(format) + ": [" + e.Title + "](" + e.c.Connection.BaseURL + "browse/" + e.issue.Key + ")"
}

func (e Event) GetTags() []string {
	return e.Tags
}

func (e Event) GetEarliestDate() time.Time {
	return e.Start
}

func (m Milestone) MarkwhenLineItem() string {
	format := "2006-01-02"
	return m.Date.Format(format) + ": [" + m.Title + "](" + m.c.Connection.BaseURL + "browse/" + m.issue.Key + ")"
}

func (m Milestone) GetTags() []string {
	return m.Tags
}

func (m Milestone) GetEarliestDate() time.Time {
	return m.Date
}

func ProcessCalendar(wg *errgroup.Group, c *JiraConfig, issue *jira.Issue, project string) (err error) {
	var fetchedIssue *jira.Issue

	fetchedIssue, err = GetIssue(c, issue, fetchedIssue)
	if err != nil {
		return errors.Wrap(err, "Failed in GetIssue")
	}

	if fetchedIssue.Fields.Comments != nil && len(fetchedIssue.Fields.Comments.Comments) > 0 {
		for _, comment := range fetchedIssue.Fields.Comments.Comments {
			lines := strings.Split(comment.Body, "\n")
			for _, line := range lines {
				matches := regexp.MustCompile(`\[[^\]]+\]`).FindAllString(line, -1)

				matchReal := []string{}

				for _, match := range matches {
					matchReal = append(matchReal, regexp.MustCompile(`\[([^\]]+)\]`).ReplaceAllString(match, `$1`))
				}
				for _, match := range matchReal {
					f, ok := tagFunc[match]
					if ok {
						f(c, fetchedIssue, matchReal, listTagFuncs())
					}
				}
			}
		}
	}

	c.progress[project].IncrBy(1)

	return

}

var tagFunc map[string]func(*JiraConfig, *jira.Issue, []string, []string) error = map[string]func(*JiraConfig, *jira.Issue, []string, []string) error{
	"Event": func(c *JiraConfig, issue *jira.Issue, matches []string, reservedTags []string) error {

		startTime, err := getStartDate(c, issue)
		if err != nil {
			slog.Warn("Cannot add as milestone based on missing start date", err)
		}

		if time.Time(issue.Fields.Duedate).Compare(time.Time{}) != 1 {
			slog.Warn("Cannot add as milestone based on missing due date")
		}

		events.mu.Lock()

		e := Event{
			Title: issue.Fields.Summary,
			Start: startTime,
			End:   time.Time(issue.Fields.Duedate),
			Tags:  filterTags(matches, reservedTags),
			c:     c,
			issue: issue,
		}

		events.data = append(events.data, e)
		events.mu.Unlock()
		return nil
	},
	"Milestone/Start": func(c *JiraConfig, issue *jira.Issue, matches []string, reservedTags []string) (err error) {

		startTime, err := getStartDate(c, issue)
		if err != nil {
			slog.Warn("Cannot add as milestone base on start date", err)
		}

		milestones.mu.Lock()
		milestones.data = append(milestones.data, Milestone{
			Title: issue.Fields.Summary,
			Date:  startTime,
			Tags:  filterTags(matches, reservedTags),
			c:     c,
			issue: issue,
		})
		milestones.mu.Unlock()
		return
	},
	"Milestone/End": func(c *JiraConfig, issue *jira.Issue, matches []string, reservedTags []string) error {

		if time.Time(issue.Fields.Duedate).Compare(time.Time{}) != 1 {
			slog.Warn("No due date for " + issue.Key + ", cannot add as milestone base on end date")
		}

		milestones.mu.Lock()
		milestones.data = append(milestones.data, Milestone{
			Title: issue.Fields.Summary,
			Date:  time.Time(issue.Fields.Duedate),
			Tags:  filterTags(matches, reservedTags),
			c:     c,
			issue: issue,
		})
		milestones.mu.Unlock()
		return nil
	},
}

func filterTags(inputTags []string, reservedTags []string) []string {
	out := []string{}
	for _, match := range inputTags {
		ok := false
		for _, tag := range reservedTags {
			if tag == match {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, match)
		}
	}
	return out
}

func listTagFuncs() (out []string) {
	for tag := range tagFunc {
		out = append(out, tag)
	}
	return
}

func WriteCalendar() error {

	items := []CalendarEntry{}

	for _, e := range events.data {
		items = append(items, e)
	}
	for _, m := range milestones.data {
		items = append(items, m)
	}

	tags := map[string][]*CalendarEntry{}

	for _, item := range items {
		if calendarLookaheadTime == nil || item.GetEarliestDate().Before(*calendarLookaheadTime) {
			for _, tag := range item.GetTags() {
				tags[tag] = append(tags[tag], &item)
			}
		}
	}

	output := []string{
		"---",
		"title: WMS Calendar",
		`view:`,
		`- \*`,
		`ranges:`,
		`- 6 month`,
		`- 3 month`,
		`- 1 month`,
	}

	for tag := range tags {
		h := md5.New()
		io.WriteString(h, tag)
		r := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(h.Sum(nil)))))
		output = append(output, "#"+tagName(tag)+": "+colorful.Hcl(
			r.Float64()*360.0,
			0.5+r.Float64()*0.3,
			0.5+r.Float64()*0.3).Hex())

	}

	output = append(output, "---", "")

	for tag := range tags {
		output = append(output, "section "+tag+" #"+tagName(tag))

		section := []string{}

		for _, item := range tags[tag] {
			section = append(section, (*item).MarkwhenLineItem()+" #"+tagName(tag))
		}

		slices.Sort(section)

		output = append(output, section...)
		output = append(output, "endSection")
	}

	slog.Info("test")

	return WriteFile(*calendarPath, []byte(strings.Join(output, "\n")))

}

func getStartDate(c *JiraConfig, issue *jira.Issue) (t time.Time, err error) {

	customFields, _, err := c.client.Issue.GetCustomFields(context.Background(), issue.ID)
	if err != nil {
		return
	}

	start, ok := customFields[c.StartDateField]
	if start == "" || !ok {
		return time.Time{}, errors.New("No start date defined for " + issue.Key)
	}

	t, err = time.Parse("2006-01-02", string(start))
	if err != nil {
		return time.Time{}, err
	}
	return
}

func tagName(tag string) string {
	return strings.ReplaceAll(tag, " ", "_")
}
