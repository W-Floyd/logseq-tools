package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"log/slog"

	"dario.cat/mergo"
	"github.com/Jeffail/gabs/v2"
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
		BaseURL     *string `json:"base_url"`
		Username    *string `json:"username"`
		DisplayName *string `json:"display_name"`
		APIToken    *string `json:"api_token"`
		Parallel    *int    `json:"parallel"`
	} `json:"connection"`

	Options  JiraOptions    `json:"options"`
	Projects []*JiraProject `json:"projects"`

	apiLimited *sync.Mutex  // Lock this to prevent calls while API cools down, unlock once done
	client     *jira.Client // Client to use for communication
	progress   map[string]*mpb.Bar
}

type JiraProject struct {
	Key     *string     `json:"key"`     // Project key
	Options JiraOptions `json:"options"` // Project specific options
	config  *JiraConfig // Config for reference
}

type OutputFormat struct {
	Enabled *bool
}

type JiraOptions struct {
	Enabled          *bool `json:"enabled"`            // Whether to process this Jira project
	IncludeWatchers  *bool `json:"include_watchers"`   // This can be slow, so you may want to disable it
	IncludeComments  *bool `json:"include_comments"`   // This can be slow, so you may want to disable it
	ExcludeFromGraph *bool `json:"exclude_from_graph"` // If you have a lot of these, it can easily pollute your graph
	IncludeDone      *bool `json:"include_done"`       // Whether to include done items to help clean up the list
	IncludeTask      *bool `json:"include_task"`       // Whether to include a task on each item with a due date
	IncludeMyTasks   *bool `json:"include_my_tasks"`   // Whether to include my tasks in all cases
	LinkNames        *bool `json:"link_names"`         // Whether to [[link]] names
	LinkDates        *bool `json:"link_dates"`         // Whether to [[link]] dates
	SearchUsers      *bool `json:"search_users"`       // Whether to search users - may not be possible due to permissions

	Paths struct {
		LogseqRoot string `json:"logseq_root"`
		CacheRoot  string `json:"cache_root"`
	} `json:"paths"`

	Outputs struct {
		Logseq *OutputFormat
		Table  *OutputFormat
	} `json:"outputs"`

	CustomFields []struct {
		From *string `json:"from"`
		To   *string `json:"to"`
		As   *string `json:"as"`
	} `json:"custom_fields"`

	Status struct {
		Match []struct {
			From    []*string `json:"from"`    // Statuses to translate from
			To      *string   `json:"to"`      // Status to translate to
			Exclude *bool     `json:"exclude"` // Whether to exclude any matching statuses
		} `json:"match"`
		Default *string `json:"default"`
	} `json:"status"`

	Type []struct {
		From    []*string `json:"from"`
		To      *string   `json:"to"`
		Exclude *bool     `json:"exclude"`
	} `json:"type"`
}

var (
	users     map[string]string   = map[string]string{}
	usersLock *sync.Mutex         = &sync.Mutex{}
	parents   map[string]*string  = map[string]*string{}
	children  map[string][]string = map[string][]string{}
)

func (c *JiraConfig) Process(wg *errgroup.Group) (err error) {

	err = mergo.Merge(&c.Options, config.Jira.Options, mergo.WithAppendSlice, mergo.WithOverrideEmptySlice, mergo.WithSliceDeepCopy)
	if err != nil {
		return errors.Wrap(err, "Couldn't merge GeneralOptions with InstanceOptions")
	}

	for _, p := range c.Projects {
		p.config = c
	}

	if !*c.Options.Enabled {
		return nil
	}

	c.apiLimited = &sync.Mutex{}

	c.client, err = c.createClient()
	if err != nil {
		return errors.Wrap(err, "Couldn't create a client")
	}

	c.progress = make(map[string]*mpb.Bar)

	for _, project := range c.Projects {

		pbar := progress.AddBar(0,
			mpb.PrependDecorators(
				decor.Name(*project.Key, decor.WC{C: decor.DindentRight | decor.DextraSpace}),
				decor.Name("processing", decor.WCSyncSpaceR),
				decor.CountersNoUnit("%d / %d", decor.WCSyncWidth),
			),
			mpb.AppendDecorators(
				decor.OnComplete(decor.Percentage(decor.WC{W: 5}), "done"),
			),
		)

		c.progress[*project.Key] = pbar

	}

	for _, project := range c.Projects {

		err = ProcessProject(wg, project)
		if err != nil {
			return errors.Wrap(err, "Failed processing project "+*project.Key)
		}
	}

	return nil

}

