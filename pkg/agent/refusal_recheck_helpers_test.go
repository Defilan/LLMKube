/*
Copyright 2026.

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

package agent

import (
	"testing"
	"time"
)

// shortenRefusalRecheck makes refusal re-checks run within the poll-loop
// tests' timeout: 50ms doubling to 200ms instead of 5s doubling to a minute.
func shortenRefusalRecheck(t *testing.T) {
	t.Helper()
	initial, maxWait := refusalRecheckInitial, refusalRecheckMax
	refusalRecheckInitial, refusalRecheckMax = 50*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { refusalRecheckInitial, refusalRecheckMax = initial, maxWait })
}
