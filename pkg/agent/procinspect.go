package agent

import "slices"

// processInspector reads kernel facts about a process that identify it beyond
// its PID. Engine records store the start token so an agent that restarts can
// tell its own engine from an unrelated process that later reused the PID
// (#2079).
type processInspector interface {
	// StartToken returns an opaque value that is equal for the same process
	// across calls and different for any other process that reuses the PID.
	// It fails when no live process has the PID.
	StartToken(pid int) (string, error)
	// Executables returns the candidate paths of the running program, each
	// symlink-resolved and de-duplicated: the kernel exec path, argv[0], and
	// argv[1] (the script when the program is a #! interpreter). A match
	// against any candidate is a match.
	Executables(pid int) ([]string, error)
}

// executableMatches reports whether recorded is one of the running program's
// candidate paths. An empty record never matches.
func executableMatches(candidates []string, recorded string) bool {
	return recorded != "" && slices.Contains(candidates, recorded)
}