func ProcessProject(wg *errgroup.Group, project *JiraProject) error {

	err := mergo.Merge(&project.Options, project.config.Options, mergo.WithAppendSlice, mergo.WithOverrideEmptySlice, mergo.WithSliceDeepCopy)
	if err != nil {
		return errors.Wrap(err, "Couldn't merge InstanceOptions with Options")
	}

	c := project.config

	errs, _ := errgroup.WithContext(context.Background())
	if c.Connection.Parallel != nil {
		errs.SetLimit(*c.Connection.Parallel)
	} else {
		errs.SetLimit(4)
	}

	slog.Info("Processing Project: " + *project.Key)

	issues := make(chan jira.Issue)

	query := "project = " + *project.Key

	if *recent {
		query += " AND updated >= " + lastRun.Add(time.Second*-30).Format(`"2006/01/02 15:04"`)
	}

	if *timeline {
		query += ` AND comment ~ 'ExtractTag'`
	}

	slog.Info("Query: " + query)

	go func() error {
		return GetIssues(query, project, issues)
	}()

	c.progress[*project.Key].SetTotal(int64(len(issues)), false)

	for issue := range issues {
		issue := issue
		if *timeline {
			errs.Go(func() error {
				err := ProcessTimeline(wg, &issue, project)
				return errors.Wrap(err, "Failed to ProcessTimeline "+issue.Key)
			})
		} else {
			errs.Go(func() error {
				err := ProcessIssue(wg, &issue, project)
				return errors.Wrap(err, "Failed to ProcessIssue "+issue.Key)
			})
		}
	}

	return errors.Wrap(errs.Wait(), "Goroutine failed from ProcessProject")

}

