package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Unified async-job tools: uniterm_exec launches a command in either mode and
// always returns a jobId; uniterm_exec_result polls it; uniterm_interrupt
// stops it. Background (non-PTY SSH exec) and foreground (live-PTY tap) share
// one input/output schema and job lifecycle; switching mode online changes
// only the `mode` argument.
//
// Foreground completion is driven by an idle-settle event or explicit EOF —
// never by matching an injected end marker, which used to hang when the marker
// could not be extracted.

const (
	ModeBackground = "background"
	ModeForeground = "foreground"

	// fgJobPrefix marks a jobId owned by the foreground (PTY) runner so the
	// poll/interrupt dispatch can tell it apart from an SSH exec commandId.
	fgJobPrefix = "fg-"
	// bgCachePrefix marks a synchronously-completed background result cached
	// for uniterm_exec_result.
	bgCachePrefix = "bg-"
)

// finishedTTL bounds how long a sync-completed background result stays cached
// and pollable before reaping.
const finishedTTL = 10 * time.Minute

// finishedBackground is a sync-completed background result cached so the
// unified result tool can read it through the same poll path as async jobs.
type finishedBackground struct {
	stdout    []byte
	stderr    []byte
	exitCode  int
	state     string
	expiresAt time.Time
}

type unitermExecIn struct {
	SessionID   string `json:"sessionId" jsonschema:"session id from list_sessions"`
	Command     string `json:"command" jsonschema:"command to execute"`
	Mode        string `json:"mode,omitempty" jsonschema:"background (non-PTY SSH exec, default) | foreground (live terminal PTY tap)"`
	TimeoutMs   int    `json:"timeoutMs,omitempty" jsonschema:"background sync wait budget before going async (default 20000)"`
	HardLimitMs int    `json:"hardLimitMs,omitempty" jsonschema:"hard runtime cap in ms (default 300000)"`
	IdleMs      int    `json:"idleMs,omitempty" jsonschema:"foreground silence that counts as completion in ms (default 1200)"`
}

type unitermExecOut struct {
	JobID     string `json:"jobId"`
	Async     bool   `json:"async,omitempty"`
	Mode      string `json:"mode"`
	State     string `json:"state,omitempty"`
	Stdout    string `json:"stdout,omitempty"`
	Stderr    string `json:"stderr,omitempty"`
	ExitCode  int    `json:"exitCode"`
	Truncated bool   `json:"truncated,omitempty"`
}

type unitermResultIn struct {
	JobID  string `json:"jobId" jsonschema:"job id returned by uniterm_exec"`
	Offset int    `json:"offset,omitempty" jsonschema:"byte offset to resume stdout from (default 0)"`
}

type unitermResultOut struct {
	JobID      string `json:"jobId"`
	Mode       string `json:"mode,omitempty"`
	State      string `json:"state"` // running | exited | error | killed | timeout | session_closed | unknown
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
	ExitCode   int    `json:"exitCode"`
	NextOffset int    `json:"nextOffset"`
	Truncated  bool   `json:"truncated,omitempty"`
}

type unitermInterruptIn struct {
	JobID  string `json:"jobId" jsonschema:"job id from uniterm_exec"`
	Signal string `json:"signal,omitempty" jsonschema:"SIGINT (default), SIGTERM or SIGKILL"`
}

// registerJobTools installs the unified uniterm_* tools. The legacy
// exec_command / get_command_output / interrupt_command tools remain as
// aliases so existing external agents keep working.
func (s *Server) registerJobTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "uniterm_exec",
		Description: "Run a command and return a jobId. mode=background (default) uses a non-PTY SSH exec channel with a real exit code and separate stdout/stderr; " +
			"mode=foreground types into the live terminal and completes on idle/EOF. Poll with uniterm_exec_result(jobId).",
	}, s.toolUnitermExec)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "uniterm_exec_result",
		Description: "Fetch output (and status/exit code) of a job started by uniterm_exec, by jobId. Supports paging via offset.",
	}, s.toolUnitermResult)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "uniterm_interrupt",
		Description: "Stop a still-running job started by uniterm_exec (SIGINT/SIGTERM/SIGKILL for background; stop the foreground tap otherwise).",
	}, s.toolUnitermInterrupt)
}

