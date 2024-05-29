package main

import (
	"regexp"

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

type stackTracer interface {
	StackTrace() errors.StackTrace
}
