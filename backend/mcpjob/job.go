// Package mcpjob implements the unified async job interface that flattens the
// MCP server's two command-execution modes (background non-PTY exec and
// foreground live-PTY typing) behind one contract: every launch returns a
// {job_id}, and the agent polls uniterm_exec_result(job_id) for completion.
//
// The background mode already ran over SSH exec channels (backend/session
// mcp_exec.go); this package owns the FOREGROUND mode — type a command into the
// live terminal and complete on an idle-settle event or explicit EOF, never on
// an injected end-marker that can fail to match and hang. The registry also
// normalizes output accumulation, truncation and the shared result schema both
// modes return to the agent.
package mcpjob

import (
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Job states shared by both modes. The MCP tool layer maps these onto the
// agent-facing state strings.
const (
	StateRunning       = "running"
	StateExited        = "exited"
	StateError         = "error"
	StateKilled        = "killed"
	StateTimeout       = "timeout"
	StateSessionClosed = "session_closed"
)

// Foreground completion events delivered by the tap.
const (
	eventIdleSettled = "idle"
	eventEOF         = "eof"
	eventHardTimeout = "timeout"
	eventInterrupted = "killed"
)

// Default caps; the MCP layer can override per call.
const (
	DefaultIdleSettle    = 1200 * time.Millisecond
	DefaultHardLimit     = 5 * time.Minute
	DefaultMaxOutput     = 64 * 1024
	defaultOutputCap     = DefaultMaxOutput * 4
	defaultJobTTL        = 10 * time.Minute
	defaultMaxConcurrent = 8
)

// Job is one foreground execution lifecycle. Background jobs are tracked by the
// session package's own registry; this struct models the foreground path.
type Job struct {
	ID        string
	SessionID string

	mu       sync.Mutex
	stdout   []byte
	stderr   []byte
	exitCode int
	state    string
	err      error

	done     chan struct{}
	finished bool
}

// Snapshot returns a consistent copy of the current output/state.
func (j *Job) Snapshot() (stdout, stderr []byte, exitCode int, state string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]byte, len(j.stdout))
	copy(out, j.stdout)
	errOut := make([]byte, len(j.stderr))
	copy(errOut, j.stderr)
	return out, errOut, j.exitCode, j.state, j.err
}

func (j *Job) setState(state string, exitCode int, err error) {
	j.mu.Lock()
	if j.finished {
		j.mu.Unlock()
		return
	}
	j.state = state
	j.exitCode = exitCode
	j.err = err
	j.finished = state != StateRunning
	if j.finished {
		close(j.done)
	}
	j.mu.Unlock()
}

func (j *Job) appendLocked(dst *[]byte, b []byte) {
	if len(*dst)+len(b) > defaultOutputCap {
		drop := len(*dst) + len(b) - defaultOutputCap
		if drop >= len(b) {
			*dst = append((*dst)[:0], b...)
		} else {
			*dst = append((*dst)[drop:], b...)
		}
	} else {
		*dst = append(*dst, b...)
	}
}

// Pty is the small live-session surface the foreground tap needs, implemented
// by the session layer (Write + data/status callbacks). Kept narrow so the
// registry tests against a fake.
type Pty interface {
	// Write sends bytes (the command + newline) to the live terminal.
	Write(data []byte) error
	// OnData registers a stream listener; returns an unsubscribe function.
	OnData(cb func(data []byte)) (off func())
	// OnStatusChange registers a connection-lost listener; returns unsub.
	OnStatusChange(cb func(status string)) (off func())

	// Alive reports whether the session is still connected at tap start.
	Alive() bool
}

// Registry tracks foreground jobs across all live sessions.
type Registry struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	running int
	// newID is injectable for tests (deterministic ids); default uuid.
	newID func() string
}

// NewRegistry builds an empty registry with the uuid id generator.
func NewRegistry() *Registry {
	return &Registry{
		jobs:  make(map[string]*Job),
		newID: func() string { return "fg-" + uuid.New().String() },
	}
}

