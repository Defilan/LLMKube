package grounding

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// AddedLine is one added line attributed to its new-file path and line number.
type AddedLine struct {
	File string
	Line int
	Text string
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// AddedLines returns the added (+) lines from `git diff --unified=0
// base...HEAD -- pathspec...`, each attributed to its new-file line number.
// Header lines (+++), removed lines, and context are ignored. A git error is
// returned; an empty diff yields nil with no error.
func AddedLines(
	ctx context.Context, workspace string, run CommandRunner, base string, pathspec []string,
) ([]AddedLine, error) {
	args := append([]string{"diff", "--unified=0", base + "...HEAD", "--"}, pathspec...)
	out, err := run(ctx, workspace, nil, "git", args...)
	if err != nil {
		return nil, err
	}
	var added []AddedLine
	var curFile string
	var curLine int
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ b/"):
			curFile = strings.TrimPrefix(line, "+++ b/")
		case strings.HasPrefix(line, "+++ "): // /dev/null etc.
			curFile = ""
		case strings.HasPrefix(line, "@@"):
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				curLine, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(line, "+") && curFile != "":
			added = append(added, AddedLine{File: curFile, Line: curLine, Text: strings.TrimPrefix(line, "+")})
			curLine++
		}
	}
	return added, nil
}
