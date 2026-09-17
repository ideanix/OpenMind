// Package mcpserver exposes the store as MCP tools.
package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ideanix/openmind/internal/digest"
	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/store"
)

// Version is stamped into the MCP implementation info.
var Version = "dev"

const saveGuidance = `Save a note when you learn something a future session (yours or a teammate's) would need and cannot derive from the code: a decision and its reason, an environment fact, a correction from the user, a convention that differs from defaults, where something lives outside the repo. Do NOT save: anything readable from the codebase, secrets or credentials (writes are rejected), transient status, or long transcripts. One fact per note; title = the fact in one line; body = detail, **Why:** and **How to apply:** lines when relevant.`

// New builds an MCP server bound to one store and one acting user.
func New(st *store.Store, author string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "openmind", Version: Version}, &mcp.ServerOptions{
		Instructions: "OpenMind is the team's shared memory. Call openmind_context(project) at the start of a task to load what the team already knows, openmind_search before re-deriving facts, and openmind_put to record new learnings. " + saveGuidance,
	})
	h := handlers{st: st, author: author}

	type searchIn struct {
		Query   string   `json:"query" jsonschema:"free-text query; empty lists recent notes"`
		Project string   `json:"project" jsonschema:"project name, e.g. the repository name"`
		Type    string   `json:"type,omitempty" jsonschema:"user|feedback|project|reference|decision"`
		Tags    []string `json:"tags,omitempty"`
		Limit   int      `json:"limit,omitempty" jsonschema:"max results, default 20"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_search", Description: "Search the team's notes for a project. Returns ranked notes with ids and snippets."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			if in.Limit == 0 {
				in.Limit = 20
			}
			hits, err := st.Search(ctx, in.Query, store.Filter{Project: in.Project, Type: in.Type, Tags: in.Tags, Limit: in.Limit, Viewer: author})
			if err != nil {
				return nil, nil, err
			}
			if len(hits) == 0 {
				return text("No notes found."), nil, nil
			}
			var b strings.Builder
			for _, hit := range hits {
				n := hit.Note
				fmt.Fprintf(&b, "- [%s] **%s** (%s, %s, by %s, %s)\n  %s\n", n.ID, n.Title, n.Type, n.Scope, n.Author, n.Updated.Format("2006-01-02"), hit.Snippet)
			}
			return text(b.String()), nil, nil
		})

	type getIn struct {
		ID string `json:"id"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_get", Description: "Get one note by id, with full body and provenance."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getIn) (*mcp.CallToolResult, any, error) {
			n, err := st.Get(ctx, in.ID, author)
			if err != nil {
				return errText(err), nil, nil
			}
			return text(string(n.Markdown())), nil, nil
		})

	type listIn struct {
		Project string `json:"project"`
		Type    string `json:"type,omitempty"`
		Since   string `json:"since,omitempty" jsonschema:"RFC3339 timestamp; only notes updated after it"`
		Status  string `json:"status,omitempty" jsonschema:"active (default)|draft|deprecated|any"`
		Limit   int    `json:"limit,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_list", Description: "List notes for a project, newest first. Use since= to see what changed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, any, error) {
			f := store.Filter{Project: in.Project, Type: in.Type, Status: in.Status, Limit: in.Limit, Viewer: author}
			if in.Since != "" {
				t, err := time.Parse(time.RFC3339, in.Since)
				if err != nil {
					return errText(fmt.Errorf("since: %w", err)), nil, nil
				}
				f.Since = t
			}
			notes, err := st.List(ctx, f)
			if err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			for _, n := range notes {
				fmt.Fprintf(&b, "- [%s] %s (%s, %s, by %s, %s)\n", n.ID, n.Title, n.Type, n.Status, n.Author, n.Updated.Format("2006-01-02"))
			}
			if b.Len() == 0 {
				b.WriteString("No notes.")
			}
			return text(b.String()), nil, nil
		})

	type putIn struct {
		ID      string   `json:"id,omitempty" jsonschema:"set to update an existing note"`
		Project string   `json:"project"`
		Title   string   `json:"title" jsonschema:"the fact in one line"`
		Body    string   `json:"body" jsonschema:"markdown detail; include Why and How to apply"`
		Type    string   `json:"type,omitempty" jsonschema:"user|feedback|project|reference|decision (default project)"`
		Scope   string   `json:"scope,omitempty" jsonschema:"personal|project|team (default project)"`
		Tags    []string `json:"tags,omitempty"`
		Tool    string   `json:"tool,omitempty" jsonschema:"agent that produced the note, e.g. claude-code"`
		Session string   `json:"session,omitempty" jsonschema:"session id of the agent"`
		Model   string   `json:"model,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_put", Description: "Create or update a note. " + saveGuidance},
		func(ctx context.Context, _ *mcp.CallToolRequest, in putIn) (*mcp.CallToolResult, any, error) {
			n := &note.Note{ID: in.ID, Project: in.Project, Title: in.Title, Body: in.Body, Type: in.Type, Scope: in.Scope, Tags: in.Tags, Author: author,
				Source: note.Source{Tool: in.Tool, Session: in.Session, Model: in.Model}}
			out, err := st.Put(ctx, n)
			if err != nil {
				return errText(err), nil, nil
			}
			return text(fmt.Sprintf("saved %s (revision %d)", out.ID, out.Revision)), nil, nil
		})

	type depIn struct {
		ID     string `json:"id"`
		Reason string `json:"reason" jsonschema:"why the note is no longer true"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_deprecate", Description: "Mark a note obsolete. It stays in history but leaves search and context."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in depIn) (*mcp.CallToolResult, any, error) {
			out, err := st.Deprecate(ctx, in.ID, author, in.Reason)
			if err != nil {
				return errText(err), nil, nil
			}
			return text(fmt.Sprintf("deprecated %s", out.ID)), nil, nil
		})

	type ctxIn struct {
		Project string `json:"project"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_context", Description: "Compact digest of everything the team knows about a project. Call once at the start of a task."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ctxIn) (*mcp.CallToolResult, any, error) {
			return h.context(ctx, in.Project)
		})

	return s
}

type handlers struct {
	st     *store.Store
	author string
}

func (h handlers) context(ctx context.Context, project string) (*mcp.CallToolResult, any, error) {
	notes, err := h.st.List(ctx, store.Filter{Project: project, Limit: 500, Viewer: h.author})
	if err != nil {
		return nil, nil, err
	}
	return text(digest.Render(project, notes)), nil, nil
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errText(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