// Start types command into pty and watches for completion. It returns the job
// immediately; the caller polls Snapshot. Completion is driven by the idle
// timer (no output for idleSettle) or an explicit EOF (session closed) — never
// by matching an end marker. A hard deadline moves the job to StateTimeout; it
// stays pollable and interruptible instead of hanging the caller.
func (r *Registry) Start(pty Pty, sessionID, command, newline string, idleSettle, hardLimit time.Duration, maxOutput int) (*Job, error) {
	if idleSettle <= 0 {
		idleSettle = DefaultIdleSettle
	}
	if hardLimit <= 0 {
		hardLimit = DefaultHardLimit
	}
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutput
	}

	r.mu.Lock()
	if r.running >= defaultMaxConcurrent {
		r.mu.Unlock()
		return nil, ErrTooMany
	}
	r.running++
	r.mu.Unlock()

	job := &Job{
		ID:        r.newID(),
		SessionID: sessionID,
		state:     StateRunning,
		done:      make(chan struct{}),
	}
	r.mu.Lock()
	r.jobs[job.ID] = job
	r.mu.Unlock()
	r.scheduleReaper(job.ID)

	if !pty.Alive() {
		r.finish(job, StateSessionClosed, -1, ErrSessionClosed)
		r.releaseSlot()
		return job, nil
	}

	if err := pty.Write([]byte(command + newline)); err != nil {
		r.finish(job, StateError, -1, err)
		r.releaseSlot()
		return job, nil
	}

	go r.watch(pty, job, idleSettle, hardLimit, maxOutput)
	return job, nil
}

func (r *Registry) watch(pty Pty, job *Job, idleSettle, hardLimit time.Duration, maxOutput int) {
	defer r.releaseSlot()

	var (
		eof      bool
		lastData = time.Now()
		start    = time.Now()
	)
	dataOff := pty.OnData(func(data []byte) {
		job.mu.Lock()
		job.appendLocked(&job.stdout, data)
		job.mu.Unlock()
		lastData = time.Now()
	})
	statusOff := pty.OnStatusChange(func(status string) {
		if status == "disconnected" || status == "error" {
			eof = true
		}
	})
	defer func() {
		if dataOff != nil {
			dataOff()
		}
		if statusOff != nil {
			statusOff()
		}
	}()

	// Poll on a short ticker: check EOF, the idle-settle deadline and the hard
	// limit. Completion is an event (idle gap / session close), never a marker
	// match, so a dynamic prompt or TUI cannot make the watcher hang.
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-job.done:
			return
		case now := <-ticker.C:
			if eof {
				r.finish(job, StateSessionClosed, -1, ErrSessionClosed)
				return
			}
			if now.Sub(start) >= hardLimit {
				r.finish(job, StateTimeout, -1, nil)
				return
			}
			if now.Sub(lastData) >= idleSettle {
				r.finish(job, StateExited, 0, nil)
				return
			}
		}
	}
}

// Interrupt stops a running job: foreground taps just close the job (the typed
// command keeps running in the terminal, but the watcher stops collecting).
func (r *Registry) Interrupt(jobID string) error {
	r.mu.Lock()
	job, ok := r.jobs[jobID]
	r.mu.Unlock()
	if !ok {
		return ErrUnknownJob
	}
	job.mu.Lock()
	running := job.state == StateRunning
	job.mu.Unlock()
	if running {
		r.finish(job, StateKilled, -1, ErrInterrupted)
	}
	return nil
}

// Lookup returns the job for polling.
func (r *Registry) Lookup(jobID string) (*Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[jobID]
	return job, ok
}

func (r *Registry) finish(job *Job, state string, exitCode int, err error) {
	job.setState(state, exitCode, err)
}

func (r *Registry) releaseSlot() {
	r.mu.Lock()
	if r.running > 0 {
		r.running--
	}
	r.mu.Unlock()
}

func (r *Registry) scheduleReaper(id string) {
	time.AfterFunc(defaultJobTTL, func() {
		r.mu.Lock()
		if job, ok := r.jobs[id]; ok {
			job.mu.Lock()
			done := job.finished
			job.mu.Unlock()
			if done {
				delete(r.jobs, id)
			}
		}
		r.mu.Unlock()
	})
}

// Truncate caps a byte slice at max, cutting at a newline when possible and
// appending a marker. Shared by both modes' result shaping.
func Truncate(b []byte, max int) []byte {
	if max <= 0 || len(b) <= max {
		return b
	}
	const marker = "\n…[uniterm: output truncated]"
	trunc := b[:max]
	if i := strings.LastIndexByte(string(trunc), '\n'); i > max/2 {
		trunc = trunc[:i]
	}
	return append(trunc, []byte(marker)...)
}

// Result is the normalized poll payload both modes return.
type Result struct {
	JobID      string
	State      string
	Stdout     string
	Stderr     string
	ExitCode   int
	NextOffset int
	Truncated  bool
}
