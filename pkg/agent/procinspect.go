package agent

// processInspector reads kernel facts about a process that identify it beyond
// its PID. Engine records store the start token so an agent that restarts can
// tell its own engine from an unrelated process that later reused the PID
// (#2079).
type processInspector interface {
	// StartToken returns an opaque value that is equal for the same process
	// across calls and different for any other process that reuses the PID.
	// It fails when no live process has the PID.
	StartToken(pid int) (string, error)
}
