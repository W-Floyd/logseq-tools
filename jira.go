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
	Connection struct {
		BaseURL  string `json:"base_url"`
		Username string `json:"username"`
		APIToken string `json:"api_token"`
	} `json:"connection"`
	Projects         []string `json:"projects"`           // XXXXX string identifier of projects to process
	Enabled          bool     `json:"enabled"`            // Whether to process this Jira instance
	IncludeWatchers  bool     `json:"include_watchers"`   // This can be slow, so you may want to disable it
	IncludeComments  bool     `json:"include_comments"`   // This can be slow, so you may want to disable it
	ExcludeFromGraph bool     `json:"exclude_from_graph"` // If you have a lot of these, it can easily pollute your graph
	IncludeDone      bool     `json:"include_done"`       // Whether to include done items to help clean up the list
	DoneStatus       []string `json:"done_status"`        // Names to consider as done
	LinkNames        bool     `json:"link_names"`         // Whether to [[link]] names

	// TODO - Implement
	// IncludeURL       bool         `json:"include_url"`        // Whether to include the URL in the page name to disambiguate instances

	apiLimited *sync.Mutex  // Lock this to prevent calls while API cools down, unlock once done
	client     *jira.Client // Client to use for communication
}

func (c *JiraConfig) Process(wg *sync.WaitGroup) (err error) {

	c.apiLimited = &sync.Mutex{}

	c.client, err = c.createClient()
	if err != nil {
		return err
	}

	for _, project := range c.Projects {

		err = ProcessProject(wg, c, project)
		if err != nil {
			return err
		}

	}

	return nil

}

func ProcessProject(wg *sync.WaitGroup, c *JiraConfig, project string) error {

	log.Println("Processing Project: " + project)

	issues, err := GetIssues(c, "project = "+project)
	if err != nil {
		return err
	}

	for _, issue := range issues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = ProcessIssue(wg, c, &issue, project)
		}()
		if err != nil {
			return err
		}
	}

	return nil

}

