package mcp

import (
	"context"
	"testing"
)

// jobFakeExec implements SSHExecutor for the background path with scriptable
// output / async handle.
type jobFakeExec struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	cmdID    string
}

func (f *jobFakeExec) MCPExec(cmd string, syncTimeoutMs, hardTimeoutMs, maxOutput int) ([]byte, []byte, int, string, error) {
	return f.stdout, f.stderr, f.exitCode, f.cmdID, nil
}
func (f *jobFakeExec) MCPLatestOutput(commandID string, offset, max int) ([]byte, []byte, int, string, error) {
	out := f.stdout
	if offset > 0 && offset < len(out) {
		out = out[offset:]
	}
	return out, f.stderr, f.exitCode, "exited", nil
}
func (f *jobFakeExec) MCPInterrupt(commandID string, signal string) error { return nil }

// fakeForeground implements ForegroundJobs.
type fakeForeground struct {
	started  string
	polled   string
	interrupted string
}

func (f *fakeForeground) StartForeground(sessionID, command string, idleMs, hardLimitMs int) (string, error) {
	f.started = command
	return "fg-test-1", nil
}
func (f *fakeForeground) PollForeground(jobID string, offset, max int) ([]byte, []byte, int, string, error) {
	f.polled = jobID
	return []byte("hello world\n"), nil, 0, "exited", nil
}
func (f *fakeForeground) InterruptForeground(jobID, signal string) error {
	f.interrupted = jobID
	return nil
}

func newJobTestServer(fg ForegroundJobs, exec SSHExecutor) *Server {
	return &Server{env: Env{
		Sessions:   func(string) (SSHExecutor, bool) { return exec, true },
		Commands:   func(string) (string, SSHExecutor, bool) { return "sid", exec, true },
		Policy:     func() Policy { return PolicyBypass },
		Foreground: fg,
	}, finishedBg: map[string]finishedBackground{}}
}

func TestUnitermExecBackgroundSyncCached(t *testing.T) {
	exec := &jobFakeExec{stdout: []byte("output line\n"), exitCode: 0}
	srv := newJobTestServer(nil, exec)

	_, out, err := srv.toolUnitermExec(context.Background(), nil, unitermExecIn{
		SessionID: "s1", Command: "ls", Mode: ModeBackground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Mode != ModeBackground || out.State != "exited" {
		t.Fatalf("out=%+v", out)
	}
	if out.JobID == "" || out.Stdout != "output line\n" {
		t.Fatalf("out=%+v", out)
	}

	// Poll through the unified result tool: the sync result must be cached.
	_, result, err := srv.toolUnitermResult(context.Background(), nil, unitermResultIn{JobID: out.JobID})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "exited" || result.Stdout != "output line\n" {
		t.Fatalf("result=%+v", result)
	}
}

func TestUnitermExecBackgroundAsyncCommandID(t *testing.T) {
	exec := &jobFakeExec{cmdID: "ssh-cmd-9", exitCode: -1}
	srv := newJobTestServer(nil, exec)
	_, out, err := srv.toolUnitermExec(context.Background(), nil, unitermExecIn{SessionID: "s1", Command: "sleep 30", Mode: ModeBackground})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Async || out.JobID != "ssh-cmd-9" || out.State != "running" {
		t.Fatalf("out=%+v", out)
	}
	_, result, err := srv.toolUnitermResult(context.Background(), nil, unitermResultIn{JobID: "ssh-cmd-9"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != ModeBackground || result.State != "exited" {
		t.Fatalf("result=%+v", result)
	}
}

func TestUnitermExecForegroundRoutesToTap(t *testing.T) {
	fg := &fakeForeground{}
	srv := newJobTestServer(fg, &jobFakeExec{})

	_, out, err := srv.toolUnitermExec(context.Background(), nil, unitermExecIn{
		SessionID: "s1", Command: "top", Mode: ModeForeground, IdleMs: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.JobID != "fg-test-1" || !out.Async || out.Mode != ModeForeground {
		t.Fatalf("out=%+v", out)
	}
	if fg.started != "top" {
		t.Fatalf("foreground not started: %q", fg.started)
	}

	_, result, err := srv.toolUnitermResult(context.Background(), nil, unitermResultIn{JobID: "fg-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Mode != ModeForeground || result.Stdout != "hello world\n" {
		t.Fatalf("result=%+v", result)
	}
	if fg.polled != "fg-test-1" {
		t.Fatalf("not routed to foreground poller")
	}

	_, _, err = srv.toolUnitermInterrupt(context.Background(), nil, unitermInterruptIn{JobID: "fg-test-1"})
	if err != nil {
		t.Fatal(err)
	}
	if fg.interrupted != "fg-test-1" {
		t.Fatal("foreground interrupt not routed")
	}
}

func TestUnitermExecForegroundUnsupported(t *testing.T) {
	srv := newJobTestServer(nil, &jobFakeExec{})
	_, _, err := srv.toolUnitermExec(context.Background(), nil, unitermExecIn{SessionID: "s1", Command: "x", Mode: ModeForeground})
	if err == nil {
		t.Fatal("expected error for unsupported foreground")
	}
}

func TestUnitermExecBadMode(t *testing.T) {
	srv := newJobTestServer(nil, &jobFakeExec{})
	_, _, err := srv.toolUnitermExec(context.Background(), nil, unitermExecIn{SessionID: "s1", Command: "x", Mode: "weird"})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}
