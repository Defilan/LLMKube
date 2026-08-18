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