func (s *Server) toolUnitermExec(ctx context.Context, req *mcp.CallToolRequest, in unitermExecIn) (*mcp.CallToolResult, unitermExecOut, error) {
	if strings.TrimSpace(in.SessionID) == "" {
		return nil, unitermExecOut{}, fmt.Errorf("sessionId is required")
	}
	if strings.TrimSpace(in.Command) == "" {
		return nil, unitermExecOut{}, fmt.Errorf("command is required")
	}
	mode := in.Mode
	if mode == "" {
		mode = ModeBackground
	}

	risk := classifyCommand(in.Command)
	if classifyDownloadPipe(in.Command) {
		risk = RiskDangerous
	}
	if err := s.gateExec(ctx, req, in.SessionID, in.Command, risk); err != nil {
		return nil, unitermExecOut{Mode: mode}, err
	}

	switch mode {
	case ModeForeground:
		return s.execForeground(ctx, req, in)
	case ModeBackground:
		return s.execBackground(ctx, req, in)
	default:
		return nil, unitermExecOut{Mode: mode}, fmt.Errorf("unknown mode %q (want background|foreground)", mode)
	}
}

// execBackground wraps the existing SSH non-PTY executor behind the unified
// job schema. An async commandId is returned as-is; a sync-completed result is
// minted a cached jobId so the client always reads through uniterm_exec_result.
func (s *Server) execBackground(ctx context.Context, req *mcp.CallToolRequest, in unitermExecIn) (*mcp.CallToolResult, unitermExecOut, error) {
	exec, ok := s.env.Sessions(in.SessionID)
	if !ok {
		return nil, unitermExecOut{Mode: ModeBackground}, fmt.Errorf("session %s not found (use list_sessions)", in.SessionID)
	}

	syncTimeout := in.TimeoutMs
	if syncTimeout <= 0 {
		syncTimeout = int(DefaultSyncTimeout / time.Millisecond)
	}
	hardTimeout := in.HardLimitMs
	if hardTimeout <= 0 {
		hardTimeout = int(DefaultCommandTimeout / time.Millisecond)
	}
	if hardTimeout < syncTimeout {
		hardTimeout = syncTimeout
	}

	stdout, stderr, exitCode, cmdID, err := exec.MCPExec(in.Command, syncTimeout, hardTimeout, DefaultMaxOutputBytes)
	out := unitermExecOut{Mode: ModeBackground, ExitCode: exitCode}
	s.audit(ctx, req, "uniterm_exec", in.SessionID, in.Command, &exitCode, err)

	if cmdID != "" {
		out.JobID = cmdID
		out.Async = true
		out.State = "running"
		return nil, out, nil
	}

	jobID := bgCachePrefix + uuid.New().String()
	s.cacheFinishedBackground(jobID, stdout, stderr, exitCode)
	out.JobID = jobID
	out.State = "exited"
	out.Stdout = string(stdout)
	out.Stderr = string(stderr)
	out.Truncated = len(stdout) >= DefaultMaxOutputBytes || len(stderr) >= DefaultMaxOutputBytes
	return nil, out, err
}

// execForeground starts a live-PTY tap job; completion is an idle/EOF event.
func (s *Server) execForeground(ctx context.Context, req *mcp.CallToolRequest, in unitermExecIn) (*mcp.CallToolResult, unitermExecOut, error) {
	if s.env.Foreground == nil {
		return nil, unitermExecOut{Mode: ModeForeground}, fmt.Errorf("foreground mode is not available on this build")
	}
	idleMs := in.IdleMs
	if idleMs <= 0 {
		idleMs = 1200
	}
	hardLimit := in.HardLimitMs
	if hardLimit <= 0 {
		hardLimit = int(DefaultCommandTimeout / time.Millisecond)
	}
	jobID, err := s.env.Foreground.StartForeground(in.SessionID, in.Command, idleMs, hardLimit)
	out := unitermExecOut{Mode: ModeForeground, JobID: jobID, Async: true, State: "running"}
	s.audit(ctx, req, "uniterm_exec", in.SessionID, in.Command, nil, err)
	if err != nil {
		out.State = "error"
		return nil, out, err
	}
	return nil, out, nil
}

