package main

import (
	"context"
	"log"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/cloud"
)

type JiraConfig struct {
	BaseURL  string   `json:"base_url"`
	Username string   `json:"username"`
	APIToken string   `json:"api_token"`
	Projects []string `json:"projects"`
}

func (c JiraConfig) Process(wg *sync.WaitGroup) error {

	client, err := c.createClient()
	if err != nil {
		return err
	}

	for _, project := range c.Projects {

		err := ProcessProject(wg, c, client, project)
		if err != nil {
			return err
		}

	}

	return nil

}

func ProcessProject(wg *sync.WaitGroup, c JiraConfig, client *jira.Client, project string) error {

	log.Println("Processing Project: " + project)

	issues, err := GetIssues(client, "project = "+project)
	if err != nil {
		return err
	}

	for _, issue := range issues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = ProcessIssue(wg, c, client, &issue, project)
		}()
		if err != nil {
			return err
		}
	}

	return nil

}

func ProcessIssue(wg *sync.WaitGroup, c JiraConfig, client *jira.Client, issue *jira.Issue, project string) (err error) {

	var fetchedIssue *jira.Issue // Use GetIssue() on this to populate on first use, but reuse therafter
	// Like so:
	// fetchedIssue, err = GetIssue(client, issue, fetchedIssue)
	// if err!=nil{
	//   return nil
	// }

	if !config.Jira.IncludeDone && func() bool {
		for _, n := range config.Jira.DoneStatus {
			if issue.Fields.Status.Name == n {
				return true
			}
		}
		return false
	}() {
		return nil
	}

	log.Println("Processing Issue: " + issue.Key)

	output := []string{
		"alias:: " + issue.Key,
		"title:: " + issue.Key + " | " + SearchAndReplace(issue.Fields.Summary, []struct {
			matcher string
			repl    string
		}{
			{ // Replace slash with FULLWIDTH SOLIDUS to prevent hierarchy pages being made
				matcher: `/`,
				repl:    `／`,
			},
			{ // Replace [[text]] with (text)
				matcher: `\[\[ *([^\]]+) *\]\]`,
				repl:    `( $1 )`,
			},
		}),
		"type:: jira-ticket",
		"project:: " + project,
		"url:: " + c.BaseURL + "browse/" + issue.Key,
		"description:: " + issue.Fields.Summary,
		"status:: " + issue.Fields.Status.Name,
		"date_created:: [[" + DateFormat(time.Time(issue.Fields.Created)) + "]]",
		"date_created_sortable:: " + time.Time(issue.Fields.Created).Format("2006/01/02"),
	}

	if config.Jira.ExcludeFromGraph {
		output = append(output, "exclude-from-graph-view:: true")
	}

	if config.Jira.IncludeWatchers && issue.Fields.Watches != nil && issue.Fields.Watches.WatchCount > 0 {

		watchers := []string{}

		log.Println("Getting watchers for " + issue.Key)
		jiraApiCallCount += 1
		users, _, err := client.Issue.GetWatchers(context.Background(), issue.ID)
		if err != nil {
			return err
		}
		for _, u := range *users {
			watchers = append(watchers, "[["+u.DisplayName+"]]")
		}

		slices.Sort(watchers)

		output = append(output, "watchers:: "+strings.Join(watchers, ", "))
	}

	if time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1 {
		output = append(output,
			"date_due:: [["+DateFormat(time.Time(issue.Fields.Duedate))+"]]",
			"date_due_sortable:: "+time.Time(issue.Fields.Duedate).Format("2006/01/02"),
		)
	}

	if issue.Fields.Assignee != nil {
		output = append(output,
			"assignee:: [["+issue.Fields.Assignee.DisplayName+"]]",
		)
	}

	if issue.Fields.Reporter != nil {
		output = append(output,
			"reporter:: [["+issue.Fields.Reporter.DisplayName+"]]",
		)
	}

	output = append(output, ParseJiraText(issue.Fields.Description)...)

	if issue.Fields.IssueLinks != nil {
		links := map[string]([]string){}

		for _, link := range issue.Fields.IssueLinks {
			if link.OutwardIssue != nil {
				links[link.Type.Name] = append(links[link.Type.Name], link.OutwardIssue.Key)
			}
		}

		keys := make([]string, 0, len(links))
		for k := range links {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		for _, linkType := range keys {
			issues := links[linkType]
			output = append(output, "- # "+linkType)
			for _, issue := range issues {
				output = append(output, "    - [["+issue+"]]")
			}
		}

	}

	if time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1 {
		output = append(output,
			"- ***",
			"- "+func() string {
				switch issue.Fields.Status.Name {
				case "Done", "Past":
					return "DONE"
				}
				return "TODO"
			}()+" [[Jira Task]] [["+issue.Key+"]]",
			"  DEADLINE: <"+time.Time(issue.Fields.Duedate).Format("2006-01-02 Mon")+">",
			"  SCHEDULED: <"+time.Time(issue.Fields.Duedate).Format("2006-01-02 Mon")+">",
		)
	}

	if config.Jira.IncludeComments {
		fetchedIssue, err = GetIssue(client, issue, fetchedIssue)
		if err != nil {
			return err
		}
		if fetchedIssue.Fields.Comments != nil {
			output = append(output, "- ### Comments")
			for _, c := range fetchedIssue.Fields.Comments.Comments {
				output = append(output, "- [["+c.Author.DisplayName+"]] - Created: "+c.Created+" | Updated: "+c.Updated)
				output = append(output, PrefixStringSlice(ParseJiraText(c.Body), "  ")...)
				output = append(output, "***")
			}
		}
	}

	return WritePage(issue.Key, []byte(strings.Join(output, "\n")))

}

func (c JiraConfig) createClient() (*jira.Client, error) {
	tp := jira.BasicAuthTransport{
		Username: c.Username,
		APIToken: c.APIToken,
	}
	return jira.NewClient(c.BaseURL, tp.Client())
}

// https://github.com/andygrunwald/go-jira/issues/55#issuecomment-676631140
func GetIssues(client *jira.Client, searchString string) ([]jira.Issue, error) {
	last := 0
	var issues []jira.Issue = nil
	for {
		opt := &jira.SearchOptions{
			MaxResults: 100,
			StartAt:    last,
		}

		chunk, resp, err := client.Issue.Search(context.Background(), searchString, opt)
		if err != nil {
			return nil, err
		}

		total := resp.Total
		if issues == nil {
			issues = make([]jira.Issue, 0, total)
		}
		issues = append(issues, chunk...)
		last = resp.StartAt + len(chunk)
		if last >= total {
			break
		}
	}
	return issues, nil
}

func ParseJiraText(input string) []string {
	description := strings.Split(JiraToMD(input), "\n")
	descriptionFormatted := []string{""}

	for _, l := range description {
		if l == "" {
			continue
		}
		if regexp.MustCompile(`^[0-9]+\. `).MatchString(l) {
			l = regexp.MustCompile(`^[0-9]+\. `).ReplaceAllString(l, "")
			descriptionFormatted = append(descriptionFormatted, "- "+l, "  logseq.order-list-type:: number")
			continue
		}
		descriptionFormatted = append(descriptionFormatted, "- "+l)
	}
	return descriptionFormatted
}

func PrefixStringSlice(i []string, p string) (o []string) {
	for _, l := range i {
		o = append(o, p+l)
	}
	return
}

func GetIssue(client *jira.Client, sparseIssue *jira.Issue, fullIssueCheck *jira.Issue) (fullIssue *jira.Issue, err error) {
	if fullIssueCheck == nil {
		log.Println("Fetching specific info for " + sparseIssue.Key)
		jiraApiCallCount += 1
		fullIssue, _, err = client.Issue.Get(context.Background(), sparseIssue.Key, nil)
	} else {
		fullIssue = fullIssueCheck
	}
	return fullIssue, err
}