func ProcessIssue(wg *errgroup.Group, issue *jira.Issue, project *JiraProject) (err error) {

	c := project.config

	var fetchedIssue *jira.Issue // Use GetIssue() on this to populate on first use, and reuse thereafter
	watchers := &[]string{}      // use GetWatchers() on this to populate on first use, and reuse thereafter
	// Like so:
	// fetchedIssue, err = GetIssue(c, issue, fetchedIssue)
	// if err!=nil{
	//   return nil
	// }

	// Skip if excluded by a matcher
	for _, matcher := range project.Options.Status.Match {
		if matcher.Exclude != nil && *matcher.Exclude {
			for _, m := range matcher.From {
				if *m == issue.Fields.Status.Name {
					return nil
				}
			}
		}
	}

	slog.Info("Processing Issue: " + issue.Key)

	output := []string{
		"alias:: " + issue.Key,
		"title:: " + LogseqTitle(issue),
		"type:: jira-ticket",
		"jira-type:: " + JiraTypeSubstitute(project, issue),
		"jira-project:: " + *project.Key,
		"url:: " + *c.Connection.BaseURL + "browse/" + issue.Key,
		"description:: " + LogseqTransform(issue.Fields.Summary),
		"status:: " + issue.Fields.Status.Name,
		"status-simple:: " + SimplifyStatus(project, issue),
	}

	if issue.Fields.Parent != nil {
		output = append(output, "parent:: [["+issue.Fields.Parent.Key+"]]")
	}

	if *project.Options.ExcludeFromGraph {
		output = append(output, "exclude-from-graph-view:: true")
	}

	if *project.Options.IncludeWatchers && issue.Fields.Watches != nil && issue.Fields.Watches.WatchCount > 0 {

		err = GetWatchers(project, issue, watchers)
		if err != nil {
			return errors.Wrap(err, "Failed in GetWatchers")
		}

		slices.Sort(*watchers)

		if len(*watchers) > 0 {
			output = append(output, "watchers:: "+strings.Join(*watchers, ", "))
		}
	}

	if issue.Fields.Assignee != nil {
		output = append(output, "assignee:: "+ProcessPersonName(issue.Fields.Assignee, project))
	}

	if issue.Fields.Reporter != nil {
		output = append(output, "reporter:: "+ProcessPersonName(issue.Fields.Reporter, project))
	}

	fetchedIssue, _, err = GetIssue(project, issue, fetchedIssue)
	if err != nil {
		return errors.Wrap(err, "Failed in GetIssue")
	}

	if *project.Options.LinkDates {
		output = append(output, "date-created:: [["+DateFormat(time.Time(issue.Fields.Created))+"]]")
	}

	output = append(output, "date-created-sortable:: "+time.Time(issue.Fields.Created).Format("20060102"))

	issueForDueDateCheck := issue
	dueDateCheckDepth := 0
	hasDueDate := true

	for {
		if time.Time(issueForDueDateCheck.Fields.Duedate).Compare(time.Time{}) == 1 {
			if *project.Options.LinkDates {
				output = append(output, "date-due:: [["+DateFormat(time.Time(issueForDueDateCheck.Fields.Duedate))+"]]")
			}
			test := time.Time(issueForDueDateCheck.Fields.Duedate).Format("20060102")
			slog.Debug(test)
			output = append(output, "date-due-sortable:: "+time.Time(issueForDueDateCheck.Fields.Duedate).Format("20060102"))
			break
		}

		if issueForDueDateCheck.Fields.Parent == nil {
			hasDueDate = false
			break
		}

		var ok bool
		_, ok = knownIssues[issueForDueDateCheck.Fields.Parent.Key]

		if !ok {
			issueForDueDateCheck = &jira.Issue{
				ID:  issueForDueDateCheck.Fields.Parent.ID,
				Key: issueForDueDateCheck.Fields.Parent.Key,
			}
		} else {
			issueForDueDateCheck = knownIssues[issueForDueDateCheck.Fields.Parent.Key]
		}

		issueForDueDateCheck, _, err = GetIssue(project, issueForDueDateCheck, nil)
		if err != nil {
			return errors.Wrap(err, "Failed in GetIssue on parent "+issueForDueDateCheck.Key)
		}

		dueDateCheckDepth += 1

	}

	if hasDueDate {
		if dueDateCheckDepth == 0 {
			output = append(output, "date-due-explicit:: yes")
		} else {
			output = append(output, "date-due-explicit:: no")
		}
		output = append(output, "date-date-source:: [["+issueForDueDateCheck.Key+"]]")
	}

	hasClosedParent := false

	issueForClosedCheck := issue

	for {
		if issueForClosedCheck.Fields == nil || issueForClosedCheck.Fields.Parent == nil {
			break
		}

		var ok bool
		_, ok = knownIssues[issueForClosedCheck.Fields.Parent.Key]

		if !ok {
			issueForClosedCheck = &jira.Issue{
				ID:  issueForClosedCheck.Fields.Parent.ID,
				Key: issueForClosedCheck.Fields.Parent.Key,
			}
		} else {
			issueForClosedCheck = knownIssues[issueForClosedCheck.Fields.Parent.Key]
		}

		issueForClosedCheck, _, err = GetIssue(project, issueForClosedCheck, nil)
		if err != nil {
			return errors.Wrap(err, "Failed in GetIssue on parent "+issueForDueDateCheck.Key)
		}

		if SimplifyStatus(project, issueForClosedCheck) == "DONE" {
			hasClosedParent = true
			break
		}

	}

	if hasClosedParent {
		output = append(output, "has-closed-parent:: true")
	} else {
		output = append(output, "has-closed-parent:: false")
	}

	customFields, err := TranslateCustomFields(project, fetchedIssue)
	if err != nil {
		return errors.Wrap(err, "Failed in TranslateCustomFields")
	}

	output = append(output, customFields...)

	line, err := ParseJiraText(project, issue.Fields.Description, fetchedIssue)
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

	if (*project.Options.IncludeTask &&
		time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1) ||
		(*project.Options.IncludeMyTasks &&
			issue.Fields.Assignee != nil &&
			issue.Fields.Assignee.DisplayName == *c.Connection.DisplayName) {
		output = append(output,
			"- ***",
			"- "+SimplifyStatus(project, issue)+" [[Jira Task]] [["+issue.Key+"]]",
			"  id:: "+deterministicGUID(issue.Key))

		if time.Time(issue.Fields.Duedate).Compare(time.Time{}) == 1 {
			output = append(output, "\tDEADLINE: <"+time.Time(issue.Fields.Duedate).Format("2006-01-02 Mon")+">",
				"\tSCHEDULED: <"+time.Time(issue.Fields.Duedate).Format("2006-01-02 Mon")+">",
			)
		}
	}

	if *project.Options.IncludeComments {
		fetchedIssue, _, err = GetIssue(project, issue, fetchedIssue)
		if err != nil {
			return errors.Wrap(err, "Failed in GetIssue")
		}
		if fetchedIssue.Fields.Comments != nil && len(fetchedIssue.Fields.Comments.Comments) > 0 {
			output = append(output, "- ### Comments")
			for _, comment := range fetchedIssue.Fields.Comments.Comments {
				nameText := comment.Author.DisplayName
				if *project.Options.LinkNames {
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

				lines, err := ParseJiraText(project, comment.Body, fetchedIssue)
				if err != nil {
					return errors.Wrap(err, "Failed in ParseJiraText")
				}

				output = append(output, PrefixStringSlice(lines, "\t")...)
				output = append(output, "***")
			}
		}
	}

	err = WritePage(issue.Key, []byte(strings.Join(output, "\n")))

	if err == nil {
		c.progress[*project.Key].IncrBy(1)
	}

	return err

}

func SaveAttachment(project *JiraProject, a *jira.Attachment) (logseqPath string, err error) {

	c := project.config

	filename := "assets/jira/jira_" + a.ID + filepath.Ext(a.Filename)

	logseqPath = "../" + filename
	filePath := project.Options.Paths.LogseqRoot + "/" + filename

	if _, err := os.Stat(filePath); errors.Is(err, os.ErrNotExist) {

		o, _, err := APIWrapper(c, func(a []any) (output []any, resp *jira.Response, err error) {
			output = make([]any, 1)

			apiEndpoint := "rest/api/3/attachment/content/" + a[0].(string)

			req, err := c.client.NewRequest(context.Background(), http.MethodGet, apiEndpoint, nil)
			if err != nil {
				err = errors.New("Error in c.client.NewRequest")
				return
			}

			resp, err = c.client.Do(req, nil)

			if err != nil {
				err = errors.New("Error in c.client.Do")
				return
			}

			slog.Debug("Reading attachment body")

			fileContents, err := io.ReadAll(resp.Body)
			if err != nil {
				err = errors.Wrap(err, "Failed to read response body")
				return
			}

			output[0] = []byte{}

			output[0] = fileContents

			return output, resp, errors.Wrap(err, "Couldn't download attachment with ID '"+a[0].(string)+"'")
		}, []any{
			a.ID,
		})

		if err != nil {
			return "", errors.Wrap(err, "Failed in APIWrapper for attachment with filename "+a.Filename)
		}

		if o[0] == nil {
			return "", errors.New("Output is empty")
		}
		fileContents := o[0].([]byte)

		err = WriteFile(filePath, fileContents)
		if err != nil {
			return "", errors.Wrap(err, "Failed to write attachment file")
		}
	} else {
		jiraCacheHits.IncrBy(1)
	}

	return
}

func SimplifyStatus(project *JiraProject, i *jira.Issue) string {

	for _, matcher := range project.Options.Status.Match {
		for _, m := range matcher.From {
			if *m == i.Fields.Status.Name {
				return *matcher.To
			}
		}
	}

	return *project.Options.Status.Default
}

func (c *JiraConfig) createClient() (*jira.Client, error) {
	tp := jira.BasicAuthTransport{
		Username: *c.Connection.Username,
		APIToken: *c.Connection.APIToken,
	}
	return jira.NewClient(*c.Connection.BaseURL, tp.Client())
}

// Modified from https://github.com/andygrunwald/go-jira/issues/55#issuecomment-676631140
func GetIssues(searchString string, project *JiraProject, issues chan jira.Issue) (err error) {

	c := project.config

	totalIssuesForProject := 0

	for _, i := range knownIssues {
		if i.Fields.Project.Key == *project.Key {
			totalIssuesForProject += 1
		}
	}

	c.progress[*project.Key].SetTotal(int64(totalIssuesForProject), false)

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
		if resp.StatusCode == 404 {
			continue
		}
		if err != nil {
			return errors.Wrap(err, "Failed in APIWrapper for getting issues using "+searchString)
		}
		chunk := o[0].([]jira.Issue)

		total := resp.Total
		for _, i := range chunk {
			c.progress[*project.Key].SetTotal(int64(totalIssuesForProject), false)
			newIssues = append(newIssues, &i)
			if _, ok := knownIssues[i.Key]; !ok {
				totalIssuesForProject += 1
				knownIssues[i.Key] = &i
			} else if knownIssues[i.Key].Fields != nil && lastRun != nil && time.Time(knownIssues[i.Key].Fields.Updated).After(*lastRun) {
				knownIssues[i.Key] = &i
			}
			issues <- i
		}
		last = resp.StartAt + len(chunk)

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
			if knownIssues[ik].Fields.Project.Key == *project.Key {
				issues <- *knownIssues[ik]
			}
		}
	}

	close(issues)
	return nil
}

