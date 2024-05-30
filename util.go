package main

import (
	"regexp"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
)

func SearchAndReplace(
	src string,
	reg []struct {
		matcher, repl string
	},
) string {
	for _, l := range reg {
		src = regexp.MustCompile(l.matcher).ReplaceAllString(src, l.repl)
	}
	return src
}

func DateFormat(input time.Time) string {
	return input.Format("Jan") + " " + humanize.Ordinal(input.Day()) + ", " + input.Format("2006")
}

type stackTracer interface {
	StackTrace() errors.StackTrace
}
