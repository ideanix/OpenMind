// Package mcpserver exposes the store as MCP tools.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ideanix/openmind/internal/auth"
	"github.com/ideanix/openmind/internal/digest"
	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/store"
)

// Version is stamped into the MCP implementation info.
var Version = "dev"

const saveGuidance = `Save a note when you learn something a future session (yours or a teammate's) would need and cannot derive from the code: a decision and its reason, an environment fact, a correction from the user, a convention that differs from defaults, where something lives outside the repo. Do NOT save: anything readable from the codebase, secrets or credentials (writes are rejected), transient status, or long transcripts. One fact per note; title = the fact in one line; body = detail, **Why:** and **How to apply:** lines when relevant.`

// New builds an MCP server for one caller. When project is non-empty the
// session is pinned to it: every tool ignores its project argument and can
// only see that project. When empty, tools take a project argument and the
// identity's grants decide what is visible.
func New(st *store.Store, id auth.Identity, project string) *mcp.Server {
	return NewWithMeta(st, id, project, Meta{Remote: "stdio", Agent: "stdio"})
}

// Meta describes the connection a session arrived on, for the activity log.
type Meta struct {
	Remote string // client IP, or "stdio"
	Agent  string // client user agent
}

// NewWithMeta is New with connection details recorded on every event.
func NewWithMeta(st *store.Store, id auth.Identity, project string, meta Meta) *mcp.Server {
	scopeNote := "This session is bound to project " + project + "; the project argument is ignored."
	if project == "" {
		scopeNote = "Pass the project name explicitly."
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "openmind", Version: Version}, &mcp.ServerOptions{
		Instructions: "OpenMind is the team's shared memory. " + scopeNote + " Call openmind_context at the start of a task to load what the team already knows, openmind_search before re-deriving facts, and openmind_put to record new learnings. " + saveGuidance,
	})
	h := handlers{st: st, id: id, bound: project, meta: meta}
	author := id.User

	type searchIn struct {
		Query   string   `json:"query" jsonschema:"free-text query; empty lists recent notes"`
		Project string   `json:"project,omitempty" jsonschema:"project name; ignored when the session is bound to a project"`
		Type    string   `json:"type,omitempty" jsonschema:"user|feedback|project|reference|decision"`
		Tags    []string `json:"tags,omitempty"`
		Limit   int      `json:"limit,omitempty" jsonschema:"max results, default 20"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_search", Description: "Search the team's notes for a project. Returns ranked notes with ids and snippets."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			if in.Limit == 0 {
				in.Limit = 20
			}
			project, err := h.project(in.Project)
			if err != nil {
				h.log(ctx, "openmind_search", store.KindRead, in.Project, in.Query, "", err)
				return errText(err), nil, nil
			}
			hits, err := st.Search(ctx, in.Query, store.Filter{Project: project, Type: in.Type, Tags: in.Tags, Limit: in.Limit, Viewer: author})
			if err != nil {
				return nil, nil, err
			}
			h.log(ctx, "openmind_search", store.KindRead, project, fmt.Sprintf("%q → %d hits", in.Query, len(hits)), "", nil)
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
			n, err := h.get(ctx, in.ID)
			if err != nil {
				h.log(ctx, "openmind_get", store.KindRead, "", in.ID, in.ID, err)
				return errText(err), nil, nil
			}
			h.log(ctx, "openmind_get", store.KindRead, n.Project, n.Title, n.ID, nil)
			return text(string(n.Markdown())), nil, nil
		})

	type listIn struct {
		Project string `json:"project,omitempty"`
		Type    string `json:"type,omitempty"`
		Since   string `json:"since,omitempty" jsonschema:"RFC3339 timestamp; only notes updated after it"`
		Status  string `json:"status,omitempty" jsonschema:"active (default)|draft|deprecated|any"`
		Limit   int    `json:"limit,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_list", Description: "List notes for a project, newest first. Use since= to see what changed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIn) (*mcp.CallToolResult, any, error) {
			project, err := h.project(in.Project)
			if err != nil {
				h.log(ctx, "openmind_list", store.KindRead, in.Project, "", "", err)
				return errText(err), nil, nil
			}
			detail := ""
			if in.Since != "" {
				detail = "since " + in.Since
			}
			h.log(ctx, "openmind_list", store.KindRead, project, detail, "", nil)
			f := store.Filter{Project: project, Type: in.Type, Status: in.Status, Limit: in.Limit, Viewer: author}
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
		Project string   `json:"project,omitempty"`
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
			project, err := h.project(in.Project)
			if err != nil {
				h.log(ctx, "openmind_put", store.KindWrite, in.Project, in.Title, in.ID, err)
				return errText(err), nil, nil
			}
			if in.ID != "" {
				if _, err := h.get(ctx, in.ID); err != nil {
					h.log(ctx, "openmind_put", store.KindWrite, project, in.Title, in.ID, err)
					return errText(err), nil, nil
				}
			}
			n := &note.Note{ID: in.ID, Project: project, Title: in.Title, Body: in.Body, Type: in.Type, Scope: in.Scope, Tags: in.Tags, Author: author,
				Source: note.Source{Tool: in.Tool, Session: in.Session, Model: in.Model}}
			out, err := st.Put(ctx, n)
			if err != nil {
				h.log(ctx, "openmind_put", store.KindWrite, project, in.Title, in.ID, err)
				return errText(err), nil, nil
			}
			h.log(ctx, "openmind_put", store.KindWrite, project, fmt.Sprintf("%s (rev %d)", out.Title, out.Revision), out.ID, nil)
			return text(fmt.Sprintf("saved %s (revision %d)", out.ID, out.Revision)), nil, nil
		})

	type depIn struct {
		ID     string `json:"id"`
		Reason string `json:"reason" jsonschema:"why the note is no longer true"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_deprecate", Description: "Mark a note obsolete. It stays in history but leaves search and context."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in depIn) (*mcp.CallToolResult, any, error) {
			if _, err := h.get(ctx, in.ID); err != nil {
				h.log(ctx, "openmind_deprecate", store.KindWrite, "", in.ID, in.ID, err)
				return errText(err), nil, nil
			}
			out, err := st.Deprecate(ctx, in.ID, author, in.Reason)
			if err != nil {
				return errText(err), nil, nil
			}
			h.log(ctx, "openmind_deprecate", store.KindWrite, out.Project, out.Title+": "+in.Reason, out.ID, nil)
			return text(fmt.Sprintf("deprecated %s", out.ID)), nil, nil
		})

	type ctxIn struct {
		Project string `json:"project,omitempty"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "openmind_context", Description: "Compact digest of everything the team knows about a project. Call once at the start of a task."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in ctxIn) (*mcp.CallToolResult, any, error) {
			return h.context(ctx, in.Project)
		})

	return s
}