func ParseJiraText(project *JiraProject, input string, issue *jira.Issue) ([]string, error) {

	var err error

	description := strings.Split(JiraToMD(input), "\n")
	descriptionFormatted := []string{""}

	imageMatcher := `(?U)(!\[\]\()([^\)]+)\)`

	re := regexp.MustCompile(imageMatcher)

	imageReplacements := map[string]string{}

	for _, l := range description {

		// Images
		matches := re.FindAllString(l, -1)
		for _, match := range matches {
			filename := re.ReplaceAllString(match, `$2`)
			filepath := ""
			if !strings.HasPrefix(filename, "http") {
				if _, ok := imageReplacements[filename]; !ok {
					found := false
					for _, attachment := range issue.Fields.Attachments {
						if attachment.Filename == filename {
							filepath, err = SaveAttachment(project, attachment)
							if err != nil {
								return nil, errors.Wrap(err, "Failed to save attachment "+attachment.ID)
							}
							found = true
							break
						}
					}
					if found {
						imageReplacements[filename] = filepath
					} else {
						slog.Warn("Did not find attachment for " + filename)
						imageReplacements[filename] = filename
					}
				}

				l = strings.ReplaceAll(l, filename, imageReplacements[filename])

				l = re.ReplaceAllString(l, `![`+imageReplacements[filename]+`]($2)`)
			}

		}

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

				displayName, err := FindUser(project, accountID)
				if err != nil {
					if *project.Options.SearchUsers {
						slog.Info(err.Error() + " - Can't find user, likely an authorization error, won't bother retrying.")
						*project.Options.SearchUsers = false
					}
					displayName = accountID
				} else {
					if *project.Options.LinkNames {
						displayName = "[[" + displayName + "]]"
					}
				}

				lines[0] = strings.Replace(lines[0], rawAccountID, displayName, 1)
			}
		}

		// Issue links
		for _, matcher := range issueUrlMatchers {
			for i, line := range lines {
				lines[i] = matcher.ReplaceAllString(line, `[[$1]]`)
			}
		}

		descriptionFormatted = append(descriptionFormatted, lines...)
	}

	if *debug {
		// Useful for debugging original content vs J2M output.
		h1 := fnv1a.HashString64(input)

		WriteFile("./debug/"+strconv.FormatUint(h1, 36)+".original", []byte(input))
		WriteFile("./debug/"+strconv.FormatUint(h1, 36)+".formatted", []byte(strings.Join(description, "\n")))
		WriteFile("./debug/"+strconv.FormatUint(h1, 36)+".final", []byte(strings.Join(descriptionFormatted, "\n")))
	}

	return descriptionFormatted, nil
}

