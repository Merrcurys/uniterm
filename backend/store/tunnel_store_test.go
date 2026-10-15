package store

import (
	"testing"

	"github.com/ys-ll/uniterm/backend/session"
)

// TestTunnelStoreSyncRoundTrip pins the sync exchange contract for
// tunnels.json, including the Version backfill ImportPlain applies.
func TestTunnelStoreSyncRoundTrip(t *testing.T) {
	a := NewTunnelStore(t.TempDir())
	seed := session.TunnelStoreData{
		Version: 1,
		Groups:  []session.TunnelGroup{{ID: "tg1", Name: "g"}},
		Tunnels: []session.Tunnel{{ID: "t1", Name: "tn", Mode: "local", SSHConnID: "c1", ListenPort: 8080,
			AutoStart: true, AutoReconnect: true, ReconnectInterval: 120, ReconnectJitter: 20}},
	}
	if err := a.Save(seed); err != nil {
		t.Fatalf("Save: %v", err)
	}
	plain, err := a.ExportPlain()
	if err != nil {
		t.Fatalf("ExportPlain: %v", err)
	}
	b := NewTunnelStore(t.TempDir())
	if err := b.ImportPlain(plain); err != nil {
		t.Fatalf("ImportPlain: %v", err)
	}
	got, err := b.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Groups) != 1 || len(got.Tunnels) != 1 || got.Tunnels[0].ID != "t1" || got.Tunnels[0].ListenPort != 8080 {
		t.Fatalf("tunnels lost: %+v", got)
	}
	if !got.Tunnels[0].AutoStart || !got.Tunnels[0].AutoReconnect || got.Tunnels[0].ReconnectInterval != 120 || got.Tunnels[0].ReconnectJitter != 20 {
		t.Fatalf("tunnel reconnect settings lost: %+v", got.Tunnels[0])
	}
}
