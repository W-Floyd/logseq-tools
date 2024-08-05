package main

import (
	"regexp"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/zeebo/xxh3"
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

// Adapted from https://gist.github.com/PaulBradley/08598aa755a6845f46691ab363ddf7f6?permalink_comment_id=4684711#gistcomment-4684711
func deterministicGUID(input string) string {
	h := xxh3.HashString128(input).Bytes()
	guid, _ := uuid.FromBytes(h[:])
	return guid.String()
}
