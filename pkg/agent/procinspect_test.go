package agent

import (
	"os"
	"os/exec"
	"testing"
)

func TestProcessInspector_StartTokenStableForSameProcess(t *testing.T) {
	in := newProcessInspector()
	a, err := in.StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self): %v", err)
	}
	b, err := in.StartToken(os.Getpid())
	if err != nil {
		t.Fatalf("StartToken(self) again: %v", err)
	}
	if a == "" || a != b {
		t.Fatalf("tokens %q and %q, want one non-empty stable token", a, b)
	}
}

func TestProcessInspector_StartTokenDiffersForAnotherProcess(t *testing.T) {
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	in := newProcessInspector()
	self, err := in.StartToken(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	child, err := in.StartToken(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("StartToken(child): %v", err)
	}
	if child == "" || child == self {
		t.Fatalf("child token %q, self %q: want a different non-empty token", child, self)
	}
}

func TestProcessInspector_StartTokenErrorsForDeadPID(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := newProcessInspector().StartToken(cmd.Process.Pid); err == nil {
		t.Fatal("StartToken on a reaped PID returned no error")
	}
}