type handlers struct {
	st    *store.Store
	id    auth.Identity
	bound string
	meta  Meta
}

// log records one tool call. kind is derived from err when it is non-nil.
func (h handlers) log(ctx context.Context, action, kind, project, detail, noteID string, err error) {
	if err != nil {
		kind = store.KindError
		if errors.Is(err, ErrForbidden) || errors.Is(err, store.ErrNotFound) {
			kind = store.KindDenied
		}
		detail = strings.TrimSpace(detail + " → " + err.Error())
	}
	if project == "" {
		project = h.bound
	}
	h.st.LogActivity(ctx, store.Event{User: h.id.User, Project: project, Action: action, Kind: kind,
		Detail: detail, NoteID: noteID, Remote: h.meta.Remote, Agent: h.meta.Agent})
}

// ErrProjectRequired is returned when neither the session nor the call names
// a project.
var ErrProjectRequired = errors.New("project is required")

// ErrForbidden is returned when the identity has no grant for the project.
var ErrForbidden = errors.New("access to this project is not granted")

// project resolves the effective project for a call and checks the grant.
func (h handlers) project(requested string) (string, error) {
	p := h.bound
	if p == "" {
		p = requested
	}
	if p == "" {
		return "", ErrProjectRequired
	}
	if !h.id.Allows(p) {
		return "", ErrForbidden
	}
	return p, nil
}

// get loads a note and hides it unless it belongs to an allowed project (and
// to the bound project, when the session is pinned).
func (h handlers) get(ctx context.Context, id string) (*note.Note, error) {
	n, err := h.st.Get(ctx, id, h.id.User)
	if err != nil {
		return nil, err
	}
	if (h.bound != "" && n.Project != h.bound) || !h.id.Allows(n.Project) {
		return nil, store.ErrNotFound
	}
	return n, nil
}

func (h handlers) context(ctx context.Context, requested string) (*mcp.CallToolResult, any, error) {
	project, err := h.project(requested)
	if err != nil {
		h.log(ctx, "openmind_context", store.KindRead, requested, "", "", err)
		return errText(err), nil, nil
	}
	notes, err := h.st.List(ctx, store.Filter{Project: project, Limit: 500, Viewer: h.id.User})
	if err != nil {
		return nil, nil, err
	}
	h.log(ctx, "openmind_context", store.KindRead, project, fmt.Sprintf("digest of %d notes", len(notes)), "", nil)
	return text(digest.Render(project, notes)), nil, nil
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func errText(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}
}
