package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ideanix/openmind/internal/note"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestPutSearchRevise(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	n, err := s.Put(ctx, &note.Note{Project: "logdoc", Title: "API tests need local Redis", Body: "Run make redis before go test.", Tags: []string{"Testing"}, Author: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if n.ID == "" || n.Revision != 1 {
		t.Fatalf("bad note %+v", n)
	}
	hits, err := s.Search(ctx, "redis test", Filter{Project: "logdoc", Viewer: "bob"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %v %d", err, len(hits))
	}
	if hits[0].Snippet == "" {
		t.Error("empty snippet")
	}
	n.Body = "Run make redis before go test ./..."
	n2, err := s.Put(ctx, n)
	if err != nil || n2.Revision != 2 {
		t.Fatalf("revise: %v %+v", err, n2)
	}
	h, _ := s.History(ctx, n.ID)
	if len(h) != 1 || h[0].Revision != 1 {
		t.Fatalf("history: %+v", h)
	}
	// tag filter
	hits, _ = s.Search(ctx, "", Filter{Project: "logdoc", Tags: []string{"testing"}, Viewer: "bob"})
	if len(hits) != 1 {
		t.Fatalf("tag filter: %d", len(hits))
	}
	// deprecate hides from default listing
	if _, err := s.Deprecate(ctx, n.ID, "alice", "no longer true"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(ctx, Filter{Project: "logdoc", Viewer: "bob"}); len(l) != 0 {
		t.Fatalf("deprecated still listed: %d", len(l))
	}
}

func TestPersonalScopeHidden(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	n, err := s.Put(ctx, &note.Note{Project: "p", Title: "mine", Body: "private thing", Scope: note.ScopePersonal, Author: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, n.ID, "bob"); err != ErrNotFound {
		t.Fatalf("bob sees alice's personal note: %v", err)
	}
	if hits, _ := s.Search(ctx, "private", Filter{Project: "p", Viewer: "bob"}); len(hits) != 0 {
		t.Fatal("search leaks personal note")
	}
	if _, err := s.Get(ctx, n.ID, "alice"); err != nil {
		t.Fatal(err)
	}
}

func TestSecretRejected(t *testing.T) {
	s := open(t)
	_, err := s.Put(context.Background(), &note.Note{Project: "p", Title: "key", Body: "AKIAIOSFODNN7EXAMPLE"})
	if err == nil {
		t.Fatal("secret accepted")
	}
}
