package mcpserver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ideanix/openmind/internal/auth"
	"github.com/ideanix/openmind/internal/store"
)

func TestToolsEndToEnd(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	srv := New(st, auth.Identity{User: "alice", Projects: []string{"*"}}, "")
	ct, stt := mcp.NewInMemoryTransports()
	go srv.Run(ctx, stt)
	cli := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}
	out := call("openmind_put", map[string]any{"project": "logdoc", "title": "Tests need Redis", "body": "Run make redis first.\n**Why:** queue not mocked.", "tags": []string{"testing"}, "tool": "claude-code"})
	if !strings.HasPrefix(out, "saved ") {
		t.Fatalf("put: %s", out)
	}
	id := strings.Fields(out)[1]
	if s := call("openmind_search", map[string]any{"project": "logdoc", "query": "redis"}); !strings.Contains(s, id) {
		t.Fatalf("search: %s", s)
	}
	if g := call("openmind_get", map[string]any{"id": id}); !strings.Contains(g, "queue not mocked") {
		t.Fatalf("get: %s", g)
	}
	if c := call("openmind_context", map[string]any{"project": "logdoc"}); !strings.Contains(c, "Tests need Redis") {
		t.Fatalf("context: %s", c)
	}
	if r := call("openmind_put", map[string]any{"project": "logdoc", "title": "leak", "body": "AKIAIOSFODNN7EXAMPLE"}); !strings.Contains(r, "secret detected") {
		t.Fatalf("secret not rejected: %s", r)
	}
	call("openmind_deprecate", map[string]any{"id": id, "reason": "redis is mocked now"})
	if l := call("openmind_list", map[string]any{"project": "logdoc"}); l != "No notes." {
		t.Fatalf("list after deprecate: %s", l)
	}
}

func connect(t *testing.T, srv *mcp.Server) func(string, map[string]any) string {
	t.Helper()
	ctx := context.Background()
	ct, stt := mcp.NewInMemoryTransports()
	go srv.Run(ctx, stt)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return func(name string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}
}

func TestBoundSessionCannotCrossProjects(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	root := auth.Identity{User: "root", Projects: []string{"*"}}
	admin := connect(t, New(st, root, ""))
	out := admin("openmind_put", map[string]any{"project": "work", "title": "work secret fact", "body": "internal only"})
	workID := strings.Fields(out)[1]
	admin("openmind_put", map[string]any{"project": "personal", "title": "personal fact", "body": "mine"})

	// A session bound to "personal" cannot see "work" even if it asks for it.
	bound := connect(t, New(st, root, "personal"))
	if s := bound("openmind_search", map[string]any{"project": "work", "query": "secret"}); strings.Contains(s, "work secret") {
		t.Fatalf("bound session leaked another project: %s", s)
	}
	if g := bound("openmind_get", map[string]any{"id": workID}); !strings.Contains(g, "not found") {
		t.Fatalf("bound get leaked: %s", g)
	}
	if c := bound("openmind_context", map[string]any{"project": "work"}); !strings.Contains(c, "personal fact") || strings.Contains(c, "work secret") {
		t.Fatalf("context not pinned: %s", c)
	}

	// A token granted only "personal" cannot reach "work" in unbound mode either.
	limited := connect(t, New(st, auth.Identity{User: "bob", Projects: []string{"personal"}}, ""))
	if s := limited("openmind_search", map[string]any{"project": "work", "query": "secret"}); !strings.Contains(s, "not granted") {
		t.Fatalf("grant not enforced: %s", s)
	}
	if s := limited("openmind_put", map[string]any{"project": "work", "title": "x", "body": "y"}); !strings.Contains(s, "not granted") {
		t.Fatalf("put grant not enforced: %s", s)
	}
	if s := limited("openmind_list", map[string]any{}); !strings.Contains(s, "project is required") {
		t.Fatalf("unbound call without project accepted: %s", s)
	}
}