func PrefixStringSlice(i []string, p string) (o []string) {
	for _, l := range i {
		o = append(o, p+l)
	}
	return
}

func GetCachedIssuePath(project *JiraProject, sparseIssue *jira.Issue) (filepath string, dir string, err error) {
	if sparseIssue.Fields == nil {
		err = errors.New("No fields to parse, possibly a truly sparse issue")
		return
	}
	filepath = strings.Join([]string{project.Options.Paths.CacheRoot, sparseIssue.Key, time.Time(sparseIssue.Fields.Updated).Format("2006-01-02T15-04-05.999999999Z07-00")}, "/") + ".json"
	dir = regexp.MustCompile("[^/]*$").ReplaceAllString(filepath, "")
	return
}

func GetIssue(project *JiraProject, sparseIssue *jira.Issue, fullIssueCheck *jira.Issue) (fullIssue *jira.Issue, customFields jira.CustomFields, err error) {

	ignoreCacheLocal := false

	customFields = map[string]string{}

	c := project.config

	cachedFilePath, _, err := GetCachedIssuePath(project, sparseIssue)

	if err != nil {
		ignoreCacheLocal = true
	}

	var jsonByteValue []byte

	if !ignoreCacheLocal || !*ignoreCache {
		_, err = os.Stat(cachedFilePath)
	}

	if ignoreCacheLocal || *ignoreCache || errors.Is(err, os.ErrNotExist) {

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
				return nil, nil, errors.Wrap(err, "Failed in APIWrapper getting issue "+sparseIssue.Key)
			}
			fullIssue = o[0].(*jira.Issue)
		}

		jsonByteValue, err = json.MarshalIndent(fullIssue, "", "  ")
		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed in json.Marshal")
		}

		cachedFilePath, dir, err := GetCachedIssuePath(project, fullIssue)
		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed in GetCachedIssuePath")
		}

		if ignoreCacheLocal {
			err = os.MkdirAll(dir, os.ModeDir)
			if err != nil {
				return nil, nil, errors.Wrap(err, "Failed to make cache directory "+dir)
			}
		}

		err = WriteFile(cachedFilePath, jsonByteValue)
		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed in write file "+cachedFilePath)
		}

	} else if err != nil {

		return nil, nil, errors.Wrap(err, cachedFilePath)

	} else {

		jsonFile, err := os.Open(cachedFilePath)

		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed to open file")
		}

		defer jsonFile.Close()

		jsonByteValue, _ = io.ReadAll(jsonFile)

		err = json.Unmarshal(jsonByteValue, &fullIssue)
		if err != nil {
			return nil, nil, errors.Wrap(err, "Failed to unmarshal file")
		}

		jiraCacheHits.IncrBy(1)

	}

	jsonParsed, err := gabs.ParseJSON(jsonByteValue)
	if err != nil {
		return nil, nil, errors.Wrap(err, "Failed to unmarshal raw json: "+string(jsonByteValue))
	}

	for _, customField := range project.Options.CustomFields {
		val, ok := jsonParsed.Search("fields", *customField.From).Data().(string)
		if val != "" && val != "<nil>" && ok {
			customFields[*customField.From] = val
		}
	}

	return fullIssue, customFields, nil

}

