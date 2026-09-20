package httpapi

import (
	"testing"

	"github.com/ideanix/openmind/internal/auth"
)

func TestSessionCodeIsSingleUse(t *testing.T) {
	s := newSessions()
	owner := auth.Identity{User: "owner", Projects: []string{"*"}}
	code, _ := s.newCode(owner)
	token, id, ok := s.exchange(code)
	if !ok || id.User != "owner" || token == "" {
		t.Fatalf("exchange: %v %+v", ok, id)
	}
	if _, _, again := s.exchange(code); again {
		t.Fatal("code accepted twice")
	}
	if got, ok := s.resolve(token); !ok || !got.Admin() {
		t.Fatalf("session not resolved: %+v", got)
	}
	if _, ok := s.resolve("s_forged"); ok {
		t.Fatal("forged session accepted")
	}
	if _, _, ok := s.exchange("never-issued"); ok {
		t.Fatal("unknown code accepted")
	}
}
