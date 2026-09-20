// Package httpapi serves the MCP endpoint and a small REST API behind bearer
// token auth.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ideanix/openmind/internal/auth"
	"github.com/ideanix/openmind/internal/digest"
	"github.com/ideanix/openmind/internal/mcpserver"
	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/store"
)

// ProjectHeader pins an MCP session to one project. Clients set it from
// their per-repository config so a session can only see its own project.
const ProjectHeader = "X-OpenMind-Project"

// Handler builds the HTTP mux. An empty token map disables auth: every
// caller is anonymous with access to every project (local mode only).
//
// requireAuth is set when the server is reachable from the network: a
// request is then never treated as anonymous, even if the token store is
// emptied while the server runs.
func Handler(st *store.Store, tokens auth.Resolver, requireAuth bool, log *slog.Logger) http.Handler {
	a := &api{st: st, tokens: tokens, requireAuth: requireAuth, log: log, sessions: newSessions()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpserver.NewWithMeta(st, identityFrom(r), strings.TrimSpace(r.Header.Get(ProjectHeader)),
			mcpserver.Meta{Remote: remoteOf(r), Agent: r.UserAgent()})
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log})
	mux.Handle("/mcp", a.auth(mcpHandler))

	mux.Handle("GET /api/v1/notes", a.auth(http.HandlerFunc(a.list)))
	mux.Handle("POST /api/v1/notes", a.auth(http.HandlerFunc(a.put)))
	mux.Handle("GET /api/v1/notes/{id}", a.auth(http.HandlerFunc(a.get)))
	mux.Handle("GET /api/v1/notes/{id}/history", a.auth(http.HandlerFunc(a.history)))
	mux.Handle("DELETE /api/v1/notes/{id}", a.auth(http.HandlerFunc(a.deprecate)))
	mux.Handle("GET /api/v1/projects", a.auth(http.HandlerFunc(a.projects)))
	mux.Handle("GET /api/v1/projects/{project}/context", a.auth(http.HandlerFunc(a.context)))
	mux.Handle("GET /api/v1/context", a.auth(http.HandlerFunc(a.context)))
	mux.Handle("GET /download/openmind", a.auth(http.HandlerFunc(a.download)))
	mux.HandleFunc("GET /ui", a.ui)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ui", http.StatusFound) })
	mux.Handle("POST /api/v1/admin/login-code", a.admin(a.loginCode))
	mux.HandleFunc("POST /api/v1/session", a.sessionExchange)
	mux.Handle("GET /api/v1/admin/overview", a.admin(a.adminOverview))
	mux.Handle("GET /api/v1/admin/clients", a.admin(a.adminClients))
	mux.Handle("GET /api/v1/admin/activity", a.admin(a.adminActivity))

	mux.Handle("GET /api/v1/tasks", a.auth(http.HandlerFunc(a.taskList)))
	mux.Handle("POST /api/v1/tasks", a.auth(http.HandlerFunc(a.taskCreate)))
	mux.Handle("POST /api/v1/tasks/claim", a.auth(http.HandlerFunc(a.taskClaim)))
	mux.Handle("GET /api/v1/tasks/{id}", a.auth(http.HandlerFunc(a.taskGet)))
	mux.Handle("POST /api/v1/tasks/{id}/heartbeat", a.auth(http.HandlerFunc(a.taskHeartbeat)))
	mux.Handle("POST /api/v1/tasks/{id}/report", a.auth(http.HandlerFunc(a.taskReport)))
	mux.Handle("POST /api/v1/tasks/{id}/cancel", a.auth(http.HandlerFunc(a.taskCancel)))
	mux.Handle("POST /api/v1/tasks/{id}/requeue", a.auth(http.HandlerFunc(a.taskRequeue)))

	mux.Handle("GET /api/v1/whoami", a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := identityFrom(r)
		writeJSON(w, 200, map[string]any{"user": id.User, "projects": id.Projects})
	})))
	return mux
}

type api struct {
	st          *store.Store
	tokens      auth.Resolver
	requireAuth bool
	log         *slog.Logger
	sessions    *sessions
}

type ctxKey struct{}

func identityFrom(r *http.Request) auth.Identity {
	if id, ok := r.Context().Value(ctxKey{}).(auth.Identity); ok {
		return id
	}
	return auth.Anonymous
}

func userFrom(r *http.Request) string { return identityFrom(r).User }

// allow checks the grant for a project; empty project is rejected.
func (a *api) allow(w http.ResponseWriter, r *http.Request, project string) bool {
	if project == "" {
		writeErr(w, 400, errors.New("project is required"))
		return false
	}
	if !identityFrom(r).Allows(project) {
		a.record(r, "rest:"+strings.ToLower(r.Method), store.KindDenied, project, "no grant for "+r.URL.Path, "")
		writeErr(w, 403, errors.New("access to this project is not granted"))
		return false
	}
	return true
}