func GetWatchers(project *JiraProject, i *jira.Issue, watchers *[]string) error {

	c := project.config

	if len(*watchers) > 0 {
		return nil
	}

	cachedFilePath := strings.Join([]string{project.Options.Paths.CacheRoot, i.Key, time.Time(i.Fields.Updated).Format("2006-01-02T15-04-05.999999999Z07-00")}, "/") + "_watchers.json"

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
			if resp == nil || resp.StatusCode == 404 {
				delete(knownIssues, i.Key)
				output = nil
			}
			return output, resp, errors.Wrap(err, "Couldn't get watchers for "+a[0].(string))
		}, []any{
			i.ID,
		})
		if err != nil {
			return errors.Wrap(err, "Failed in APIWrapper getting watchers of "+i.Key)
		}
		if o != nil {
			watchingUsers := o[0].(*[]jira.User)

			for _, u := range *watchingUsers {
				*watchers = append(*watchers, ProcessPersonName(&u, project))
			}

			jsonBytes, err := json.MarshalIndent(watchers, "", "  ")
			if err != nil {
				return errors.Wrap(err, "Failed in json.Marshal")
			}

			err = WriteFile(cachedFilePath, jsonBytes)
			if err != nil {
				return errors.Wrap(err, "Failed in write file "+cachedFilePath)
			}
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

func IssueMap() (parents map[string]*string, children map[string][]string) {
	parents = map[string]*string{}
	children = map[string][]string{}
	for key, issue := range knownIssues {

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
	return
}

func WriteIssueMap() error {
	output := []string{}

	parents, children = IssueMap()

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
	*output = append(*output, strings.Repeat("\t", depth)+"- [["+LogseqTitle(knownIssues[target])+"]]")
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
		if resp == nil && strings.Contains(err.Error(), "404") {
			err = nil
			return
		}
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
			resetTime = time.Now().Add(time.Minute * 3)
			slog.Error("Failed to parse X-Ratelimit-Reset time, defaulting to " + resetTime.Format(time.RFC822))
			err = nil
		}
		resetTime = resetTime.Add(time.Second) // Add one second buffer just in case
		slog.Warn("API calls exhausted, sleeping until " + fmt.Sprint(resetTime))
		time.Sleep(time.Until(resetTime))
		slog.Warn("Waking up, API should be usable again, retrying last call.")
	} else if resp.StatusCode == 404 {
		slog.Warn("404 not found")
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return false, errors.Wrap(err, "Failed to read response body")
		}
		return false, errors.New(string(body))
	}

	return
}

