package agent

import (
	"strings"
)

// writingCommands is the set of command words that, when they are the first
// token of a pipeline segment, mutate the workspace. This is the precise
// replacement for the old fileWritingBashTokens substring match.
var writingCommands = map[string]bool{
	"tee":      true,
	"mv":       true,
	"cp":       true,
	"patch":    true,
	"dd":       true,
	"truncate": true,
	"install":  true,
	// "sed" and "git" are handled specially below because they only write
	// under specific conditions (sed with -i, git with the apply subcommand).
}

// nonWritingRedirectionTargets are redirection targets that do not count as a
// workspace write.
var nonWritingRedirectionTargets = map[string]bool{
	"/dev/null":   true,
	"/dev/stdout": true,
	"/dev/stderr": true,
}

// bashWritesWorkspace decides whether a bash command string writes to the
// workspace. It decides on the COMMAND WORD of each pipeline segment, not on
// a substring of the whole string.
//
// The command is split on the top-level shell separators ; && || and |, and
// each segment is evaluated independently. Any segment that writes makes the
// whole command true.
//
// A segment writes when its command word is one of: sed (with -i), tee, mv,
// cp, patch, dd, truncate, install, or when it is `git apply`. A segment also
// writes when it carries an output redirection ( > or >> ) to anything other
// than /dev/null, /dev/stdout or /dev/stderr.
func bashWritesWorkspace(cmd string) bool {
	for _, seg := range splitCommandSegments(cmd) {
		if segmentWrites(seg) {
			return true
		}
	}
	return false
}

// splitCommandSegments splits a command string on the top-level shell
// separators ; && || and | and returns the trimmed, non-empty segments.
// It does not attempt a full shell parse; it is a conservative split that is
// good enough to attribute a command word to each segment.
func splitCommandSegments(cmd string) []string {
	var segs []string
	var cur strings.Builder
	i := 0
	for i < len(cmd) {
		c := cmd[i]
		switch c {
		case ';':
			appendSegment(&segs, &cur)
			i++
		case '&':
			if i+1 < len(cmd) && cmd[i+1] == '&' {
				appendSegment(&segs, &cur)
				i += 2
			} else {
				cur.WriteByte(c)
				i++
			}
		case '|':
			if i+1 < len(cmd) && cmd[i+1] == '|' {
				appendSegment(&segs, &cur)
				i += 2
			} else {
				appendSegment(&segs, &cur)
				i++
			}
		default:
			cur.WriteByte(c)
			i++
		}
	}
	appendSegment(&segs, &cur)
	return segs
}

func appendSegment(segs *[]string, cur *strings.Builder) {
	s := strings.TrimSpace(cur.String())
	if s != "" {
		*segs = append(*segs, s)
	}
	cur.Reset()
}

// segmentWrites reports whether a single pipeline segment writes to the
// workspace.
func segmentWrites(seg string) bool {
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return false
	}
	// Resolve the real command word, stepping over leading VAR=... assignments
	// and any `env` prefix (e.g. `env FOO=bar sed -i ...`).
	idx := commandWordIndex(fields)
	if idx < 0 {
		// No command word at all (a bare `env`, or only assignments). A
		// redirection is still a write.
		return hasWritingRedirection(seg)
	}
	// Use the base name of the command word so /usr/bin/sed still counts.
	base := commandBaseName(fields[idx])
	args := fields[idx:]

	// Decide on the command word, then FALL THROUGH to the redirection check.
	// The sed and git arms must not return early: a segment whose command word
	// does not itself write can still write by redirecting its output into a
	// file (`git diff > patch.diff`, `sed -n p f.go > out.go`). Returning here
	// made hasWritingRedirection unreachable for exactly those two words.
	writes := false
	switch base {
	case "sed":
		writes = hasInPlaceFlag(args)
	case "git":
		writes = isGitApply(args)
	default:
		writes = writingCommands[base]
	}
	if writes {
		return true
	}
	// Any output redirection to a real file makes the segment a write.
	return hasWritingRedirection(seg)
}

