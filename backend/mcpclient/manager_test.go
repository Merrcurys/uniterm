package mcpclient

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSession stands in for *mcp.ClientSession in tests; it returns a fixed tool
// list and echoes the called tool name/args.
type fakeSession struct {
	tools []*mcp.Tool
	calls []string
}

func (f *fakeSession) ListTools(ctx context.Context, params *mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return &mcp.ListToolsResult{Tools: f.tools}, nil
}

func (f *fakeSession) CallTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.calls = append(f.calls, params.Name)
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "echo:" + params.Name},
	}}, nil
}

func (f *fakeSession) Close() error { return nil }

func TestQualifiedName(t *testing.T) {
	cases := map[string]string{
		"fs":          "mcp__fs",
		"File Server": "mcp__file_server",
		"a/b:c":       "mcp__a_b_c",
	}
	for in, want := range cases {
		if got := QualifiedName(in, "read_file"); got != want+"__read_file" {
			t.Errorf("QualifiedName(%q,read_file)=%q want %q", in, got, want+"__read_file")
		}
	}
	if _, _, ok := splitQualified("plain_tool"); ok {
		t.Error("plain tool must not parse as qualified")
	}
	server, tool, ok := splitQualified("mcp__fs__read_file")
	if !ok || server != "fs" || tool != "read_file" {
		t.Errorf("splitQualified = %q,%q,%v", server, tool, ok)
	}
}

func TestConfigValidate(t *testing.T) {
	if err := (ClientConfig{Name: "x", Transport: TransportStdio}).Validate(); err == nil {
		t.Error("stdio without command should fail")
	}
	if err := (ClientConfig{Name: "x", Transport: TransportStreamableHTTP, URL: "http://x"}).Validate(); err != nil {
		t.Errorf("valid http config: %v", err)
	}
	if err := (ClientConfig{Name: "x", Transport: "ftp"}).Validate(); err == nil {
		t.Error("unknown transport should fail")
	}
}

func TestManagerListAndCall(t *testing.T) {
	fs := &fakeSession{tools: []*mcp.Tool{
		{Name: "read_file", Description: "read a file", InputSchema: map[string]any{"type": "object"}},
		{Name: "write_file", Description: "write a file"},
	}}
	m := NewManager(NewStore(t.TempDir()))
	m.dial = func(ctx context.Context, cfg ClientConfig) (Session, error) { return fs, nil }

	ctx := context.Background()
	m.ensureConn(ctx, ClientConfig{Name: "fs", Transport: TransportStdio, Command: "npx", Enabled: true})

	tools := m.ListTools()
	if len(tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(tools))
	}
	if tools[0].QualifiedName != "mcp__fs__read_file" {
		t.Errorf("qualified = %q", tools[0].QualifiedName)
	}

	out, err := m.CallTool(ctx, "mcp__fs__read_file", map[string]any{"path": "/tmp/a"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "echo:read_file" {
		t.Errorf("out=%q", out)
	}
	if fs.calls[0] != "read_file" {
		t.Errorf("remote tool called = %q", fs.calls[0])
	}

	// A non-qualified name must be rejected.
	if _, err := m.CallTool(ctx, "read_file", nil); err == nil {
		t.Error("plain name should error")
	}
}

func TestManagerRecordsDialFailure(t *testing.T) {
	wantErr := errors.New("boom")
	m := NewManager(NewStore(t.TempDir()))
	m.dial = func(ctx context.Context, cfg ClientConfig) (Session, error) { return nil, wantErr }
	m.ensureConn(context.Background(), ClientConfig{Name: "bad", Transport: TransportStdio, Command: "x", Enabled: true})

	if tools := m.ListTools(); len(tools) != 0 {
		t.Errorf("failed conn must not list tools, got %d", len(tools))
	}
	statuses := m.Statuses([]ClientConfig{{Name: "bad", Enabled: true}})
	if len(statuses) != 1 || statuses[0].Connected || statuses[0].Error == "" {
		t.Errorf("statuses=%+v", statuses)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(t.TempDir())
	in := []ClientConfig{
		{Name: "B", Transport: TransportStdio, Command: "npx", Enabled: true},
		{Name: "a", Transport: TransportSSE, URL: "http://x/sse", Enabled: false},
	}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2, got %d", len(got))
	}
	// Saved sorted by name.
	if got[0].Name != "a" || got[1].Name != "B" {
		t.Errorf("not sorted: %s, %s", got[0].Name, got[1].Name)
	}
	// Missing file → empty, not error.
	empty := NewStore(t.TempDir())
	list, err := empty.Load()
	if err != nil || len(list) != 0 {
		t.Errorf("missing file: list=%v err=%v", list, err)
	}
}