func (s *Server) toolUnitermResult(ctx context.Context, req *mcp.CallToolRequest, in unitermResultIn) (*mcp.CallToolResult, unitermResultOut, error) {
	if strings.TrimSpace(in.JobID) == "" {
		return nil, unitermResultOut{}, fmt.Errorf("jobId is required")
	}

	// Foreground runner owns fg- prefixed jobs.
	if s.env.Foreground != nil && strings.HasPrefix(in.JobID, fgJobPrefix) {
		stdout, stderr, exitCode, state, err := s.env.Foreground.PollForeground(in.JobID, in.Offset, DefaultMaxOutputBytes)
		out := unitermResultOut{
			JobID:      in.JobID,
			Mode:       ModeForeground,
			State:      state,
			Stdout:     string(stdout),
			Stderr:     string(stderr),
			ExitCode:   exitCode,
			NextOffset: in.Offset + len(stdout),
		}
		s.audit(ctx, req, "uniterm_exec_result", "", in.JobID, &exitCode, err)
		return nil, out, nil
	}

	// Cached sync-completed background result.
	if buf, ok := s.takeFinishedBackground(in.JobID, in.Offset); ok {
		s.audit(ctx, req, "uniterm_exec_result", "", in.JobID, &buf.ExitCode, nil)
		return nil, buf, nil
	}

	// Async SSH exec commandId owned by a session.
	sid, exec, ok := s.env.Commands(in.JobID)
	if !ok || exec == nil {
		return nil, unitermResultOut{JobID: in.JobID, State: "unknown"}, fmt.Errorf("job %s not found or expired", in.JobID)
	}
	stdout, stderr, exitCode, state, err := exec.MCPLatestOutput(in.JobID, in.Offset, DefaultMaxOutputBytes)
	out := unitermResultOut{
		JobID:      in.JobID,
		Mode:       ModeBackground,
		State:      state,
		Stdout:     string(stdout),
		Stderr:     string(stderr),
		ExitCode:   exitCode,
		NextOffset: in.Offset + len(stdout),
	}
	s.audit(ctx, req, "uniterm_exec_result", sid, "", &exitCode, err)
	return nil, out, nil
}

func (s *Server) toolUnitermInterrupt(ctx context.Context, req *mcp.CallToolRequest, in unitermInterruptIn) (*mcp.CallToolResult, struct{}, error) {
	if strings.TrimSpace(in.JobID) == "" {
		return nil, struct{}{}, fmt.Errorf("jobId is required")
	}
	sig := in.Signal
	if sig == "" {
		sig = "SIGINT"
	}
	if s.env.Foreground != nil && strings.HasPrefix(in.JobID, fgJobPrefix) {
		err := s.env.Foreground.InterruptForeground(in.JobID, sig)
		s.audit(ctx, req, "uniterm_interrupt", "", in.JobID, nil, err)
		return nil, struct{}{}, err
	}
	_, exec, ok := s.env.Commands(in.JobID)
	if !ok || exec == nil {
		return nil, struct{}{}, fmt.Errorf("job %s not found or expired", in.JobID)
	}
	err := exec.MCPInterrupt(in.JobID, sig)
	s.audit(ctx, req, "uniterm_interrupt", "", in.JobID, nil, err)
	return nil, struct{}{}, err
}

// ── sync-completed background result cache ──────────────────────────────
//
// A background command that finishes inside the sync budget still gets a
// jobId (the unified contract always returns one); its result lives here until
// polled or TTL-expired. The map is small and short-lived.

func (s *Server) cacheFinishedBackground(jobID string, stdout, stderr []byte, exitCode int) {
	s.mu.Lock()
	if s.finishedBg == nil {
		s.finishedBg = map[string]finishedBackground{}
	}
	s.finishedBg[jobID] = finishedBackground{
		stdout:    append([]byte(nil), stdout...),
		stderr:    append([]byte(nil), stderr...),
		exitCode:  exitCode,
		state:     "exited",
		expiresAt: time.Now().Add(finishedTTL),
	}
	s.mu.Unlock()
}

func (s *Server) takeFinishedBackground(jobID string, offset int) (unitermResultOut, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.finishedBg[jobID]
	if !ok {
		return unitermResultOut{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(s.finishedBg, jobID)
		return unitermResultOut{}, false
	}
	out := entry.stdout
	if offset > 0 {
		if offset >= len(out) {
			out = nil
		} else {
			out = out[offset:]
		}
	}
	return unitermResultOut{
		JobID:      jobID,
		Mode:       ModeBackground,
		State:      entry.state,
		Stdout:     string(truncateBytes(out, DefaultMaxOutputBytes)),
		Stderr:     string(truncateBytes(entry.stderr, DefaultMaxOutputBytes)),
		ExitCode:   entry.exitCode,
		NextOffset: offset + len(out),
	}, true
}

// truncateBytes caps a byte slice at max, cutting at a newline when possible
// and appending a marker. Local to the mcp package (the session package has
// its own copy for the SSH path).
func truncateBytes(b []byte, max int) []byte {
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
