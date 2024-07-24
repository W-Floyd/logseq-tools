package main

import (
	"slices"
	"strings"
	"time"

	jira "github.com/andygrunwald/go-jira/v2/cloud"
	"github.com/xuri/excelize/v2"
)

func (c Config) ProcessTables() error {

	parents, children := IssueMap()

	for _, instance := range c.Jira.Instances {
		for _, project := range instance.Projects {
			if project.Options.Outputs.Table != nil && *project.Options.Outputs.Table.Enabled {

				data := [][]string{}

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

				for _, issue := range topLevel {

					for _, childIssue := range children[issue.Key] {

						dateEnd := ""
						if time.Time(knownIssues[childIssue].Fields.Duedate).Compare(time.Time{}) == 1 {
							dateEnd = time.Time(knownIssues[childIssue].Fields.Duedate).Format("2006/01/02")
						}

						customFields, err := GetCustomFields(project, knownIssues[childIssue])
						if err != nil {
							return err
						}

						// dateStart := ""
						dateEndBaseline := ""

						for _, customField := range project.Options.CustomFields {

							switch *customField.To {

							case "date_due_baseline":

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

						data = append(data, []string{
							// issue.Fields.Summary,
							knownIssues[childIssue].Fields.Summary,
							knownIssues[childIssue].Fields.Status.Name,
							dateEndBaseline,
							dateEnd,
						})

					}

				}

				slices.SortFunc(data, func(a, b []string) int {
					i := 3
					return strings.Compare(a[i], b[i])
				})

				data = append([][]string{{"Task", "Status", "Estimated Completion", "Actual Completion"}}, data...)

				f := excelize.NewFile()

				for i, row := range data {
					for j, val := range row {
						coord, err := excelize.CoordinatesToCellName(j+1, i+1)
						if err != nil {
							return err
						}
						f.SetCellValue("Sheet1", coord, val)
					}
				}

				// Save spreadsheet by the given path.
				err := f.SaveAs("./table_" + *project.Key + ".xlsx")
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
