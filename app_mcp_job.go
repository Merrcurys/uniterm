package main

import (
	"errors"
	"time"

	"github.com/ys-ll/uniterm/backend/mcpjob"
	"github.com/ys-ll/uniterm/backend/session"
)

// Unified async-job bridge: implements mcp.ForegroundJobs over the app's live
// session manager and the mcpjob foreground registry. Background (non-PTY SSH
// exec) stays on the existing session.mcpExec path; this file only owns the
// foreground live-PTY tap.

// tapSource is the additive-observer surface baseSession provides; the generic
// Session interface only exposes the single-slot setters.
type tapSource interface {
	AddDataTap(cb func([]byte)) func()
	AddStatusTap(cb func(session.SessionStatus)) func()
}

// ptyAdapter turns a generic session.Session into the narrow mcpjob.Pty
// surface. Data taps are additive (AddDataTap) so the frontend's own stream
// subscription is never replaced.
type ptyAdapter struct {
	s    session.Session
	taps tapSource
}

func (p ptyAdapter) Write(data []byte) error { return p.s.Write(data) }

func (p ptyAdapter) OnData(cb func([]byte)) func() {
	if p.taps != nil {
		return p.taps.AddDataTap(cb)
	}
	return func() {}
}

func (p ptyAdapter) OnStatusChange(cb func(string)) func() {
	if p.taps == nil {
		return func() {}
	}
	return p.taps.AddStatusTap(func(st session.SessionStatus) {
		cb(string(st))
	})
}

func (p ptyAdapter) Alive() bool { return p.s.Status() == session.StatusConnected }

// newlineFor returns the Enter bytes the shell expects. Windows OpenSSH and
// PowerShell need CR; POSIX shells accept LF (the frontend mirrors this).
func newlineFor(s session.Session) string {
	if ssh, ok := s.(*session.SSHSession); ok && ssh.RemoteOS() == "windows-openssh" {
		return "\r"
	}
	return "\n"
}

func (a *App) jobRegistry() *mcpjob.Registry {
	a.jobOnce.Do(func() {
		a.jobs = mcpjob.NewRegistry()
	})
	return a.jobs
}

// StartForeground implements mcp.ForegroundJobs.
func (a *App) StartForeground(sessionID, command string, idleMs, hardLimitMs int) (string, error) {
	if a.sessionManager == nil {
		return "", errors.New("session manager not initialized")
	}
	s, ok := a.sessionManager.Get(sessionID)
	if !ok {
		return "", errors.New("session not found: " + sessionID)
	}
	pty := ptyAdapter{s: s}
	if taps, ok := s.(tapSource); ok {
		pty.taps = taps
	}
	job, err := a.jobRegistry().Start(
		pty, sessionID, command, newlineFor(s),
		time.Duration(idleMs)*time.Millisecond,
		time.Duration(hardLimitMs)*time.Millisecond,
		mcpjob.DefaultMaxOutput,
	)
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// PollForeground implements mcp.ForegroundJobs.
func (a *App) PollForeground(jobID string, offset, max int) (stdout, stderr []byte, exitCode int, state string, err error) {
	job, ok := a.jobRegistry().Lookup(jobID)
	if !ok {
		return nil, nil, -1, "unknown", mcpjob.ErrUnknownJob
	}
	out, errOut, code, st, jerr := job.Snapshot()
	if offset > 0 && offset < len(out) {
		out = out[offset:]
	}
	if max > 0 && len(out) > max {
		out = mcpjob.Truncate(out, max)
	}
	return out, errOut, code, st, jerr
}

// InterruptForeground implements mcp.ForegroundJobs.
func (a *App) InterruptForeground(jobID, _ string) error {
	return a.jobRegistry().Interrupt(jobID)
}
