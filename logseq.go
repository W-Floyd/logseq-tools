package main

import "regexp"

func PageNameToFileName(pagename string) (filename string) {
	return regexp.MustCompile("/").ReplaceAllString(pagename, "___")
}
