/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package repo

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCloneGitConfigGlobalForwarding shows GIT_CONFIG_GLOBAL reaches the
// clone subprocess: an insteadOf rule in the pointed-to config rewrites
// the clone URL to the local bare repo, so the clone succeeds only if
// git read the rule (#2013). Code that drops the variable tries to
// fetch example.invalid and fails.
func TestCloneGitConfigGlobalForwarding(t *testing.T) {
	dir := t.TempDir()
	bare := initBareOrigin(t, dir)
	seedOrigin(t, bare)

	config := filepath.Join(dir, "gitconfig-hostile")
	rule := "[url \"" + bare + "\"]\n\tinsteadOf = https://example.invalid/repo.git\n"
	if err := os.WriteFile(config, []byte(rule), 0o600); err != nil {
		t.Fatalf("write hostile gitconfig: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)

	dest := filepath.Join(dir, "dest")
	if err := Clone(context.Background(), CloneOptions{
		RemoteURL: "https://example.invalid/repo.git",
		Dest:      dest,
	}); err != nil {
		t.Fatalf("Clone with insteadOf-rewritten URL: %v", err)
	}

	want := gitOut(t, bare, "rev-parse", "HEAD")
	if head := gitOut(t, dest, "rev-parse", "HEAD"); head != want {
		t.Errorf("clone HEAD = %s, want the bare repo's HEAD %s (the seeded commit)", head, want)
	}
	if subject := gitOut(t, dest, "show", "-s", "--format=%s", "HEAD"); subject != "seed" {
		t.Errorf("clone HEAD subject = %q, want \"seed\" (the seeded commit)", subject)
	}
}