func (a *api) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := auth.Anonymous
		if a.requireAuth || (a.tokens != nil && a.tokens.Enabled()) {
			raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
			var ok bool
			if strings.HasPrefix(raw, "s_") {
				id, ok = a.sessions.resolve(raw)
			} else if a.tokens != nil && raw != "" {
				id, ok = a.tokens.Resolve(raw)
			}
			if !ok {
				reason := "no token"
				if raw != "" {
					reason = "invalid token"
				}
				a.st.LogActivity(r.Context(), store.Event{User: "?", Action: "auth", Kind: store.KindDenied,
					Detail: reason + " for " + r.Method + " " + r.URL.Path, Remote: remoteOf(r), Agent: r.UserAgent()})
				w.Header().Set("WWW-Authenticate", `Bearer realm="openmind"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(contextWithIdentity(r.Context(), id)))
	})
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{Project: q.Get("project"), Type: q.Get("type"), Scope: q.Get("scope"), Status: q.Get("status"), Viewer: userFrom(r)}
	if !a.allow(w, r, f.Project) {
		return
	}
	if t := q.Get("tags"); t != "" {
		f.Tags = strings.Split(t, ",")
	}
	if l := q.Get("limit"); l != "" {
		f.Limit, _ = strconv.Atoi(l)
	}
	if s := q.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		f.Since = t
	}
	if query := q.Get("q"); query != "" {
		hits, err := a.st.Search(r.Context(), query, f)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		a.record(r, "rest:search", store.KindRead, f.Project, strconv.Quote(query)+" → "+strconv.Itoa(len(hits))+" hits", "")
		writeJSON(w, 200, hits)
		return
	}
	notes, err := a.st.List(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if notes == nil {
		notes = []*note.Note{}
	}
	a.record(r, "rest:list", store.KindRead, f.Project, strconv.Itoa(len(notes))+" notes", "")
	writeJSON(w, 200, notes)
}

func (a *api) put(w http.ResponseWriter, r *http.Request) {
	var n note.Note
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&n); err != nil {
		writeErr(w, 400, err)
		return
	}
	if n.Author == "" || userFrom(r) != "anonymous" {
		n.Author = userFrom(r)
	}
	if !a.allow(w, r, n.Project) {
		return
	}
	if n.ID != "" {
		if _, err := a.visible(r, n.ID); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
	}
	out, err := a.st.Put(r.Context(), &n)
	if err != nil {
		a.record(r, "rest:put", store.KindError, n.Project, n.Title+" → "+err.Error(), n.ID)
		writeErr(w, 422, err)
		return
	}
	a.record(r, "rest:put", store.KindWrite, out.Project, out.Title+" (rev "+strconv.Itoa(out.Revision)+")", out.ID)
	writeJSON(w, 200, out)
}

// visible loads a note the caller may see, or ErrNotFound.
func (a *api) visible(r *http.Request, id string) (*note.Note, error) {
	n, err := a.st.Get(r.Context(), id, userFrom(r))
	if err != nil {
		return nil, err
	}
	if !identityFrom(r).Allows(n.Project) {
		return nil, store.ErrNotFound
	}
	return n, nil
}

func (a *api) get(w http.ResponseWriter, r *http.Request) {
	n, err := a.visible(r, r.PathValue("id"))
	if err != nil {
		a.record(r, "rest:get", store.KindDenied, "", r.PathValue("id")+" → "+err.Error(), r.PathValue("id"))
		writeErr(w, statusFor(err), err)
		return
	}
	a.record(r, "rest:get", store.KindRead, n.Project, n.Title, n.ID)
	writeJSON(w, 200, n)
}

func (a *api) history(w http.ResponseWriter, r *http.Request) {
	if _, err := a.visible(r, r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	h, err := a.st.History(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if h == nil {
		h = []*note.Note{}
	}
	writeJSON(w, 200, h)
}

func (a *api) deprecate(w http.ResponseWriter, r *http.Request) {
	if _, err := a.visible(r, r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	n, err := a.st.Deprecate(r.Context(), r.PathValue("id"), userFrom(r), r.URL.Query().Get("reason"))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, n)
}

func (a *api) projects(w http.ResponseWriter, r *http.Request) {
	p, err := a.st.Projects(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	id := identityFrom(r)
	for name := range p {
		if !id.Allows(name) {
			delete(p, name)
		}
	}
	writeJSON(w, 200, p)
}

func (a *api) context(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	if project == "" {
		project = r.URL.Query().Get("project")
	}
	if !a.allow(w, r, project) {
		return
	}
	notes, err := a.st.List(r.Context(), store.Filter{Project: project, Limit: 500, Viewer: userFrom(r)})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	a.record(r, "rest:context", store.KindRead, project, "digest of "+strconv.Itoa(len(notes))+" notes", "")
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(digest.Render(project, notes)))
}

func statusFor(err error) int {
	if errors.Is(err, store.ErrNotFound) {
		return 404
	}
	return 500
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
