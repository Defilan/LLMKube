package agent

import "testing"

// TestBashWritesWorkspace is the regression test for #1520: the old
// fileWritingBashTokens substring match reset the edit-free streak on any
// command *containing* a writing token, so `go install ./...`, `npm install`,
// and `pip install` all cleared the counter and made editFreeTurnsLimit
// unreachable. The new predicate decides on the COMMAND WORD of each pipeline
// segment, so package-manager `install` is not a workspace write.
func TestBashWritesWorkspace(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		// THE BUG: these matched "install " as a substring before.
		{"go install ./...", false},
		{"go test ./...", false},
		{"npm install", false},
		{"pip install foo", false},
		// coreutils `install` as a command word IS a write.
		{"install -m 0644 a b", true},
		// substring trap: the word "install" inside another argument.
		{"grep -r install .", false},
		// sed writes only with -i.
		{"sed -i 's/a/b/' f.go", true},
		{"sed -n '1,5p' f.go", false},
		// output redirection to a real file writes; to /dev/null does not.
		{"echo hi > f.txt", true},
		{"echo hi > /dev/null", false},
		// plain reads never write.
		{"cat f.go", false},
		{"ls -l && sed -i s/a/b/ x", true}, // second segment writes
		// git apply modifies source files; git status does not.
		{"git apply p.patch", true},
		{"git status", false},
		// mv / cp are writes.
		{"mv a b", true},
		{"cp a b", true},
		// empty command: false, and no panic.
		{"", false},
	}
	for _, c := range cases {
		if got := bashWritesWorkspace(c.cmd); got != c.want {
			t.Errorf("bashWritesWorkspace(%q)=%v want %v", c.cmd, got, c.want)
		}
	}
}

// TestBashWritesWorkspace_RedirectionSurvivesCommandWordDispatch guards the
// regression found in review of #1520: dispatching on the command word must not
// swallow output redirection. The predicate this replaced checked its token
// list and then fell through to a redirect check on the whole string, so a
// redirect counted regardless of the command word. `sed` and `git` are the two
// words with their own arms, so they are the two that can lose the fallthrough.
//
// A false negative here force-terminates a model that is legitimately editing
// through the shell (see the editFreeStreak note in progress.go, #982, #896),
// so these must stay true.
func TestBashWritesWorkspace_RedirectionSurvivesCommandWordDispatch(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"sed 's/a/b/' in.go > out.go", true},
		{"sed -n 'p' f.go >> log.txt", true},
		{"git diff > /tmp/patch.diff", true},
		{"git show HEAD:f.go > f.go", true},
		// redirection to a discard target is still not a write.
		{"sed -n 'p' f.go > /dev/null", false},
		{"git diff > /dev/null", false},
		// no redirection, no write.
		{"sed -n 'p' f.go", false},
		{"git diff", false},
	}
	for _, c := range cases {
		if got := bashWritesWorkspace(c.cmd); got != c.want {
			t.Errorf("bashWritesWorkspace(%q)=%v want %v", c.cmd, got, c.want)
		}
	}
}

// TestBashWritesWorkspace_EnvPrefixResolvesRealCommandWord covers `env`, which
// is not a VAR=value assignment and so terminated the word-resolution loop
// before the real command word was reached.
func TestBashWritesWorkspace_EnvPrefixResolvesRealCommandWord(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"env FOO=bar sed -i 's/a/b/' f.go", true},
		{"env sed -i 's/a/b/' f.go", true},
		{"env -u HOME sed -i 's/a/b/' f.go", true},
		{"env FOO=bar mv a b", true},
		// env fronting a read-only command is still not a write.
		{"env FOO=bar go test ./...", false},
		{"env FOO=bar sed -n 'p' f.go", false},
		// bare env is not a write and must not panic.
		{"env", false},
	}
	for _, c := range cases {
		if got := bashWritesWorkspace(c.cmd); got != c.want {
			t.Errorf("bashWritesWorkspace(%q)=%v want %v", c.cmd, got, c.want)
		}
	}
}

// TestBashWritesWorkspace_InPlaceFlagForms covers the sed in-place flag in the
// forms real coders emit: clustered short flags and the GNU long flag. Only the
// exact `-i` and the `-i.bak` backup form were recognised.
func TestBashWritesWorkspace_InPlaceFlagForms(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"sed -i 's/a/b/' f.go", true},
		{"sed -i.bak 's/a/b/' f.go", true},
		{"sed -ie 's/a/b/' f.go", true},
		{"sed -ni 's/a/b/p' f.go", true},
		{"sed -in 's/a/b/' f.go", true},
		{"sed --in-place 's/a/b/' f.go", true},
		{"sed --in-place=.bak 's/a/b/' f.go", true},
		// short-flag clusters with no i are not in-place.
		{"sed -n 'p' f.go", false},
		{"sed -En 's/a/b/p' f.go", false},
		// a long flag that merely starts with the same letters is not -i.
		{"sed --include 's/a/b/' f.go", false},
	}
	for _, c := range cases {
		if got := bashWritesWorkspace(c.cmd); got != c.want {
			t.Errorf("bashWritesWorkspace(%q)=%v want %v", c.cmd, got, c.want)
		}
	}
}
