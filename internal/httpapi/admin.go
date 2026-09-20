package httpapi

import (
	_ "embed"
	"errors"
	"net"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/ideanix/openmind/internal/auth"
	"github.com/ideanix/openmind/internal/store"
)

//go:embed ui.html
var uiHTML []byte

// Version is shown in the control desk.
var Version = "dev"

func remoteOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// record writes one REST event to the activity log.
func (a *api) record(r *http.Request, action, kind, project, detail, noteID string) {
	a.st.LogActivity(r.Context(), store.Event{User: userFrom(r), Project: project, Action: action, Kind: kind,
		Detail: detail, NoteID: noteID, Remote: remoteOf(r), Agent: r.UserAgent()})
}

func (a *api) ui(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src data:; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Write(uiHTML)
}

// download serves the running binary so a client on the same platform can
// install the worker without a build toolchain. The platform is announced in
// a header; the client script refuses a mismatch.
func (a *api) download(w http.ResponseWriter, r *http.Request) {
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	a.record(r, "download", store.KindRead, "", "openmind binary "+runtime.GOOS+"/"+runtime.GOARCH, "")
	w.Header().Set("X-OpenMind-Platform", runtime.GOOS+"/"+runtime.GOARCH)
	w.Header().Set("Content-Disposition", `attachment; filename="openmind"`)
	http.ServeFile(w, r, exe)
}

// admin wraps a handler so that only identities holding the "*" grant pass.
func (a *api) admin(next http.HandlerFunc) http.Handler {
	return a.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !identityFrom(r).Admin() {
			a.record(r, "admin", store.KindDenied, "", r.URL.Path, "")
			writeErr(w, http.StatusForbidden, errors.New(`the control desk needs a token with the "*" grant`))
			return
		}
		next(w, r)
	}))
}

// clientView is one row of the control desk's client list.
type clientView struct {
	Name     string   `json:"name"`
	Grants   []string `json:"grants"`
	HasToken bool     `json:"has_token"`
	Tokens   int      `json:"tokens"`
	Online   bool     `json:"online"`
	*store.ClientStat
}

// onlineWindow is how recently a client must have called to count as online.
// HTTP is stateless, so "connected" means "seen recently".
const onlineWindow = 5 * time.Minute

func (a *api) adminClients(w http.ResponseWriter, r *http.Request) {
	stats, err := a.st.ClientStats(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	views := map[string]*clientView{}
	if l, ok := a.tokens.(auth.Lister); ok {
		for _, e := range l.List() {
			v := views[e.Name]
			if v == nil {
				v = &clientView{Name: e.Name, HasToken: true}
				views[e.Name] = v
			}
			v.Tokens++
			v.Grants = append(v.Grants, e.Projects...)
		}
	}
	for user, st := range stats {
		v := views[user]
		if v == nil {
			v = &clientView{Name: user}
			views[user] = v
		}
		v.ClientStat = st
		v.Online = time.Since(st.LastSeen) < onlineWindow
	}
	out := make([]*clientView, 0, len(views))
	for _, v := range views {
		if v.ClientStat == nil {
			v.ClientStat = &store.ClientStat{User: v.Name}
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, 200, out)
}

func (a *api) adminActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ActivityFilter{User: q.Get("user"), Project: q.Get("project")}
	f.AfterID, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	if m, err := strconv.Atoi(q.Get("minutes")); err == nil && m > 0 {
		f.Since = time.Now().Add(-time.Duration(m) * time.Minute)
	}
	evs, err := a.st.Activity(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, evs)
}

func (a *api) adminOverview(w http.ResponseWriter, r *http.Request) {
	projects, err := a.st.Projects(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"version":  Version,
		"now":      time.Now().UTC(),
		"you":      userFrom(r),
		"projects": projects,
		"auth":     a.requireAuth || (a.tokens != nil && a.tokens.Enabled()),
	})
}