// commandWordIndex returns the index of the real command word in fields,
// stepping over leading VAR=value assignments and any `env` prefix together
// with env's own options, so `env FOO=bar sed -i f.go` decides on sed rather
// than on env. It returns -1 when there is no command word, e.g. a bare `env`
// or a segment that is only assignments.
func commandWordIndex(fields []string) int {
	i := 0
	for {
		for i < len(fields) && isEnvAssignment(fields[i]) {
			i++
		}
		if i >= len(fields) {
			return -1
		}
		if commandBaseName(fields[i]) != "env" {
			return i
		}
		// Step over `env` itself and its own options. -u and -C take a
		// separate argument, so they consume one extra field.
		i++
		for i < len(fields) && strings.HasPrefix(fields[i], "-") {
			if fields[i] == "--" {
				i++
				break
			}
			if fields[i] == "-u" || fields[i] == "-C" {
				i++
			}
			i++
		}
	}
}

// commandBaseName returns the final path component of a command word.
func commandBaseName(word string) string {
	if idx := strings.LastIndex(word, "/"); idx >= 0 {
		return word[idx+1:]
	}
	return word
}

// isEnvAssignment reports whether a field is a VAR=... assignment, which is
// not the command word.
func isEnvAssignment(f string) bool {
	eq := strings.Index(f, "=")
	return eq > 0 && f[eq-1] >= 'A' && (f[eq-1] <= 'Z' || f[eq-1] == '_' ||
		(f[eq-1] >= 'a' && f[eq-1] <= 'z'))
}

// hasInPlaceFlag reports whether args carry sed's in-place flag in any of the
// forms coders actually emit: the exact short flag (`-i`), the backup-suffix
// form (`-i.bak`), a clustered short-flag group containing i (`-ie`, `-ni`),
// or the GNU long flag (`--in-place`, `--in-place=.bak`). args[0] is the
// command word and is not examined.
//
// Matching only `-i` and `-i.` missed the clustered and long forms, which the
// substring predicate this replaced happened to catch for `-ie`.
func hasInPlaceFlag(args []string) bool {
	for _, f := range args[1:] {
		if strings.HasPrefix(f, "--") {
			name := strings.TrimPrefix(f, "--")
			if eq := strings.Index(name, "="); eq >= 0 {
				name = name[:eq]
			}
			if name == "in-place" {
				return true
			}
			continue
		}
		if !strings.HasPrefix(f, "-") || f == "-" {
			continue
		}
		// A short-flag group, up to any inline suffix such as -i.bak.
		group := f[1:]
		if dot := strings.Index(group, "."); dot >= 0 {
			group = group[:dot]
		}
		if strings.ContainsRune(group, 'i') {
			return true
		}
	}
	return false
}

// isGitApply reports whether the fields form `git apply ...`.
func isGitApply(fields []string) bool {
	for i, f := range fields {
		if f == "apply" {
			return i > 0
		}
	}
	return false
}

// hasWritingRedirection reports whether the segment contains an output
// redirection (`>` or `>>`) whose target is a real file, i.e. not
// /dev/null, /dev/stdout, /dev/stderr, and not a file-descriptor
// duplication such as `>&1` or `2>&1`. It scans the raw segment so targets
// containing spaces are still caught; the first word after the operator is the
// target.
func hasWritingRedirection(seg string) bool {
	for i := 0; i < len(seg); i++ {
		if seg[i] != '>' {
			continue
		}
		j := i + 1
		for j < len(seg) && seg[j] == '>' { // collapse '>>'
			j++
		}
		for j < len(seg) && (seg[j] == ' ' || seg[j] == '\t') {
			j++
		}
		if j >= len(seg) || seg[j] == '&' {
			// End of string or an fd duplication like '>&2'; not a file write.
			continue
		}
		k := j
		for k < len(seg) && seg[k] != ' ' && seg[k] != '\t' &&
			seg[k] != ';' && seg[k] != '|' && seg[k] != '&' {
			k++
		}
		if _, ok := nonWritingRedirectionTargets[seg[j:k]]; !ok {
			return true
		}
		i = j - 1
	}
	return false
}
