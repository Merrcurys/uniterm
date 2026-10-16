package mcpjob

import (
	"strings"
	"testing"
	"time"
)

// fakePty is a scriptable Pty for tests: it replays queued output chunks on
// data listeners and can be marked disconnected.
type fakePty struct {
	alive        bool
	disconnected bool
	wrote        []string
	dataCbs      []func([]byte)
	statusCbs    []func(string)
}

func (f *fakePty) Write(data []byte) error {
	f.wrote = append(f.wrote, string(data))
	return nil
}
func (f *fakePty) OnData(cb func([]byte)) func() {
	f.dataCbs = append(f.dataCbs, cb)
	return func() {}
}
func (f *fakePty) OnStatusChange(cb func(string)) func() {
	f.statusCbs = append(f.statusCbs, cb)
	if f.disconnected {
		cb("disconnected")
	}
	return func() {}
}
func (f *fakePty) Alive() bool { return f.alive }
func (f *fakePty) emit(data string) {
	for _, cb := range f.dataCbs {
		cb([]byte(data))
	}
}
func (f *fakePty) disconnect() {
	f.disconnected = true
	for _, cb := range f.statusCbs {
		cb("disconnected")
	}
}

func TestForegroundCompletesOnIdleSettle(t *testing.T) {
	reg := NewRegistry()
	pty := &fakePty{alive: true}
	job, err := reg.Start(pty, "s1", "ls -la", "\n", 60*time.Millisecond, 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	pty.emit("total 12\n")
	pty.emit("a.txt\n")

	waitDone(t, job, 500*time.Millisecond)
	_, _, code, state, rerr := job.Snapshot()
	if state != StateExited {
		t.Fatalf("state=%s want exited", state)
	}
	if rerr != nil {
		t.Fatalf("err=%v", rerr)
	}
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(pty.wrote[0], "ls -la") {
		t.Fatalf("command not typed: %q", pty.wrote)
	}
}

func TestForegroundEOFOnDisconnect(t *testing.T) {
	reg := NewRegistry()
	pty := &fakePty{alive: true}
	job, _ := reg.Start(pty, "s1", "cat big.log", "\n", 2*time.Second, 5*time.Second, 0)
	pty.emit("partial line\n")
	pty.disconnect()

	waitDone(t, job, 500*time.Millisecond)
	_, _, _, state, rerr := job.Snapshot()
	if state != StateSessionClosed {
		t.Fatalf("state=%s want session_closed", state)
	}
	if rerr != ErrSessionClosed {
		t.Fatalf("err=%v", rerr)
	}
}

func TestForegroundHardTimeoutKeepsPollable(t *testing.T) {
	reg := NewRegistry()
	pty := &fakePty{alive: true}
	// Very short hard limit so the job moves to timeout without hanging.
	job, _ := reg.Start(pty, "s1", "sleep 3600", "\n", 2*time.Second, 40*time.Millisecond, 0)
	pty.emit("running...\n")

	waitDone(t, job, 500*time.Millisecond)
	_, _, _, state, _ := job.Snapshot()
	if state != StateTimeout {
		t.Fatalf("state=%s want timeout", state)
	}
	// Still interruptible after timeout.
	if err := reg.Interrupt(job.ID); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptKillsRunning(t *testing.T) {
	reg := NewRegistry()
	pty := &fakePty{alive: true}
	job, _ := reg.Start(pty, "s1", "top", "\n", 2*time.Second, 5*time.Second, 0)
	if err := reg.Interrupt(job.ID); err != nil {
		t.Fatal(err)
	}
	waitDone(t, job, 200*time.Millisecond)
	_, _, _, state, _ := job.Snapshot()
	if state != StateKilled {
		t.Fatalf("state=%s want killed", state)
	}
}

func TestStartOnDeadSession(t *testing.T) {
	reg := NewRegistry()
	pty := &fakePty{alive: false}
	job, _ := reg.Start(pty, "s1", "ls", "\n", time.Second, time.Second, 0)
	waitDone(t, job, 200*time.Millisecond)
	_, _, _, state, _ := job.Snapshot()
	if state != StateSessionClosed {
		t.Fatalf("state=%s", state)
	}
}

func TestTruncateCutsAtNewline(t *testing.T) {
	in := []byte("aaaa\nbbbb\ncccc\ndddd")
	out := Truncate(in, 12)
	if !strings.Contains(string(out), "truncated") {
		t.Fatalf("missing marker: %q", out)
	}
}

func waitDone(t *testing.T, job *Job, within time.Duration) {
	t.Helper()
	select {
	case <-job.done:
	case <-time.After(within):
		t.Fatal("job did not finish within", within)
	}
}
