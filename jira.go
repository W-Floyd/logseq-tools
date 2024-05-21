package main

import (
	"context"
	"errors"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/cloud"
)

type JiraConfig struct {
	BaseURL  string   `json:"base_url"`
	Username string   `json:"username"`
	APIToken string   `json:"api_token"`
	Projects []string `json:"projects"`
}

func (c JiraConfig) Process() error {

	client, err := c.createClient()
	if err != nil {
		return err
	}

	for _, project := range c.Projects {

		err := ProcessProject(c, client, project)
		if err != nil {
			return err
		}

	}

	return nil

}

func ProcessProject(c JiraConfig, client *jira.Client, project string) error {

	log.Println("Processing Project: " + project)

	issues, err := GetIssues(client, "project = "+project)
	if err != nil {
		return err
	}

	for _, issue := range issues {
		err := ProcessIssue(c, issue)
		if err != nil {
			return err
		}
	}

	return nil

}

func ProcessIssue(c JiraConfig, issue jira.Issue) error {

	log.Println("Processing Issue: " + issue.Key)

	output := []string{
		"alias:: " + issue.Key,
		"title:: " + issue.Key + " | " + issue.Fields.Summary,
		"exclude-from-graph-view:: true",
		"type:: jira-ticket",
		"url:: " + c.BaseURL + "browse/" + issue.Key,
		"description:: " + issue.Fields.Summary,
		"status:: " + issue.Fields.Status.Name,
		"date_created:: [[" + DateFormat(time.Time(issue.Fields.Created)) + "]]",
		"date_created_sortable:: " + time.Time(issue.Fields.Created).Format("2006/01/02"),
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

	output = append(output,
		"",
		"- "+func(input string) string {
			output := input
			output = regexp.MustCompile("\n#").ReplaceAllString(output, "\n    - ")
			output = regexp.MustCompile("\n\n").ReplaceAllString(output, "\n")
			return output
		}(issue.Fields.Description),
	)

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
			"  DEADLINE: <"+time.Time(issue.Fields.Duedate).Format("2006-01-01 Mon")+">",
			"  SCHEDULED: <"+time.Time(issue.Fields.Duedate).Format("2006-01-01 Mon")+">",
		)
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

func findProject(client *jira.Client, projectName string) (*jira.Project, error) {
	p, _, err := client.Project.GetAll(context.Background(), &jira.GetQueryOptions{})
	if err != nil {
		return nil, err
	}

	for _, pro := range *p {
		if pro.Name == projectName {
			project, _, err := client.Project.Get(context.Background(), pro.ID)
			return project, err
		}
	}

	return nil, errors.New("Cannot find specified project " + projectName)
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
