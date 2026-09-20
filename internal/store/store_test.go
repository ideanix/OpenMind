package store

import (
	"context"
	"path/filepath"
	"strings"
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

func TestActivity(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	s.LogActivity(ctx, Event{User: "laptop2", Project: "logdoc", Action: "openmind_search", Kind: KindRead, Detail: "redis", Remote: "192.168.1.7", Agent: "claude-code"})
	s.LogActivity(ctx, Event{User: "laptop2", Project: "logdoc", Action: "openmind_put", Kind: KindWrite, Detail: "Tests need Redis", NoteID: "n1"})
	s.LogActivity(ctx, Event{User: "?", Action: "auth", Kind: KindDenied, Detail: "invalid token"})
	evs, err := s.Activity(ctx, ActivityFilter{User: "laptop2"})
	if err != nil || len(evs) != 2 || evs[0].Action != "openmind_put" {
		t.Fatalf("activity: %v %+v", err, evs)
	}
	tail, _ := s.Activity(ctx, ActivityFilter{AfterID: evs[0].ID})
	if len(tail) != 1 || tail[0].Kind != KindDenied {
		t.Fatalf("tail: %+v", tail)
	}
	stats, err := s.ClientStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := stats["laptop2"]
	if c == nil || c.Reads24h != 1 || c.Writes24h != 1 || c.Last == nil || c.Last.Action != "openmind_put" {
		t.Fatalf("stats: %+v", c)
	}
}

func TestTaskLifecycle(t *testing.T) {
	s := open(t)
	ctx := context.Background()
	all := func(string) bool { return true }
	if got, _ := s.ClaimTask(ctx, "laptop2", "w", all); got != nil {
		t.Fatal("claimed from an empty queue")
	}
	t1, err := s.CreateTask(ctx, &Task{Project: "logdoc", Prompt: "Add a healthcheck endpoint\nand a test", Assignee: "laptop2", CreatedBy: "inerc"})
	if err != nil || t1.Title != "Add a healthcheck endpoint" {
		t.Fatalf("create: %v %+v", err, t1)
	}
	if _, err := s.CreateTask(ctx, &Task{Project: "logdoc", Prompt: "use AKIAIOSFODNN7EXAMPLE", Assignee: "laptop2"}); err == nil {
		t.Fatal("secret in prompt accepted")
	}
	// A worker without a grant for the project gets nothing; another user's worker too.
	if got, _ := s.ClaimTask(ctx, "laptop2", "w", func(string) bool { return false }); got != nil {
		t.Fatal("claimed without project grant")
	}
	if got, _ := s.ClaimTask(ctx, "someone-else", "w", all); got != nil {
		t.Fatal("claimed another assignee's task")
	}
	got, err := s.ClaimTask(ctx, "laptop2", "mac2:/repo", all)
	if err != nil || got == nil || got.ID != t1.ID || got.Status != TaskRunning {
		t.Fatalf("claim: %v %+v", err, got)
	}
	if again, _ := s.ClaimTask(ctx, "laptop2", "w", all); again != nil {
		t.Fatal("task claimed twice")
	}
	if alive, _ := s.HeartbeatTask(ctx, t1.ID, "laptop2"); !alive {
		t.Fatal("heartbeat on running task failed")
	}
	done, err := s.FinishTask(ctx, t1.ID, "laptop2", TaskDone, "did it; key AKIAIOSFODNN7EXAMPLE leaked in output")
	if err != nil || done.Status != TaskDone || strings.Contains(done.Report, "AKIA") {
		t.Fatalf("finish: %v %+v", err, done)
	}
	if _, err := s.FinishTask(ctx, t1.ID, "laptop2", TaskDone, "again"); err != ErrTaskState {
		t.Fatalf("double finish: %v", err)
	}
	// cancel → heartbeat says stop
	t2, _ := s.CreateTask(ctx, &Task{Project: "logdoc", Prompt: "second", Assignee: "laptop2"})
	s.ClaimTask(ctx, "laptop2", "w", all)
	if _, err := s.CancelTask(ctx, t2.ID); err != nil {
		t.Fatal(err)
	}
	if alive, _ := s.HeartbeatTask(ctx, t2.ID, "laptop2"); alive {
		t.Fatal("cancelled task still reported alive")
	}
	if r, err := s.RequeueTask(ctx, t2.ID); err != nil || r.Status != TaskQueued {
		t.Fatalf("requeue: %v", err)
	}
}
