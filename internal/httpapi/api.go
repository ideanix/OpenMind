// Package httpapi serves the MCP endpoint and a small REST API behind bearer
// token auth.
package httpapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ideanix/openmind/internal/digest"
	"github.com/ideanix/openmind/internal/mcpserver"
	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/store"
)

// Tokens maps a bearer secret to a user name. An empty map disables auth and
// attributes every write to "anonymous".
type Tokens map[string]string

// Handler builds the HTTP mux.
func Handler(st *store.Store, tokens Tokens, log *slog.Logger) http.Handler {
	a := &api{st: st, tokens: tokens, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })

	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return mcpserver.New(st, userFrom(r))
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, Logger: log})
	mux.Handle("/mcp", a.auth(mcpHandler))

	mux.Handle("GET /api/v1/notes", a.auth(http.HandlerFunc(a.list)))
	mux.Handle("POST /api/v1/notes", a.auth(http.HandlerFunc(a.put)))
	mux.Handle("GET /api/v1/notes/{id}", a.auth(http.HandlerFunc(a.get)))
	mux.Handle("GET /api/v1/notes/{id}/history", a.auth(http.HandlerFunc(a.history)))
	mux.Handle("DELETE /api/v1/notes/{id}", a.auth(http.HandlerFunc(a.deprecate)))
	mux.Handle("GET /api/v1/projects", a.auth(http.HandlerFunc(a.projects)))
	mux.Handle("GET /api/v1/projects/{project}/context", a.auth(http.HandlerFunc(a.context)))
	mux.Handle("GET /api/v1/whoami", a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"user": userFrom(r)})
	})))
	return mux
}

type api struct {
	st     *store.Store
	tokens Tokens
	log    *slog.Logger
}

type ctxKey struct{}

func userFrom(r *http.Request) string {
	if u, ok := r.Context().Value(ctxKey{}).(string); ok {
		return u
	}
	return "anonymous"
}

func (a *api) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := "anonymous"
		if len(a.tokens) > 0 {
			raw := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
			var ok bool
			for secret, name := range a.tokens {
				if subtle.ConstantTimeCompare([]byte(secret), []byte(raw)) == 1 {
					user, ok = name, true
					break
				}
			}
			if !ok {
				w.Header().Set("WWW-Authenticate", `Bearer realm="openmind"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
				return
			}
		}
		ctx := contextWithUser(r.Context(), user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *api) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.Filter{Project: q.Get("project"), Type: q.Get("type"), Scope: q.Get("scope"), Status: q.Get("status"), Viewer: userFrom(r)}
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
	out, err := a.st.Put(r.Context(), &n)
	if err != nil {
		writeErr(w, 422, err)
		return
	}
	writeJSON(w, 200, out)
}

func (a *api) get(w http.ResponseWriter, r *http.Request) {
	n, err := a.st.Get(r.Context(), r.PathValue("id"), userFrom(r))
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, 200, n)
}

func (a *api) history(w http.ResponseWriter, r *http.Request) {
	if _, err := a.st.Get(r.Context(), r.PathValue("id"), userFrom(r)); err != nil {
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
	writeJSON(w, 200, p)
}

func (a *api) context(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	notes, err := a.st.List(r.Context(), store.Filter{Project: project, Limit: 500, Viewer: userFrom(r)})
	if err != nil {
		writeErr(w, 500, err)
		return
	}
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
