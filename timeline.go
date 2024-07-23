package main

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"io"
	"log"
	"log/slog"
	"math/rand"
	"regexp"
	"slices"
	"sort"
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
	groups map[string]*GroupTree = map[string]*GroupTree{}
)

type TimelineEntry interface {
	MarkwhenLineItem() string
	GetTags() []string
	GetEarliestDate() time.Time
	GetIssue() *jira.Issue
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

type GroupTree struct {
	Name        string
	Entries     []*TimelineEntry
	ChildGroups []*GroupTree
	parent      *GroupTree
}

func GetGroupTree(tag string) *GroupTree {
	if _, ok := groups[tag]; ok {
		return groups[tag]
	}

	builtPath := ""
	for _, segment := range strings.Split(tag, "/") {

		newBuiltPath := strings.TrimPrefix(builtPath+"/"+segment, "/")

		g := &GroupTree{
			Name: segment,
		}

		if builtPath != "" {
			p := GetGroupTree(builtPath)
			if _, ok := groups[newBuiltPath]; !ok {
				p.AddChild(g)
			}
		} else {
			if _, ok := groups[segment]; !ok {
				groups[segment] = g
			}
		}

		builtPath = newBuiltPath

	}

	return GetGroupTree(tag)

}

func (g *GroupTree) AddChild(c *GroupTree) *GroupTree {
	g.AddToMap()
	g.ChildGroups = append(g.ChildGroups, c)
	c.parent = g
	c.AddToMap()
	return g
}

func (g *GroupTree) AddToMap() *GroupTree {
	groups[g.GetTag()] = g
	return g
}

func (g *GroupTree) GetTag() string {
	iterationCount := 0
	parent := g.parent
	path := g.Name
	for {
		iterationCount += 1
		if iterationCount > 1000 {
			log.Fatalln("Loop detected in grouping")
		}
		if parent == nil {
			break
		}
		path = parent.Name + "/" + path
		parent = parent.parent

	}
	return path
}

func (e Event) MarkwhenLineItem() string {
	format := "2006-01-02"
	return e.Start.Format(format) + " / " + e.End.Format(format) + ": [" + e.Title + "](" + *e.c.Connection.BaseURL + "browse/" + e.issue.Key + ")"
}

func (e Event) GetTags() []string {
	return e.Tags
}

func (e Event) GetEarliestDate() time.Time {
	return e.Start
}

func (e Event) GetIssue() *jira.Issue {
	return e.issue
}

func (m Milestone) GetIssue() *jira.Issue {
	return m.issue
}

func (m Milestone) MarkwhenLineItem() string {
	format := "2006-01-02"
	return m.Date.Format(format) + ": [" + m.Title + "](" + *m.c.Connection.BaseURL + "browse/" + m.issue.Key + ")"
}

func (m Milestone) GetTags() []string {
	return m.Tags
}

func (m Milestone) GetEarliestDate() time.Time {
	return m.Date
}

func ProcessTimeline(wg *errgroup.Group, issue *jira.Issue, project *JiraProject) (err error) {
	var fetchedIssue *jira.Issue

	c := project.config

	fetchedIssue, err = GetIssue(project, issue, fetchedIssue)
	if err != nil {
		return errors.Wrap(err, "Failed in GetIssue")
	}

	if fetchedIssue.Fields.Comments != nil && len(fetchedIssue.Fields.Comments.Comments) > 0 {
		for _, comment := range fetchedIssue.Fields.Comments.Comments {
			lines := strings.Split(comment.Body, "\n")
			// hasTag := false
			for _, line := range lines {
				if !strings.Contains(line, "ExtractTag") {
					continue
				}
				// hasTag = true

				matches := regexp.MustCompile(`\[[^\]]+\]`).FindAllString(line, -1)

				matchReal := []string{}

				for _, match := range matches {
					matchReal = append(matchReal, regexp.MustCompile(`\[([^\]]+)\]`).ReplaceAllString(match, `$1`))
				}

				for _, match := range matchReal {
					f, ok := tagFunc[match]
					if ok {
						f(project, fetchedIssue, matchReal, listTagFuncs())
					}
				}
			}
			// if hasTag && issue.Fields.Assignee != nil {
			// 	tagFunc["Event"](c, fetchedIssue, []string{"Person/" + issue.Fields.Assignee.DisplayName}, listTagFuncs())
			// }
		}
	}

	c.progress[*project.Key].IncrBy(1)

	return

}

var tagFunc map[string]func(*JiraProject, *jira.Issue, []string, []string) error = map[string]func(*JiraProject, *jira.Issue, []string, []string) error{
	"Event": func(project *JiraProject, issue *jira.Issue, matches []string, reservedTags []string) error {

		startTime, err := getStartDate(project, issue)
		if err != nil {
			slog.Warn("Cannot add " + issue.Key + " as event based on missing start date - " + err.Error())
			return nil
		}

		if time.Time(issue.Fields.Duedate).Compare(time.Time{}) != 1 {
			slog.Warn("Cannot add " + issue.Key + " as event based on missing due date")
			return nil
		}

		events.mu.Lock()

		e := Event{
			Title: issue.Fields.Summary,
			Start: startTime,
			End:   time.Time(issue.Fields.Duedate),
			Tags:  filterTags(matches, reservedTags),
			c:     project.config,
			issue: issue,
		}

		events.data = append(events.data, e)
		events.mu.Unlock()
		return nil
	},
	"Milestone/Start": func(project *JiraProject, issue *jira.Issue, matches []string, reservedTags []string) (err error) {

		startTime, err := getStartDate(project, issue)
		if err != nil {
			slog.Warn("Cannot add " + issue.Key + " as milestone base on start date - " + err.Error())
		}

		milestones.mu.Lock()
		milestones.data = append(milestones.data, Milestone{
			Title: issue.Fields.Summary,
			Date:  startTime,
			Tags:  filterTags(matches, reservedTags),
			c:     project.config,
			issue: issue,
		})
		milestones.mu.Unlock()
		return
	},
	"Milestone/End": func(project *JiraProject, issue *jira.Issue, matches []string, reservedTags []string) error {

		if time.Time(issue.Fields.Duedate).Compare(time.Time{}) != 1 {
			slog.Warn("No due date for " + issue.Key + ", cannot add as milestone base on end date")
		}

		milestones.mu.Lock()
		milestones.data = append(milestones.data, Milestone{
			Title: issue.Fields.Summary,
			Date:  time.Time(issue.Fields.Duedate),
			Tags:  filterTags(matches, reservedTags),
			c:     project.config,
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

func WriteTimeline() error {

	items := []TimelineEntry{}

	for _, e := range events.data {
		items = append(items, e)
	}
	for _, m := range milestones.data {
		items = append(items, m)
	}

	tags := map[string][]*TimelineEntry{}

	for _, item := range items {
		if timelineLookaheadTime == nil || item.GetEarliestDate().Before(*timelineLookaheadTime) {
			for _, tag := range item.GetTags() {
				tags[tag] = append(tags[tag], &item)
			}
		}
	}

	output := []string{
		"---",
		"title: WMS Timeline",
		`view:`,
		`- \*`,
		`ranges:`,
		`- 6 month`,
		`- 3 month`,
		`- 1 month`,
	}

	colors := []string{}

	tagsToColor := []string{}

	for tag := range tags {
		tagsToColor = append(tagsToColor, tag)
	}

	people := map[string]string{}

	for _, entry := range items {
		if entry.GetIssue().Fields.Assignee != nil {
			people[entry.GetIssue().Fields.Assignee.DisplayName] = tagName("Person/Assignee/" + entry.GetIssue().Fields.Assignee.DisplayName)
		}
	}

	for _, person := range people {
		tagsToColor = append(tagsToColor, person)
	}

	for _, tag := range tagsToColor {
		h := md5.New()
		io.WriteString(h, tag)
		r := rand.New(rand.NewSource(int64(binary.BigEndian.Uint64(h.Sum(nil)))))
		colors = append(colors, "#"+tagName(tag)+": "+colorful.Hcl(
			r.Float64()*360.0,
			0.5+r.Float64()*0.3,
			0.5+r.Float64()*0.3).Hex())
	}

	slices.Sort(colors)

	output = append(output, colors...)

	output = append(output, "---", "")

	for tag, entries := range tags {
		g := GetGroupTree(tag)
		g.Entries = append(g.Entries, entries...)
	}

	sections := [][]string{}

	for _, g := range groups {
		if g.parent == nil {
			sections = append(sections, g.Print())
		}
	}

	sort.SliceStable(sections, sortSliceOfSlices(sections))

	for _, section := range sections {
		output = append(output, section...)
	}

	return WriteFile(*timelinePath, []byte(strings.Join(output, "\n")))

}

func (g *GroupTree) Print() (output []string) {

	header := "group"

	if g.parent == nil {
		header = "section"
	}

	output = append(output, header+" "+g.Name+" #"+tagName(g.GetTag()))

	sections := [][]string{}
	entries := []string{}

	for _, c := range g.ChildGroups {
		sections = append(sections, c.Print())
	}

	for _, item := range g.Entries {
		line := (*item).MarkwhenLineItem() + " #" + tagName(g.GetTag())
		if (*item).GetIssue().Fields.Assignee != nil {
			line += " " + tagName("#Person/Assignee/"+(*item).GetIssue().Fields.Assignee.DisplayName)
		}
		entries = append(entries, line)
	}

	slices.Sort(entries)
	sort.SliceStable(sections, sortSliceOfSlices(sections))

	tail := "endGroup"

	if g.parent == nil {
		tail = "endSection"
	}

	for _, s := range sections {
		output = append(output, s...)
	}
	output = append(output, entries...)
	output = append(output, tail)

	return
}

func getStartDate(project *JiraProject, issue *jira.Issue) (t time.Time, err error) {

	c := project.config

	customFields, _, err := c.client.Issue.GetCustomFields(context.Background(), issue.ID)
	if err != nil {
		return
	}

	start := ""

	for _, customField := range project.Options.CustomFields {
		if *customField.To == "date_start" {
			start = customFields[*customField.From]
		}
	}

	if start == "" {
		return time.Time{}, errors.New("No start date defined for " + issue.Key)
	}

	t, err = time.Parse("2006-01-02", string(start))
	if err != nil {
		return time.Time{}, err
	}
	return
}

func tagName(tag string) string {
	replacePairs := [][2]string{
		{
			" ", "_",
		},
		{
			"/", "_",
		},
	}
	for _, pair := range replacePairs {
		tag = strings.ReplaceAll(tag, pair[0], pair[1])
	}
	return tag
}

func sortSliceOfSlices(sections [][]string) func(i, j int) bool {
	return func(i, j int) bool {
		// edge cases
		if len(sections[i]) == 0 && len(sections[j]) == 0 {
			return false // two empty slices - so one is not less than other i.e. false
		}
		if len(sections[i]) == 0 || len(sections[j]) == 0 {
			return len(sections[i]) == 0 // empty slice listed "first" (change to != 0 to put them last)
		}

		// both slices len() > 0, so can test this now:
		return sections[i][0] < sections[j][0]
	}

}
