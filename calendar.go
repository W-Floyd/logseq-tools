package main

import (
	"fmt"
	"math"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apognu/gocal"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type CalendarConfig struct {
	Enabled       bool   `json:"enabled"`
	Title         string `json:"title"`
	IcsUrl        string `json:"ics_url"`
	AllEventsDone bool   `json:"all_events_done"`
	Exclusions    struct {
		MaxDuration struct {
			Enabled     bool    `json:"enabled"`
			LengthHours float64 `json:"length_hours"`
		} `json:"max_duration"`
		Titles    []string `json:"titles"`
		PastDates bool     `json:"past_dates"`
	} `json:"exclusions"`
}

func (c *CalendarConfig) Process(wg *errgroup.Group) (err error) {

	if !c.Enabled {
		return nil
	}

	resp, err := http.Get(c.IcsUrl)
	if err != nil {
		return errors.Wrap(err, "Failed in http.Get")
	}

	var tzMapping = map[string]string{
		"Central Standard Time":  "US/Central",
		"Mountain Standard Time": "US/Mountain",
		"Eastern Standard Time":  "US/Eastern",
	}

	gocal.SetTZMapper(func(s string) (*time.Location, error) {
		if tzid, ok := tzMapping[s]; ok {
			return time.LoadLocation(tzid)
		}
		return nil, fmt.Errorf("")
	})

	cal := gocal.NewParser(resp.Body)
	cal.SkipBounds = true
	end := time.Now().Add(time.Hour * 24 * 180)
	cal.End = &end

	cal.Parse()

	slices.SortFunc(cal.Events, func(a, b gocal.Event) int {
		comp := a.Start.Compare(*b.Start)
		if comp != 0 {
			return comp
		}
		comp = a.End.Compare(*b.End)
		if comp != 0 {
			return comp
		}
		return strings.Compare(a.Summary, b.Summary)
	})

	days := map[string][]string{}

	dateFormat := "2006_01_02"

	for _, e := range cal.Events {

		duration := e.End.Sub(*e.Start)

		// Excluded titles
		if slices.Contains(c.Exclusions.Titles, e.Summary) {
			continue
		}

		// MaxDuration
		if c.Exclusions.MaxDuration.Enabled && duration >= time.Duration(c.Exclusions.MaxDuration.LengthHours*float64(time.Hour)) {
			continue
		}

		durationMinutes := int(math.Round(duration.Minutes()))

		page := e.Start.Format(dateFormat)

		text := []string{}

		if e.Status == "CANCELED" || strings.HasPrefix(e.Summary, "Canceled: ") {
			text = append(text,
				"- CANCELED [[Calendar Event]] - "+e.Summary,
			)
		} else {
			if e.End.Before(time.Now()) || c.AllEventsDone {
				text = append(text,
					"- DONE [[Calendar Event]] - "+e.Summary,
				)
			} else {
				text = append(text,
					"- WAITING [[Calendar Event]] - "+e.Summary,
				)
			}
		}

		text = append(text,
			"  SCHEDULED: <"+e.Start.Format("2006-01-02 Mon 15:04")+">",
			"  :AGENDA:",
			"  estimated: "+strconv.Itoa(
				durationMinutes,
			)+"m",
			"  :END:",
			"  status:: "+e.Status,
		)

		if e.Organizer != nil {
			text = append(text,
				"  organizer:: "+e.Organizer.Value,
			)
		}

		days[page] = append(days[page], text...)

	}

	for k, d := range days {
		if time.Now().Format(dateFormat) <= k || c.Exclusions.PastDates {
			err = WriteFile(
				path.Join(
					config.Jira.Options.Paths.LogseqRoot,
					"pages",
					"calendar",
					c.Title,
					PageNameToFileName("calendar/"+c.Title+"/"+k)+".md"),
				[]byte(strings.Join(d, "\n")))
			if err != nil {
				return errors.Wrap(err, "Failed in WriteFile for "+k)
			}
		}
	}

	return

}
