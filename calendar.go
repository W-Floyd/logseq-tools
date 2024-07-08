package main

import (
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	ics "github.com/arran4/golang-ical"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

type CalendarConfig struct {
	Enabled    bool   `json:"enabled"`
	Title      string `json:"title"`
	IcsUrl     string `json:"ics_url"`
	UpdatePast bool   `json:"update_past"`
}

func (c *CalendarConfig) Process(wg *errgroup.Group) (err error) {

	if !c.Enabled {
		return nil
	}

	resp, err := http.Get(c.IcsUrl)
	if err != nil {
		return errors.Wrap(err, "Failed in http.Get")
	}

	cal, err := ics.ParseCalendar(resp.Body)

	events := cal.Events()

	slices.SortFunc(events, func(a, b *ics.VEvent) int {
		aVal, err := a.GetStartAt()
		if err != nil {
			return 0
		}
		bVal, err := b.GetStartAt()
		if err != nil {
			return 0
		}
		comp := aVal.Compare(bVal)
		if comp != 0 {
			return comp
		}

		aVal, err = a.GetEndAt()
		if err != nil {
			return 0
		}
		bVal, err = b.GetEndAt()
		if err != nil {
			return 0
		}
		return aVal.Compare(bVal)
	})

	days := map[string][]string{}

	dateFormat := "2006_01_02"

	for _, e := range events {
		start, err := e.GetStartAt()
		if err != nil {
			return errors.Wrap(err, "Failed in GetStartAt")
		}
		page := start.Format(dateFormat)

		text := []string{}

		summary := e.GetProperty(ics.ComponentProperty("SUMMARY"))

		text = append(text, summary.Value)

		days[page] = append(days[page], text...)

	}

	for k, d := range days {
		if time.Now().Format(dateFormat) >= k || c.UpdatePast {
			err = WriteFile(
				path.Join(
					config.LogseqRoot,
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