func FindUser(project *JiraProject, id string) (string, error) {

	c := project.config

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
	if !*project.Options.SearchUsers {
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

func JiraTypeSubstitute(project *JiraProject, issue *jira.Issue) string {
	for _, pair := range project.Options.Type {
		for _, m := range pair.From {
			if issue.Fields.Type.Description == *m {
				return *pair.To
			}
		}
	}
	return issue.Fields.Type.Description
}

func TranslateCustomFields(project *JiraProject, issue *jira.Issue) (output []string, err error) {

	_, customFields, err := GetIssue(project, issue, nil)
	if err != nil {
		errors.Wrap(err, "Failed in GetCustomFields")
	}

	for _, customField := range project.Options.CustomFields {
		val, ok := customFields[*customField.From]
		if val != "" && val != "<nil>" && ok {
			if customField.As != nil {
				switch *customField.As {
				case "date_sortable":
					if *project.Options.LinkDates {
						date, err := time.Parse("2006-01-02", val)
						if err != nil {
							errors.Wrap(err, "Failed in time.Parse")
						}
						output = append(output, *customField.To+":: [["+DateFormat(date)+"]]")
					}
					output = append(output, *customField.To+"-sortable:: "+strings.ReplaceAll(val, "-", ""))
				}
			} else {
				output = append(output, *customField.To+":: "+val)
			}
		}

	}
	return
}

func ProcessPersonName(person *jira.User, project *JiraProject) string {
	nameText := regexp.MustCompile("[0-9]").ReplaceAllString(person.DisplayName, "")
	if *project.Options.LinkNames {
		nameText = "[[" + nameText + "]]"
	}
	return nameText
}
