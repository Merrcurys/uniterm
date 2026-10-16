// Package mcpclient implements uniTerm as an MCP CLIENT: it connects to
// external MCP servers (stdio subprocess, streamable HTTP, legacy SSE) and
// exposes their tools to the built-in AI assistant.
//
// This package is deliberately isolated from backend/mcp (which implements
// uniTerm as an MCP SERVER for external agents). The two coexist and must not
// share state: server config lives in local_state.json + mcp.json, client
// config lives in its own mcp_clients.json.
//
// Transport/tool protocol handling is delegated to the vendored
// github.com/modelcontextprotocol/go-sdk/mcp SDK — no protocol stack is
// reimplemented here.
package mcpclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Transport identifiers.
const (
	TransportStdio          = "stdio"
	TransportStreamableHTTP = "streamable-http"
	TransportSSE            = "sse"
)

// ClientConfig is one external MCP server connection, as persisted in
// mcp_clients.json.
type ClientConfig struct {
	// Name is the user-facing label and the stable key used to namespace its
	// tools (mcp__<name>__<tool>). Required, unique, non-empty.
	Name string `json:"name"`
	// Transport is stdio | streamable-http | sse.
	Transport string `json:"transport"`
	// Command + Args run a stdio server subprocess. Used when Transport ==
	// stdio.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// Env are extra environment variables for the stdio subprocess.
	Env map[string]string `json:"env,omitempty"`
	// URL is the streamable-http / sse endpoint.
	URL string `json:"url,omitempty"`
	// Headers are static request headers (e.g. Authorization: Bearer ...) for
	// the HTTP transports.
	Headers map[string]string `json:"headers,omitempty"`
	// Enabled toggles the connection without deleting it.
	Enabled bool `json:"enabled"`
}

// Validate returns a readable error when the config cannot be connected.
func (c ClientConfig) Validate() error {
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		return errors.New("mcp client name is required")
	}
	switch c.Transport {
	case TransportStdio:
		if strings.TrimSpace(c.Command) == "" {
			return errors.New("stdio transport requires a command")
		}
	case TransportStreamableHTTP, TransportSSE:
		if strings.TrimSpace(c.URL) == "" {
			return fmt.Errorf("%s transport requires a url", c.Transport)
		}
	default:
		return fmt.Errorf("unknown transport %q (want %s|%s|%s)", c.Transport,
			TransportStdio, TransportStreamableHTTP, TransportSSE)
	}
	return nil
}

// ClientsFile is the on-disk shape of mcp_clients.json.
type ClientsFile struct {
	Clients []ClientConfig `json:"clients"`
}

// Store persists the client list as JSON in the data dir. It owns the file;
// the Manager owns live connections.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore builds a Store rooted at dataDir (file: mcp_clients.json).
func NewStore(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, "mcp_clients.json")}
}

// Path returns the backing file path (diagnostics / tests).
func (s *Store) Path() string { return s.path }

// Load reads the client list. A missing file yields an empty list (the
// feature is off until the user adds a server), never an error.
func (s *Store) Load() ([]ClientConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f ClientsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	out := f.Clients
	if out == nil {
		out = []ClientConfig{}
	}
	return out, nil
}

// Save writes the whole list atomically (temp file + rename), 0600 perms:
// headers may carry bearer tokens.
func (s *Store) Save(clients []ClientConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if clients == nil {
		clients = []ClientConfig{}
	}
	// Normalize + de-duplicate by name on write so the file stays canonical.
	seen := map[string]bool{}
	clean := make([]ClientConfig, 0, len(clients))
	for _, c := range clients {
		c.Name = strings.TrimSpace(c.Name)
		if c.Name == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		clean = append(clean, c)
	}
	sort.Slice(clean, func(i, j int) bool {
		ni, nj := strings.ToLower(clean[i].Name), strings.ToLower(clean[j].Name)
		if ni != nj {
			return ni < nj
		}
		return clean[i].Name < clean[j].Name
	})

	payload, err := json.MarshalIndent(ClientsFile{Clients: clean}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
