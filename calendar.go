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

	"log/slog"

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
	TimeZones []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"timezones"`
}

func (c *CalendarConfig) Process(wg *errgroup.Group) (err error) {

	if !c.Enabled {
		return nil
	}

	resp, err := http.Get(c.IcsUrl)
	if err != nil {
		return errors.Wrap(err, "Failed in http.Get")
	}

	var tzMapping = map[string]string{}

	for _, tz := range c.TimeZones {
		tzMapping[tz.From] = tz.To
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

	calendarEvents := cal.Events

	slices.SortStableFunc(calendarEvents, func(a, b gocal.Event) int {
		funcs := [](func() int){
			func() int { return a.Start.Compare(*b.Start) },
			func() int { return a.End.Compare(*b.End) },
			func() int { return strings.Compare(a.Summary, b.Summary) },
		}
		for _, f := range funcs {
			comp := f()
			if comp != 0 {
				return comp
			}
		}
		return 0
	})

	days := map[string][]string{}

	dateFormat := "2006_01_02"

	for _, e := range calendarEvents {

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

		baseId := e.Uid

		recurranceId := *e.Start

		if e.RecurrenceID != "" {
			recurranceId, err = time.Parse("20060102T150405", e.RecurrenceID)
			if err != nil {
				return errors.Wrap(err, "Failed in time.Parse")
			}
		}

		slog.Debug(e.Summary + " - " + baseId + " " + recurranceId.Format("20060102T150405"))

		text = append(text,
			"  SCHEDULED: <"+e.Start.Local().Format("2006-01-02 Mon 15:04")+">",
			"  status:: "+e.Status,
			"  id:: "+deterministicGUID(baseId+recurranceId.Format("20060102T150405")),
			"  :AGENDA:",
			"  estimated: "+strconv.Itoa(
				durationMinutes,
			)+"m",
			"  :END:",
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
