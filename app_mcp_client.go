package main

import (
	"context"
	"time"

	"github.com/ys-ll/uniterm/backend/log"
	"github.com/ys-ll/uniterm/backend/mcpclient"
)

// MCP client wiring: uniTerm connects to external MCP servers and feeds their
// tools into the built-in AI assistant. This is the CLIENT half; the SERVER
// half (uniTerm exposed to external agents) lives in app_mcp.go / backend/mcp.
// The two share no state and persist independently.

// ensureMCPClient lazily builds the manager over mcp_clients.json.
func (a *App) ensureMCPClient() *mcpclient.Manager {
	a.mcpClientOnce.Do(func() {
		a.mcpClient = mcpclient.NewManager(mcpclient.NewStore(a.dataDir))
	})
	return a.mcpClient
}

// StartMCPClient loads persisted external servers and connects the enabled
// ones. Called after stores are ready (alongside StartMCP).
func (a *App) StartMCPClient() {
	if a.dataDir == "" {
		return
	}
	mgr := a.ensureMCPClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mgr.Reload(ctx)
}

// ListMCPClients returns every persisted external server config.
func (a *App) ListMCPClients() ([]mcpclient.ClientConfig, error) {
	return a.ensureMCPClient().StoreLoad()
}

// SaveMCPClient upserts one external server by name, persists, and reloads
// live connections (a newly added/enabled server connects immediately; a
// disabled/edited one is dropped and re-dialed as needed).
func (a *App) SaveMCPClient(cfg mcpclient.ClientConfig) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	mgr := a.ensureMCPClient()
	all, err := mgr.StoreLoad()
	if err != nil {
		return err
	}
	replaced := false
	for i := range all {
		if all[i].Name == cfg.Name {
			all[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		all = append(all, cfg)
	}
	if err := mgr.StoreSave(all); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	mgr.Reload(ctx)
	return nil
}

// RemoveMCPClient deletes a persisted server and closes its live connection.
func (a *App) RemoveMCPClient(name string) error {
	mgr := a.ensureMCPClient()
	all, err := mgr.StoreLoad()
	if err != nil {
		return err
	}
	kept := all[:0]
	for _, c := range all {
		if c.Name != name {
			kept = append(kept, c)
		}
	}
	if err := mgr.StoreSave(kept); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	mgr.Reload(ctx)
	return nil
}

// TestMCPClient dials a candidate config once, lists its tools, and closes —
// used by the settings "test" button without persisting first.
func (a *App) TestMCPClient(cfg mcpclient.ClientConfig) ([]mcpclient.DiscoveredTool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return a.ensureMCPClient().TestConnection(ctx, cfg)
}

// ListMCPTools flattens healthy external servers' tools for the AI panel.
// Never errors out: a broken connection degrades to "no tools from it".
func (a *App) ListMCPTools() ([]mcpclient.DiscoveredTool, error) {
	if a.dataDir == "" {
		return []mcpclient.DiscoveredTool{}, nil
	}
	return a.ensureMCPClient().ListTools(), nil
}

// CallMCPTool invokes an external tool by its qualified name and returns text.
func (a *App) CallMCPTool(qualified string, args map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.ensureMCPClient().CallTool(ctx, qualified, args)
}

// MCPClientStatuses reports per-server health for the settings UI.
func (a *App) MCPClientStatuses() []mcpclient.ConnStatus {
	if a.dataDir == "" {
		return nil
	}
	mgr := a.ensureMCPClient()
	configs, err := mgr.StoreLoad()
	if err != nil {
		log.Writef("mcpclient: statuses: %v", err)
		return nil
	}
	return mgr.Statuses(configs)
}