func ProcessIssue(wg *sync.WaitGroup, c *JiraConfig, issue *jira.Issue, project string) (err error) {

	var fetchedIssue *jira.Issue // Use GetIssue() on this to populate on first use, but reuse thereafter
	// Like so:
	// fetchedIssue, err = GetIssue(client, issue, fetchedIssue)
	// if err!=nil{
	//   return nil
	// }

	if !c.IncludeDone && func() bool {
		for _, n := range c.DoneStatus {
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
		"url:: " + c.Connection.BaseURL + "browse/" + issue.Key,
		"description:: " + issue.Fields.Summary,
		"status:: " + issue.Fields.Status.Name,
		"date_created:: [[" + DateFormat(time.Time(issue.Fields.Created)) + "]]",
		"date_created_sortable:: " + time.Time(issue.Fields.Created).Format("2006/01/02"),
	}

	if c.ExcludeFromGraph {
		output = append(output, "exclude-from-graph-view:: true")
	}

	if c.IncludeWatchers && issue.Fields.Watches != nil && issue.Fields.Watches.WatchCount > 0 {

		watchers := []string{}

		log.Println("Getting watchers for " + issue.Key)
		c.apiLimited.Lock()
		c.apiLimited.Unlock() //lint:ignore SA2001 as we've only checked so we can make our API call - still rick of race condition, but lessened
		var users *[]jira.User
		var resp *jira.Response
		for {
			retry := false
			users, resp, err = c.client.Issue.GetWatchers(context.Background(), issue.ID)
			if err != nil {
				retry, err = CheckAPILimit(c, resp)
				if err != nil {
					return err
				}
			}
			if !retry {
				break
			}
		}
		jiraApiCallCount += 1
		for _, u := range *users {
			nameText := u.DisplayName
			if c.LinkNames {
				nameText = "[[" + nameText + "]]"
			}
			watchers = append(watchers, nameText)
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
		nameText := issue.Fields.Assignee.DisplayName
		if c.LinkNames {
			nameText = "[[" + nameText + "]]"
		}
		output = append(output, "assignee:: "+nameText)
	}

	if issue.Fields.Reporter != nil {
		nameText := issue.Fields.Reporter.DisplayName
		if c.LinkNames {
			nameText = "[[" + nameText + "]]"
		}
		output = append(output, "reporter:: "+nameText)
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

	if c.IncludeComments {
		fetchedIssue, err = GetIssue(c, issue, fetchedIssue)
		if err != nil {
			return err
		}
		if fetchedIssue.Fields.Comments != nil {
			output = append(output, "- ### Comments")
			for _, comment := range fetchedIssue.Fields.Comments.Comments {
				nameText := comment.Author.DisplayName
				if c.LinkNames {
					nameText = "[[" + nameText + "]]"
				}
				output = append(output, "- "+nameText+" - Created: "+comment.Created+" | Updated: "+comment.Updated)
				output = append(output, PrefixStringSlice(ParseJiraText(comment.Body), "  ")...)
				output = append(output, "***")
			}
		}
	}

	return WritePage(issue.Key, []byte(strings.Join(output, "\n")))

}

func (c *JiraConfig) createClient() (*jira.Client, error) {
	tp := jira.BasicAuthTransport{
		Username: c.Connection.Username,
		APIToken: c.Connection.APIToken,
	}
	return jira.NewClient(c.Connection.BaseURL, tp.Client())
}

// Modified from https://github.com/andygrunwald/go-jira/issues/55#issuecomment-676631140
func GetIssues(c *JiraConfig, searchString string) (issues []jira.Issue, err error) {
	last := 0
	for {
		opt := &jira.SearchOptions{
			MaxResults: 100,
			StartAt:    last,
		}

		var resp *jira.Response
		var chunk []jira.Issue
		c.apiLimited.Lock()
		c.apiLimited.Unlock() //lint:ignore SA2001 as we've only checked so we can make our API call - still rick of race condition, but lessened

		for {
			log.Println("Getting chunk")
			retry := false
			chunk, resp, err = c.client.Issue.Search(context.Background(), searchString, opt)
			if err != nil {
				retry, err = CheckAPILimit(c, resp)
				if err != nil {
					return nil, err
				}
			}
			if !retry {
				break
			}
		}

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

func GetIssue(c *JiraConfig, sparseIssue *jira.Issue, fullIssueCheck *jira.Issue) (fullIssue *jira.Issue, err error) {
	if fullIssueCheck == nil {
		log.Println("Fetching specific info for " + sparseIssue.Key)
		c.apiLimited.Lock()
		c.apiLimited.Unlock() //lint:ignore SA2001 as we've only checked so we can make our API call - still rick of race condition, but lessened

		var resp *jira.Response
		for {
			retry := false
			fullIssue, resp, err = c.client.Issue.Get(context.Background(), sparseIssue.Key, nil)
			if err != nil {
				retry, err = CheckAPILimit(c, resp)
				if err != nil {
					return nil, err
				}
			}
			if !retry {
				break
			}
		}

		if err != nil {
			return nil, err
		}
		jiraApiCallCount += 1
	} else {
		fullIssue = fullIssueCheck
	}
	return fullIssue, err
}

func CheckAPILimit(c *JiraConfig, resp *jira.Response) (retry bool, err error) {
	if resp.StatusCode == 429 {
		retry = true
		c.apiLimited.Lock()
		resetTime, err := time.Parse("2006-01-02T15:04Z", resp.Response.Header.Get("X-Ratelimit-Reset"))
		if err != nil {
			return false, err
		}
		resetTime = resetTime.Add(time.Second * 1) // Add one second buffer just in case
		log.Println("API calls exhausted, sleeping until ", resetTime)
		time.Sleep(time.Until(resetTime))
		log.Println("Waking up, API should be usable again, retrying last call.")
		c.apiLimited.Unlock()
	} else {
		retry = false
	}

	return
}
