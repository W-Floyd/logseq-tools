package main

import (
	"slices"
	"strings"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/cloud"
	"github.com/pkg/errors"
	"github.com/xuri/excelize/v2"
)

func (c Config) ProcessTables() error {

	parents, children := IssueMap()

	for _, instance := range c.Jira.Instances {

		instanceOptions, err := UnderlayOptions(&c.Jira.Options, &instance.Options)
		if err != nil {
			return errors.Wrap(err, "Couldn't merge GeneralOptions with InstanceOptions")
		}

		instance.Options = *instanceOptions

		for _, project := range instance.Projects {
			if project.Options.Outputs.Table.Enabled != nil && *project.Options.Outputs.Table.Enabled {

				projectOptions, err := UnderlayOptions(&instance.Options, &project.Options)
				if err != nil {
					return errors.Wrap(err, "Couldn't merge GeneralOptions with ProjectOptions")
				}

				project.Options = *projectOptions

				issues := []*jira.Issue{}

				for _, issue := range knownIssues {
					if issue.Fields.Project.Key == *project.Key {
						issues = append(issues, issue)
					}
				}

				topLevel := []*jira.Issue{}

				for _, issue := range issues {
					if p, ok := parents[issue.Key]; !ok || p == nil {
						topLevel = append(topLevel, issue)
					}
				}

				f := excelize.NewFile()

				header := []string{
					"Epic", "Task", "Status", "Baseline Completion", "Actual Completion",
					"Delay",
				}

				i := 1

				linkStyle, err := f.NewStyle(&excelize.Style{
					Font: &excelize.Font{Color: "1265BE", Underline: "single"},
				})
				if err != nil {
					return err
				}

				headerStyle, err := f.NewStyle(&excelize.Style{
					Font: &excelize.Font{
						Size: 12,
						Bold: true,
					},
				})
				if err != nil {
					return err
				}

				greenStyle, err := f.NewStyle(&excelize.Style{
					Font: &excelize.Font{Color: "006100"},
					Fill: excelize.Fill{
						Type:    "pattern",
						Color:   []string{"C6EFCE"},
						Pattern: 1,
					},
				})
				if err != nil {
					return err
				}

				orangeStyle, err := f.NewStyle(&excelize.Style{
					Font: &excelize.Font{Color: "9C5700"},
					Fill: excelize.Fill{
						Type:    "pattern",
						Color:   []string{"FFEB9C"},
						Pattern: 1,
					},
				})
				if err != nil {
					return err
				}

				for j, h := range header {
					coord, err := excelize.CoordinatesToCellName(j+1, i)
					if err != nil {
						return err
					}

					f.SetCellValue("Sheet1", coord, h)
					f.SetCellStyle("Sheet1", coord, coord, headerStyle)
				}

				issueList := []struct {
					Issue  string
					Parent *jira.Issue
				}{}

				for _, issue := range topLevel {

					for _, childIssue := range children[issue.Key] {

						issueList = append(issueList, struct {
							Issue  string
							Parent *jira.Issue
						}{
							Issue:  childIssue,
							Parent: issue,
						})

					}
				}

				slices.SortFunc(issueList, func(a, b struct {
					Issue  string
					Parent *jira.Issue
				}) int {
					s := 0
					aDue, err := GetDueDate(knownIssues[a.Issue], project)
					if err != nil {
						errors.Wrap(err, "Failed in GetDueDate for "+knownIssues[a.Issue].Key)
					}
					bDue, err := GetDueDate(knownIssues[b.Issue], project)
					if err != nil {
						errors.Wrap(err, "Failed in GetDueDate for "+knownIssues[b.Issue].Key)
					}

					if aDue == nil {
						aDue = &time.Time{}
					}
					if bDue == nil {
						bDue = &time.Time{}
					}

					if aDue != nil && bDue != nil {
						s = aDue.Compare(*bDue)
					}
					if s == 0 {
						s = strings.Compare(a.Parent.Fields.Summary, b.Parent.Fields.Summary)
					}
					if s == 0 {
						s = strings.Compare(knownIssues[a.Issue].Fields.Summary, knownIssues[b.Issue].Fields.Summary)
					}
					return s
				})

				outputDateFormat := "2006/01/02"

				for _, lineItem := range issueList {

					issue := lineItem.Parent
					childIssue := lineItem.Issue

					i += 1

					dateEnd := ""
					dateEndTime, err := GetDueDate(knownIssues[childIssue], project)
					if err != nil {
						errors.Wrap(err, "Failed in GetDueDate for "+knownIssues[childIssue].Key)
					}
					if dateEndTime != nil {
						dateEnd = dateEndTime.Format("2006/01/02")
					}

					_, customFields, err, _ := GetIssue(project, knownIssues[childIssue], nil)
					if err != nil {
						return err
					}

					dateEndBaseline := ""

					for _, customField := range project.Options.CustomFields {
						switch *customField.To {
						case "date-due-baseline":
							val, ok := customFields[*customField.From]
							if val != "" && val != "<nil>" && ok {
								dateEndBaselineTime, err := time.Parse("2006-01-02", val)
								if err != nil {
									return err
								}
								dateEndBaseline = dateEndBaselineTime.Format("2006/01/02")
							}
						}
					}

					if dateEndBaseline == "" && dateEnd != "" {
						dateEndBaseline = dateEnd
					} else if dateEnd == "" && dateEndBaseline != "" {
						dateEnd = dateEndBaseline
					}

					for j := 0; j < len(header); j++ {
						coord, err := excelize.CoordinatesToCellName(j+1, i)
						if err != nil {
							return err
						}

						t := "value"
						var val interface{}
						disp := ""
						var style *int

						switch header[j] {
						case "Epic":
							t = "link"
							disp = issue.Fields.Summary
							val = *instance.Connection.BaseURL + "browse/" + issue.Key
						case "Task":
							t = "link"
							disp = knownIssues[childIssue].Fields.Summary
							val = *instance.Connection.BaseURL + "browse/" + knownIssues[childIssue].Key
						case "Status":
							val = knownIssues[childIssue].Fields.Status.Name
							if val == "Done" {
								style = &greenStyle
							}
						case "Baseline Completion":
							val = dateEndBaseline
						case "Actual Completion":
							if dateEnd != "" {
								style = &orangeStyle
								if dateEnd <= dateEndBaseline {
									style = &greenStyle
								}
							}
							val = dateEnd
						case "Delay":
							if dateEnd != "" {
								end, err := time.Parse(outputDateFormat, dateEnd)
								if err != nil {
									return err
								}
								endBaseline, err := time.Parse(outputDateFormat, dateEndBaseline)
								if err != nil {
									return err
								}

								val = int(end.Sub(endBaseline).Hours() / 24)

							}
						}

						switch t {
						case "value":
							switch val := val.(type) {
							case int:
								f.SetCellInt("Sheet1", coord, val)
							default:
								f.SetCellValue("Sheet1", coord, val)
							}

							if style != nil {
								f.SetCellStyle("Sheet1", coord, coord, *style)
							}
						case "link":
							f.SetCellStyle("Sheet1", coord, coord, linkStyle)
							f.SetCellValue("Sheet1", coord, disp)
							f.SetCellHyperLink("Sheet1", coord, val.(string), "External", excelize.HyperlinkOpts{Display: &disp})
						}

					}

				}

				// Save spreadsheet by the given path.
				err = f.SaveAs("./table_" + *project.Key + ".xlsx")
				if err != nil {
					return err
				}

				err = f.Close()
				if err != nil {
					return err
				}
			}
		}
	}

	return nil

}
