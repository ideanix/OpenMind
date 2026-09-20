package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/ideanix/openmind/internal/auth"
	"github.com/ideanix/openmind/internal/store"
)

// sessions holds browser sessions for the control desk. They live in memory
// only: a server restart signs everyone out, and nothing secret is persisted.
//
// Sign-in flow: `openmind ui` asks for a one-time code with the owner token,
// opens /ui?code=…, and the page exchanges the code for a session token. The
// code is single-use and short-lived, so it is harmless in browser history.
type sessions struct {
	mu    sync.Mutex
	codes map[string]grant // hash(code) → identity
	live  map[string]grant // hash(session token) → identity
}

type grant struct {
	id      auth.Identity
	expires time.Time
}

const (
	codeTTL    = 60 * time.Second
	sessionTTL = 12 * time.Hour
)

func newSessions() *sessions {
	return &sessions{codes: map[string]grant{}, live: map[string]grant{}}
}

func (s *sessions) sweep(now time.Time) {
	for k, g := range s.codes {
		if now.After(g.expires) {
			delete(s.codes, k)
		}
	}
	for k, g := range s.live {
		if now.After(g.expires) {
			delete(s.live, k)
		}
	}
}

func (s *sessions) newCode(id auth.Identity) (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.sweep(now)
	code := auth.NewSecret()
	exp := now.Add(codeTTL)
	s.codes[auth.Hash(code)] = grant{id: id, expires: exp}
	return code, exp
}

func (s *sessions) exchange(code string) (string, auth.Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.sweep(now)
	h := auth.Hash(code)
	g, ok := s.codes[h]
	if !ok {
		return "", auth.Identity{}, false
	}
	delete(s.codes, h)
	token := "s_" + auth.NewSecret() + auth.NewSecret()
	s.live[auth.Hash(token)] = grant{id: g.id, expires: now.Add(sessionTTL)}
	return token, g.id, true
}

func (s *sessions) resolve(token string) (auth.Identity, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.live[auth.Hash(token)]
	if !ok || time.Now().After(g.expires) {
		return auth.Identity{}, false
	}
	return g.id, true
}

// loginCode issues a one-time sign-in code for the caller's own identity.
func (a *api) loginCode(w http.ResponseWriter, r *http.Request) {
	code, exp := a.sessions.newCode(identityFrom(r))
	a.record(r, "login-code", store.KindWrite, "", "one-time sign-in code", "")
	writeJSON(w, 200, map[string]any{"code": code, "expires": exp.UTC()})
}

// sessionExchange trades a one-time code for a browser session. It is the
// only unauthenticated write endpoint; codes are 128-bit and single-use.
func (a *api) sessionExchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.Code == "" {
		writeErr(w, 400, errors.New("code is required"))
		return
	}
	token, id, ok := a.sessions.exchange(in.Code)
	if !ok {
		a.st.LogActivity(r.Context(), store.Event{User: "?", Action: "auth", Kind: store.KindDenied,
			Detail: "sign-in code expired or already used", Remote: remoteOf(r), Agent: r.UserAgent()})
		writeErr(w, http.StatusUnauthorized, errors.New("this sign-in link expired or was already used; run `openmind ui` again"))
		return
	}
	a.st.LogActivity(r.Context(), store.Event{User: id.User, Action: "sign-in", Kind: store.KindRead,
		Detail: "opened the control desk", Remote: remoteOf(r), Agent: r.UserAgent()})
	writeJSON(w, 200, map[string]any{"token": token, "user": id.User})
}
