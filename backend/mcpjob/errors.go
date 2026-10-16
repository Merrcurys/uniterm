package mcpjob

import "errors"

// Sentinel errors surfaced to the MCP tool layer and mapped onto readable
// agent-facing messages.
var (
	ErrTooMany       = errors.New("too many concurrent jobs")
	ErrSessionClosed = errors.New("session closed")
	ErrUnknownJob    = errors.New("job not found or expired")
	ErrInterrupted   = errors.New("job interrupted")
)
