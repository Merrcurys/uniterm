package mcpclient

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestInMemoryRoundTrip connects the manager's dialer to a real SDK server over
// in-memory transports, lists tools, and calls one — proving the SDK wiring
// (ListTools/CallTool) works, not just the fake session.
func TestInMemoryRoundTrip(t *testing.T) {
	ctx := context.Background()

	echo := func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:hi"}}}, nil
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "ping",
		Description: "echo back",
		InputSchema: map[string]any{"type": "object"},
	}, echo)

	client := mcp.NewClient(&mcp.Implementation{Name: "uniterm-test", Version: "1.0.0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	m := NewManager(NewStore(t.TempDir()))
	m.dial = func(context.Context, ClientConfig) (Session, error) { return cs, nil }

	m.ensureConn(ctx, ClientConfig{Name: "echo", Transport: TransportStdio, Enabled: true})

	tools := m.ListTools()
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("tools=%+v", tools)
	}
	if tools[0].QualifiedName != "mcp__echo__ping" {
		t.Fatalf("qualified=%q", tools[0].QualifiedName)
	}

	out, err := m.CallTool(ctx, "mcp__echo__ping", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "echo:hi" {
		t.Fatalf("out=%q", out)
	}
}
