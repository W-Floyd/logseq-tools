package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"github.com/MagicalTux/natsort"
	jira "github.com/andygrunwald/go-jira/v2/cloud"
	"github.com/pkg/errors"
	"github.com/segmentio/fasthash/fnv1a"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/sync/errgroup"
)

type JiraConfig struct {
	Connection struct {
		BaseURL     string `json:"base_url"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		APIToken    string `json:"api_token"`
		Parallel    *int   `json:"parallel"`
	} `json:"connection"`
	Projects         []string `json:"projects"`           // XXXXX string identifier of projects to process
	Enabled          bool     `json:"enabled"`            // Whether to process this Jira instance
	IncludeWatchers  bool     `json:"include_watchers"`   // This can be slow, so you may want to disable it
	IncludeComments  bool     `json:"include_comments"`   // This can be slow, so you may want to disable it
	ExcludeFromGraph bool     `json:"exclude_from_graph"` // If you have a lot of these, it can easily pollute your graph
	IncludeDone      bool     `json:"include_done"`       // Whether to include done items to help clean up the list
	IncludeTask      bool     `json:"include_task"`       // Whether to include a task on each item with a due date
	IncludeMyTasks   bool     `json:"include_my_tasks"`   // Whether to include a my tasks in all cases
	StartDateField   string   `json:"start_date_field"`   // Custom field to use for start dates
	Status           struct {
		Done []string `json:"done"` // Names to consider as done
	} `json:"status"`
	LinkNames   bool `json:"link_names"`   // Whether to [[link]] names
	SearchUsers bool `json:"search_users"` // Whether to search users - may not be possible due to permissions

	Actions struct {
	} `json:"actions"`

	// TODO - Implement
	// IncludeURL       bool         `json:"include_url"`        // Whether to include the URL in the page name to disambiguate instances

	apiLimited *sync.Mutex  // Lock this to prevent calls while API cools down, unlock once done
	client     *jira.Client // Client to use for communication
	progress   map[string]*mpb.Bar
}

var (
	issuesStore []*jira.Issue          = []*jira.Issue{}
	users       map[string]string      = map[string]string{}
	usersLock   *sync.Mutex            = &sync.Mutex{}
	issues      map[string]*jira.Issue = map[string]*jira.Issue{}
	parents     map[string]*string     = map[string]*string{}
	children    map[string][]string    = map[string][]string{}
)

func (c *JiraConfig) Process(wg *errgroup.Group) (err error) {

	c.apiLimited = &sync.Mutex{}

	c.client, err = c.createClient()
	if err != nil {
		return errors.Wrap(err, "Couldn't create a client")
	}

	c.progress = make(map[string]*mpb.Bar)

	for _, project := range c.Projects {

		pbar := progress.AddBar(0,
			mpb.PrependDecorators(
				decor.Name(project, decor.WC{C: decor.DindentRight | decor.DextraSpace}),
				decor.Name("processing", decor.WCSyncSpaceR),
				decor.CountersNoUnit("%d / %d", decor.WCSyncWidth),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.Percentage(decor.WC{W: 5}), "done"),
			),
		)

		c.progress[project] = pbar

	}

	for _, project := range c.Projects {

		err = ProcessProject(wg, c, project)
		if err != nil {
			return errors.Wrap(err, "Failed processing project "+project)
		}
	}

	return nil

}

func ProcessProject(wg *errgroup.Group, c *JiraConfig, project string) error {

	errs, _ := errgroup.WithContext(context.Background())
	if c.Connection.Parallel != nil {
		errs.SetLimit(*c.Connection.Parallel)
	} else {
		errs.SetLimit(4)
	}

	slog.Info("Processing Project: " + project)

	issues := make(chan jira.Issue)

	query := "project = " + project

	if *recent {
		query += " AND updated >= " + lastRun.Add(time.Second*-30).Format(`"2006/01/02 15:04"`)
	}

	if *calendar {
		query += ` AND comment ~ 'ExtractTag'`
	}

	slog.Info("Query: " + query)

	go func() error {
		return GetIssues(c, query, project, issues)
	}()

	c.progress[project].SetTotal(int64(len(issues)), false)

	for issue := range issues {
		issue := issue
		if *calendar {
			errs.Go(func() error {
				err := ProcessCalendar(wg, c, &issue, project)
				return errors.Wrap(err, "Failed to ProcessCalendar "+issue.Key)
			})
		} else {
			errs.Go(func() error {
				err := ProcessIssue(wg, c, &issue, project)
				return errors.Wrap(err, "Failed to ProcessIssue "+issue.Key)
			})
		}
	}

	return errors.Wrap(errs.Wait(), "Goroutine failed from ProcessProject")

}

func ProcessIssue(wg *errgroup.Group, c *JiraConfig, issue *jira.Issue, project string) (err error) {

	var fetchedIssue *jira.Issue // Use GetIssue() on this to populate on first use, and reuse thereafter
	watchers := &[]string{}      // use GetWatchers() on this to populate on first use, and reuse thereafter
	// Like so:
	// fetchedIssue, err = GetIssue(c, issue, fetchedIssue)
	// if err!=nil{
	//   return nil
	// }

	if !c.IncludeDone && func() bool {
		for _, n := range c.Status.Done {
			if issue.Fields.Status.Name == n {
				return true
			}
		}
		return false
	}() {
		return nil
	}

	slog.Info("Processing Issue: " + issue.Key)

	output := []string{
		"alias:: " + issue.Key,
		"title:: " + LogseqTitle(issue),
		"type:: jira-ticket",
		"jira-type:: " + issue.Fields.Type.Description,
		"jira-project:: " + project,
		"url:: " + c.Connection.BaseURL + "browse/" + issue.Key,
		"description:: " + LogseqTransform(issue.Fields.Summary),
		"status:: " + issue.Fields.Status.Name,
		"status-simple:: " + SimplifyStatus(c, issue),
		"date-created:: [[" + DateFormat(time.Time(issue.Fields.Created)) + "]]",
		"date-created-sortable:: " + time.Time(issue.Fields.Created).Format("20060102"),
	}

	if issue.Fields.Parent != nil {
		output = append(output, "parent:: [["+issue.Fields.Parent.Key+"]]")
	}

	if c.ExcludeFromGraph {
		output = append(output, "exclude-from-graph-view:: true")
	}

	if c.IncludeWatchers && issue.Fields.Watches != nil && issue.Fields.Watches.WatchCount > 0 {

		err = GetWatchers(c, issue, watchers)
		if err != nil {
			return errors.Wrap(err, "Failed in GetWatchers")
		}

		slices.Sort(*watchers)

		output = append(output, "watchers:: "+strings.Join(*watchers, ", "))
	}

	if time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1 {
		output = append(output,
			"date_due:: [["+DateFormat(time.Time(issue.Fields.Duedate))+"]]",
			"date_due_sortable:: "+time.Time(issue.Fields.Duedate).Format("20060102"),
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

	line, err := ParseJiraText(c, issue.Fields.Description)
	if err != nil {
		return errors.Wrap(err, "Failed in ParseJiraText")
	}

	output = append(output, line...)

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
				output = append(output, "\t- [["+issue+"]]")
			}
		}

	}

	if (c.IncludeTask &&
		time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1) ||
		(c.IncludeMyTasks &&
			issue.Fields.Assignee != nil &&
			issue.Fields.Assignee.DisplayName == c.Connection.DisplayName) {
		output = append(output,
			"- ***",
			"- "+SimplifyStatus(c, issue)+" [[Jira Task]] [["+issue.Key+"]]")
		if time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1 {
			output = append(output, "\tDEADLINE: <"+time.Time(issue.Fields.Duedate).Format("2006-01-02 Mon")+">",
				"\tSCHEDULED: <"+time.Time(issue.Fields.Duedate).Format("2006-01-02 Mon")+">",
			)
		}
	}

	if c.IncludeComments {
		fetchedIssue, err = GetIssue(c, issue, fetchedIssue)
		if err != nil {
			return errors.Wrap(err, "Failed in GetIssue")
		}
		if fetchedIssue.Fields.Comments != nil && len(fetchedIssue.Fields.Comments.Comments) > 0 {
			output = append(output, "- ### Comments")
			for _, comment := range fetchedIssue.Fields.Comments.Comments {
				nameText := comment.Author.DisplayName
				if c.LinkNames {
					nameText = "[[" + nameText + "]]"
				}

				// Mon Jan 2 15:04:05 -0700 MST 2006
				format := "2006-01-02T15:04:05.000-0700" // 2024-05-10T13:46:45.585-0500

				created, err := time.Parse(format, comment.Created)
				if err != nil {
					return errors.Wrap(err, "Failed to get comment creation time")
				}

				updated, err := time.Parse(format, comment.Updated)
				if err != nil {
					return errors.Wrap(err, "Failed to get comment update time")
				}

				output = append(output, "- "+nameText+" - Created: "+DateFormat(created)+" | Updated: "+DateFormat(updated))

				line, err := ParseJiraText(c, comment.Body)
				if err != nil {
					return errors.Wrap(err, "Failed in ParseJiraText")
				}

				output = append(output, PrefixStringSlice(line, "\t")...)
				output = append(output, "***")
			}
		}
	}

	if fetchedIssue == nil {
		issuesStore = append(issuesStore, fetchedIssue)
	} else {
		issuesStore = append(issuesStore, issue)
	}

	err = WritePage(issue.Key, []byte(strings.Join(output, "\n")))

	if err == nil {
		c.progress[project].IncrBy(1)
	}

	return err

}

func SimplifyStatus(c *JiraConfig, i *jira.Issue) string {
	for _, status := range c.Status.Done {
		if i.Fields.Status.Name == status {
			return "DONE"
		}
	}
	return "TODO"
}

func (c *JiraConfig) createClient() (*jira.Client, error) {
	tp := jira.BasicAuthTransport{
		Username: c.Connection.Username,
		APIToken: c.Connection.APIToken,
	}
	return jira.NewClient(c.Connection.BaseURL, tp.Client())
}

// Modified from https://github.com/andygrunwald/go-jira/issues/55#issuecomment-676631140
func GetIssues(c *JiraConfig, searchString string, project string, issues chan jira.Issue) (err error) {
	last := 0
	newIssues := []*jira.Issue{}
	for {
		opt := &jira.SearchOptions{
			MaxResults: 100,
			StartAt:    last,
		}

		o, resp, err := APIWrapper(c, func(a []any) (output []any, resp *jira.Response, err error) {
			output = make([]any, 1)
			output[0], resp, err = c.client.Issue.Search(context.Background(), a[0].(string), a[1].(*jira.SearchOptions))
			return output, resp, errors.Wrap(err, "Couldn't search issues using jql '"+a[0].(string)+"'")
		}, []any{
			searchString,
			opt,
		})
		if err != nil {
			return errors.Wrap(err, "Failed in APIWrapper")
		}
		chunk := o[0].([]jira.Issue)

		total := resp.Total
		for _, i := range chunk {
			newIssues = append(newIssues, &i)
			issues <- i
		}
		last = resp.StartAt + len(chunk)
		c.progress[project].SetTotal(int64(last), false)
		if last >= total {
			break
		}
	}

	for ik := range knownIssues { // Also want to reprocess
		seen := false
		for _, ni := range newIssues {
			if ik == ni.Key {
				seen = true
				break
			}
		}
		if !seen {
			if knownIssues[ik].Fields.Project.Key == project {
				issues <- *knownIssues[ik]
			}
		}
	}

	for _, ni := range newIssues {
		knownIssues[ni.Key] = ni
	}

	close(issues)
	return nil
}

func ParseJiraText(c *JiraConfig, input string) ([]string, error) {

	if *debug {
		// Useful for debugging original content vs J2M output.
		h1 := fnv1a.HashString64(input)

		WriteFile("./debug/"+strconv.FormatUint(h1, 36)+".original", []byte(input))
		WriteFile("./debug/"+strconv.FormatUint(h1, 36)+".formatted", []byte(JiraToMD(input)))
	}

	description := strings.Split(JiraToMD(input), "\n")
	descriptionFormatted := []string{""}

	for _, l := range description {

		lines := []string{l}
		if lines[0] == "" {
			continue
		}

		listItem := false

		// Bullet list
		matcher := `^( *)\* `
		if regexp.MustCompile(matcher).MatchString(lines[0]) {
			listItem = true

			frontPad := regexp.MustCompile(matcher+".*").ReplaceAllString(lines[0], "$1")
			newPad := strings.Repeat("\t", len(frontPad))

			lines[0] = regexp.MustCompile(matcher).ReplaceAllString(lines[0], "")
			lines[0] = newPad + "\t- " + lines[0]
		}

		// Ordered list
		matcher = `^( *)[0-9]+\.( |\) )`
		if regexp.MustCompile(matcher).MatchString(lines[0]) {
			listItem = true

			frontPad := regexp.MustCompile(matcher+".*").ReplaceAllString(lines[0], "$1")
			newPad := strings.Repeat("\t", len(frontPad)/2)

			lines[0] = regexp.MustCompile(matcher).ReplaceAllString(lines[0], "")
			lines[0] = newPad + "\t- " + lines[0]

			lines = append(lines, newPad+"\t  logseq.order-list-type:: number")
		}

		if !listItem {
			lines[0] = "- " + lines[0]
		}

		// Account ID
		matcher = `<~accountid:([0-9]*:|)([^>]+)>`
		if regexp.MustCompile(matcher).MatchString(lines[0]) {

			accountIDs := regexp.MustCompile(matcher).FindAllString(lines[0], -1)

			for _, rawAccountID := range accountIDs {

				accountID := regexp.MustCompile(matcher).ReplaceAllString(rawAccountID, `$2`)

				if accountID == "" {
					slog.Info("Empty accountID in line: " + lines[0])
				}

				displayName, err := FindUser(c, accountID)
				if err != nil {
					if c.SearchUsers {
						slog.Info(err.Error() + " - Can't find user, likely an authorization error, won't bother retrying.")
						c.SearchUsers = false
					}
					displayName = accountID
				} else {
					if c.LinkNames {
						displayName = "[[" + displayName + "]]"
					}
				}

				lines[0] = strings.Replace(lines[0], rawAccountID, displayName, 1)
			}
		}

		descriptionFormatted = append(descriptionFormatted, lines...)
	}

	return descriptionFormatted, nil
}

func PrefixStringSlice(i []string, p string) (o []string) {
	for _, l := range i {
		o = append(o, p+l)
	}
	return
}

func GetIssue(c *JiraConfig, sparseIssue *jira.Issue, fullIssueCheck *jira.Issue) (fullIssue *jira.Issue, err error) {

	cachedFilePath := strings.Join([]string{config.CacheRoot, sparseIssue.Key, time.Time(sparseIssue.Fields.Updated).Format("2006-01-02T15-04-05.999999999Z07-00")}, "/") + ".json"

	dir := regexp.MustCompile("[^/]*$").ReplaceAllString(cachedFilePath, "")

	err = os.MkdirAll(dir, os.ModeDir)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to make cache directory "+dir)
	}

	if _, err := os.Stat(cachedFilePath); errors.Is(err, os.ErrNotExist) || *ignoreCache {

		if fullIssueCheck == nil {
			slog.Info("Fetching specific info for " + sparseIssue.Key)

			o, _, err := APIWrapper(c, func(a []any) (output []any, resp *jira.Response, err error) {
				output = make([]any, 1)
				output[0], resp, err = c.client.Issue.Get(context.Background(), a[0].(string), nil)
				return output, resp, errors.Wrap(err, "Couldn't get issue "+a[0].(string))
			}, []any{
				sparseIssue.Key,
			})
			if err != nil {
				return nil, errors.Wrap(err, "Failed in APIWrapper")
			}
			fullIssue = o[0].(*jira.Issue)

		} else {
			fullIssue = fullIssueCheck
		}

		jsonBytes, err := json.Marshal(fullIssue)
		if err != nil {
			return nil, errors.Wrap(err, "Failed in json.Marshal")
		}

		err = WriteFile(cachedFilePath, jsonBytes)
		if err != nil {
			return nil, errors.Wrap(err, "Failed in write file "+cachedFilePath)
		}

	} else if err != nil {

		return nil, errors.Wrap(err, cachedFilePath)

	} else {

		jsonFile, err := os.Open(cachedFilePath)

		if err != nil {
			return nil, errors.Wrap(err, "Failed to open file")
		}

		defer jsonFile.Close()

		byteValue, _ := io.ReadAll(jsonFile)

		err = json.Unmarshal(byteValue, &fullIssue)
		if err != nil {
			return nil, errors.Wrap(err, "Failed to unmarshal file")
		}

		jiraCacheHits.IncrBy(1)

	}

	return fullIssue, nil

}

func GetWatchers(c *JiraConfig, i *jira.Issue, watchers *[]string) error {
	if len(*watchers) > 0 {
		return nil
	}

	cachedFilePath := strings.Join([]string{config.CacheRoot, i.Key, time.Time(i.Fields.Updated).Format("2006-01-02T15-04-05.999999999Z07-00")}, "/") + "_watchers.json"

	dir := regexp.MustCompile("[^/]*$").ReplaceAllString(cachedFilePath, "")

	err := os.MkdirAll(dir, os.ModeDir)
	if err != nil {
		return errors.Wrap(err, "Failed to make cache directory "+dir)
	}

	if _, err := os.Stat(cachedFilePath); errors.Is(err, os.ErrNotExist) || *ignoreCache {

		slog.Info("Getting watchers for " + i.Key)
		o, _, err := APIWrapper(c, func(a []any) (output []any, resp *jira.Response, err error) {
			output = make([]any, 1)
			output[0], resp, err = c.client.Issue.GetWatchers(context.Background(), a[0].(string))
			return output, resp, errors.Wrap(err, "Couldn't get watchers for "+a[0].(string))
		}, []any{
			i.ID,
		})
		if err != nil {
			return errors.Wrap(err, "Failed in APIWrapper")
		}
		watchingUsers := o[0].(*[]jira.User)

		for _, u := range *watchingUsers {
			nameText := u.DisplayName
			if c.LinkNames {
				nameText = "[[" + nameText + "]]"
			}
			*watchers = append(*watchers, nameText)
		}

		jsonBytes, err := json.Marshal(watchers)
		if err != nil {
			return errors.Wrap(err, "Failed in json.Marshal")
		}

		err = WriteFile(cachedFilePath, jsonBytes)
		if err != nil {
			return errors.Wrap(err, "Failed in write file "+cachedFilePath)
		}

	} else if err != nil {

		return errors.Wrap(err, cachedFilePath)

	} else {

		jsonFile, err := os.Open(cachedFilePath)

		if err != nil {
			return errors.Wrap(err, "Failed to open file")
		}

		defer jsonFile.Close()

		byteValue, _ := io.ReadAll(jsonFile)

		err = json.Unmarshal(byteValue, &watchers)
		if err != nil {
			return errors.Wrap(err, "Failed to open file")
		}

		jiraCacheHits.IncrBy(1)

	}

	return nil
}

func LogseqTransform(str string) string {
	return SearchAndReplace(str, []struct {
		matcher string
		repl    string
	}{
		{ // Replace slash with FULLWIDTH SOLIDUS to prevent hierarchy pages being made
			matcher: `/`,
			repl:    `／`,
		},
		{ // Replace [[text]] with (text)
			matcher: `\[\[ *([^ ][^\]]+[^ ]) *\]\]`,
			repl:    `( $1 )`,
		},
	})
}

func LogseqTitle(issue *jira.Issue) string {
	return issue.Key + " | " + LogseqTransform(issue.Fields.Summary)
}

func IssueMap() error {
	output := []string{}

	for _, i := range issuesStore {
		issues[i.Key] = i
	}

	for key, issue := range issues {

		if _, ok := children[key]; !ok {
			children[key] = []string{}
		}
		if issue.Fields.Parent != nil {
			parents[key] = &issue.Fields.Parent.Key
			children[issue.Fields.Parent.Key] = append(children[issue.Fields.Parent.Key], key)
		} else {
			parents[key] = nil
		}
	}

	topLevel := []string{}

	for child, parent := range parents {
		if parent == nil {
			topLevel = append(topLevel, child)
		}
	}

	natsort.Sort(topLevel)

	for _, target := range topLevel {
		err := RecurseIssueMap(target, &output, 0)
		if err != nil {
			return errors.Wrap(err, "Recursion failed on "+target+" at depth 0")
		}
	}

	return errors.Wrap(WritePage("Jira/Item Hierarchy", []byte(strings.Join(output, "\n"))), "Failed to write page in IssueMap")
}

func RecurseIssueMap(target string, output *([]string), depth int) error {
	*output = append(*output, strings.Repeat("\t", depth)+"- [["+LogseqTitle(issues[target])+"]]")
	if depth == 0 {
		*output = append(*output, "  collapsed:: true")
	}

	targetChildren := children[target]
	natsort.Sort(targetChildren)

	for _, child := range targetChildren {
		err := RecurseIssueMap(child, output, depth+1)
		if err != nil {
			return errors.Wrap(err, "Recursion failed on "+target+" at depth "+strconv.Itoa(depth))
		}
	}
	return nil
}

func APIWrapper(c *JiraConfig, f func([]any) ([]any, *jira.Response, error), i []any) (output []any, resp *jira.Response, err error) {
	var body []byte
	var errBody error
	retryCount := 0
	for {
		retryCount += 1
		c.apiLimited.Lock()
		c.apiLimited.Unlock() //lint:ignore SA2001 as we've only checked so we can make our API call - still risk of race condition, but lessened
		retry := false
		jiraApiCalls.IncrBy(1)
		output, resp, err = f(i)
		if resp != nil {
			body, errBody = io.ReadAll(resp.Body)
			if errBody != nil {
				err = errors.Wrap(err, "Failed to read response body: "+errBody.Error())
			}
		} else {
			if retryCount < 3 {
				continue
			} else {
				return nil, nil, errors.Wrap(err, "No response")
			}
		}
		if resp.StatusCode != 200 && resp.StatusCode != 429 {
			return nil, nil, errors.Wrap(
				errors.Wrap(
					err,
					string(body),
				),
				"APIWrapper failed due to status "+strconv.Itoa(resp.StatusCode))
		}
		if err != nil {
			retry, err = CheckAPILimit(c, resp)
			if err != nil {
				return nil, nil, errors.Wrap(err, "Failed API limit check")
			}
		}
		if !retry {
			break
		}

	}
	return output, resp, errors.Wrap(err, "Failed somewhere in APIWrapper")
}

func CheckAPILimit(c *JiraConfig, resp *jira.Response) (retry bool, err error) {
	if resp.StatusCode == 200 {
		return false, nil
	} else if resp.StatusCode == 429 {
		retry = true
		c.apiLimited.Lock()
		defer c.apiLimited.Unlock()
		resetTime, err := time.Parse("2006-01-02T15:04Z", resp.Response.Header.Get("X-Ratelimit-Reset"))
		if err != nil {
			resetTime = time.Now().Add(time.Minute * 2)
			slog.Error("Failed to parse X-Ratelimit-Reset time, defaulting to " + resetTime.Format(time.RFC822))
			err = nil
		}
		resetTime = resetTime.Add(time.Second) // Add one second buffer just in case
		slog.Warn("API calls exhausted, sleeping until " + fmt.Sprint(resetTime))
		time.Sleep(time.Until(resetTime))
		slog.Warn("Waking up, API should be usable again, retrying last call.")
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, errors.Wrap(err, "Failed to read response body")
		}
		return false, errors.New(string(body))
	}

	return
}

func FindUser(c *JiraConfig, id string) (string, error) {
	usersLock.Lock()
	defer usersLock.Unlock()

	if val, ok := users[id]; ok { // User is already present
		return val, nil
	}

	for _, u := range config.Jira.Users {
		if u.AccountID == id { // User is in config
			users[id] = u.DisplayName
			return users[id], nil
		}
	}
	if !c.SearchUsers {
		return id, errors.New("Cannot find given user")
	}

	// This has never worked for me (data protection...)
	slog.Info("Getting user for " + id)
	o, _, err := APIWrapper(c, func(a []any) (output []any, resp *jira.Response, err error) {
		output = make([]any, 1)
		req, err := c.client.NewRequest(context.Background(), "GET", "/rest/api/3/user?accountId="+a[0].(string), nil)
		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed to create request for /rest/api/3/user")
		}

		ret := &jira.User{}
		resp, err = c.client.Do(req, ret)
		if err != nil {
			err = errors.Wrap(err, "Failed to do request for /rest/api/3/user")
		}

		return output, resp, errors.Wrap(err, "Failed to get user for id "+a[0].(string))
	}, []any{
		id,
	})
	if err != nil {
		return "", errors.Wrap(err, "Failed to run APIWrapper")
	}
	foundUser := o[0].(*jira.User)

	users[id] = foundUser.DisplayName

	return users[id], errors.Wrap(err, "Failed somewhere in FindUser")

}
