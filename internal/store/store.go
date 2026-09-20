// Package store persists notes in SQLite with FTS5 search and revision history.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/secrets"
)

// ErrNotFound is returned when a note id does not exist.
var ErrNotFound = errors.New("note not found")

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies migrations.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS notes (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			title TEXT NOT NULL,
			type TEXT NOT NULL,
			scope TEXT NOT NULL,
			tags TEXT NOT NULL DEFAULT '[]',
			author TEXT NOT NULL DEFAULT '',
			source_tool TEXT NOT NULL DEFAULT '',
			source_session TEXT NOT NULL DEFAULT '',
			source_model TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			body TEXT NOT NULL,
			created TEXT NOT NULL,
			updated TEXT NOT NULL,
			revision INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE INDEX IF NOT EXISTS notes_project_status ON notes(project, status, updated)`,
		`CREATE TABLE IF NOT EXISTS note_revisions (
			note_id TEXT NOT NULL,
			revision INTEGER NOT NULL,
			snapshot TEXT NOT NULL,
			author TEXT NOT NULL DEFAULT '',
			created TEXT NOT NULL,
			PRIMARY KEY (note_id, revision)
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(
			title, body, tags, content='notes', content_rowid='rowid', tokenize='unicode61'
		)`,
		`CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes BEGIN
			INSERT INTO notes_fts(rowid, title, body, tags) VALUES (new.rowid, new.title, new.body, new.tags);
		END`,
		`CREATE TRIGGER IF NOT EXISTS notes_ad AFTER DELETE ON notes BEGIN
			INSERT INTO notes_fts(notes_fts, rowid, title, body, tags) VALUES ('delete', old.rowid, old.title, old.body, old.tags);
		END`,
		`CREATE TRIGGER IF NOT EXISTS notes_au AFTER UPDATE ON notes BEGIN
			INSERT INTO notes_fts(notes_fts, rowid, title, body, tags) VALUES ('delete', old.rowid, old.title, old.body, old.tags);
			INSERT INTO notes_fts(rowid, title, body, tags) VALUES (new.rowid, new.title, new.body, new.tags);
		END`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w\n%s", err, q)
		}
	}
	if err := s.migrateActivity(); err != nil {
		return err
	}
	return s.migrateTasks()
}

// Filter narrows List and Search.
type Filter struct {
	Project string
	Type    string
	Scope   string
	Status  string // default "active"; "any" disables the filter
	Tags    []string
	Since   time.Time
	Limit   int
	// Viewer is the requesting user; personal notes of other authors are hidden.
	Viewer string
}

func (f *Filter) where(args *[]any) string {
	var w []string
	if f.Project != "" {
		w = append(w, "project = ?")
		*args = append(*args, f.Project)
	}
	if f.Type != "" {
		w = append(w, "type = ?")
		*args = append(*args, f.Type)
	}
	if f.Scope != "" {
		w = append(w, "scope = ?")
		*args = append(*args, f.Scope)
	}
	st := f.Status
	if st == "" {
		st = note.StatusActive
	}
	if st != "any" {
		w = append(w, "status = ?")
		*args = append(*args, st)
	}
	if !f.Since.IsZero() {
		w = append(w, "updated >= ?")
		*args = append(*args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	w = append(w, "(scope != 'personal' OR author = ?)")
	*args = append(*args, f.Viewer)
	for _, t := range f.Tags {
		w = append(w, "tags LIKE ?")
		*args = append(*args, `%"`+strings.ToLower(t)+`"%`)
	}
	return strings.Join(w, " AND ")
}

func (f *Filter) limit() int {
	if f.Limit <= 0 || f.Limit > 500 {
		return 50
	}
	return f.Limit
}

// Put creates a note when its ID is empty or unknown, otherwise updates it
// and records the previous version as a revision.
func (s *Store) Put(ctx context.Context, n *note.Note) (*note.Note, error) {
	if err := n.Normalize(); err != nil {
		return nil, err
	}
	if err := secrets.Scan(n.Title + "\n" + n.Body); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	tags, _ := json.Marshal(nonNil(n.Tags))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var existing *note.Note
	if n.ID != "" {
		existing, err = getTx(ctx, tx, n.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	if existing == nil {
		if n.ID == "" {
			n.ID = note.NewID()
		}
		if n.Created.IsZero() {
			n.Created = now
		}
		n.Updated = now
		n.Revision = 1
		_, err = tx.ExecContext(ctx, `INSERT INTO notes
			(id, project, title, type, scope, tags, author, source_tool, source_session, source_model, status, body, created, updated, revision)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			n.ID, n.Project, n.Title, n.Type, n.Scope, string(tags), n.Author,
			n.Source.Tool, n.Source.Session, n.Source.Model, n.Status, n.Body,
			n.Created.Format(time.RFC3339Nano), n.Updated.Format(time.RFC3339Nano), n.Revision)
		if err != nil {
			return nil, err
		}
	} else {
		snap, _ := json.Marshal(existing)
		if _, err = tx.ExecContext(ctx, `INSERT INTO note_revisions(note_id, revision, snapshot, author, created) VALUES (?,?,?,?,?)`,
			existing.ID, existing.Revision, string(snap), existing.Author, existing.Updated.Format(time.RFC3339Nano)); err != nil {
			return nil, err
		}
		n.Created = existing.Created
		n.Updated = now
		n.Revision = existing.Revision + 1
		if n.Author == "" {
			n.Author = existing.Author
		}
		_, err = tx.ExecContext(ctx, `UPDATE notes SET project=?, title=?, type=?, scope=?, tags=?, author=?,
			source_tool=?, source_session=?, source_model=?, status=?, body=?, updated=?, revision=? WHERE id=?`,
			n.Project, n.Title, n.Type, n.Scope, string(tags), n.Author,
			n.Source.Tool, n.Source.Session, n.Source.Model, n.Status, n.Body,
			n.Updated.Format(time.RFC3339Nano), n.Revision, n.ID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return n, nil
}

// Get returns one note. Personal notes of other authors are hidden.
func (s *Store) Get(ctx context.Context, id, viewer string) (*note.Note, error) {
	n, err := getTx(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if n.Scope == note.ScopePersonal && n.Author != viewer {
		return nil, ErrNotFound
	}
	return n, nil
}

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

const cols = `id, project, title, type, scope, tags, author, source_tool, source_session, source_model, status, body, created, updated, revision`

func getTx(ctx context.Context, q queryer, id string) (*note.Note, error) {
	row := q.QueryRowContext(ctx, `SELECT `+cols+` FROM notes WHERE id = ?`, id)
	n, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return n, err
}

type scanner interface{ Scan(dest ...any) error }

func scan(r scanner) (*note.Note, error) {
	var n note.Note
	var tags, created, updated string
	err := r.Scan(&n.ID, &n.Project, &n.Title, &n.Type, &n.Scope, &tags, &n.Author,
		&n.Source.Tool, &n.Source.Session, &n.Source.Model, &n.Status, &n.Body, &created, &updated, &n.Revision)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(tags), &n.Tags)
	n.Created, _ = time.Parse(time.RFC3339Nano, created)
	n.Updated, _ = time.Parse(time.RFC3339Nano, updated)
	return &n, nil
}

// List returns notes matching the filter, newest first.
func (s *Store) List(ctx context.Context, f Filter) ([]*note.Note, error) {
	var args []any
	where := f.where(&args)
	args = append(args, f.limit())
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM notes WHERE `+where+` ORDER BY updated DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collect(rows)
}

// Hit is a search result with a snippet.
type Hit struct {
	Note    *note.Note `json:"note"`
	Snippet string     `json:"snippet"`
	Score   float64    `json:"score"`
}

// Search runs a full-text query. The query is tokenized and each token is
// matched as a prefix, so "redis test" finds "Redis tests".
func (s *Store) Search(ctx context.Context, query string, f Filter) ([]Hit, error) {
	match := ftsQuery(query)
	if match == "" {
		notes, err := s.List(ctx, f)
		if err != nil {
			return nil, err
		}
		hits := make([]Hit, 0, len(notes))
		for _, n := range notes {
			hits = append(hits, Hit{Note: n, Snippet: firstLine(n.Body)})
		}
		return hits, nil
	}
	var args []any
	args = append(args, match)
	where := f.where(&args)
	args = append(args, f.limit())
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixed(cols, "n.")+`,
			snippet(notes_fts, 1, '', '', ' … ', 24) AS snip, bm25(notes_fts, 4.0, 1.0, 2.0) AS score
		FROM notes_fts JOIN notes n ON n.rowid = notes_fts.rowid
		WHERE notes_fts MATCH ? AND `+where+`
		ORDER BY score LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var n note.Note
		var tags, created, updated, snip string
		var score float64
		if err := rows.Scan(&n.ID, &n.Project, &n.Title, &n.Type, &n.Scope, &tags, &n.Author,
			&n.Source.Tool, &n.Source.Session, &n.Source.Model, &n.Status, &n.Body, &created, &updated, &n.Revision,
			&snip, &score); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &n.Tags)
		n.Created, _ = time.Parse(time.RFC3339Nano, created)
		n.Updated, _ = time.Parse(time.RFC3339Nano, updated)
		hits = append(hits, Hit{Note: &n, Snippet: oneLine(snip), Score: -score})
	}
	return hits, rows.Err()
}

// Deprecate marks a note obsolete, appending the reason to its body.
func (s *Store) Deprecate(ctx context.Context, id, viewer, reason string) (*note.Note, error) {
	n, err := s.Get(ctx, id, viewer)
	if err != nil {
		return nil, err
	}
	n.Status = note.StatusDeprecated
	if reason != "" {
		n.Body += "\n\n**Deprecated:** " + strings.TrimSpace(reason)
	}
	return s.Put(ctx, n)
}

// History returns previous versions of a note, oldest first.
func (s *Store) History(ctx context.Context, id string) ([]*note.Note, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT snapshot FROM note_revisions WHERE note_id = ? ORDER BY revision`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*note.Note
	for rows.Next() {
		var snap string
		if err := rows.Scan(&snap); err != nil {
			return nil, err
		}
		var n note.Note
		if err := json.Unmarshal([]byte(snap), &n); err != nil {
			return nil, err
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// Projects lists project names with active note counts.
func (s *Store) Projects(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project, COUNT(*) FROM notes WHERE status = 'active' GROUP BY project`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var c int
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		out[p] = c
	}
	return out, rows.Err()
}

func collect(rows *sql.Rows) ([]*note.Note, error) {
	var out []*note.Note
	for rows.Next() {
		n, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func prefixed(cols, p string) string {
	parts := strings.Split(cols, ", ")
	for i := range parts {
		parts[i] = p + parts[i]
	}
	return strings.Join(parts, ", ")
}

// oneLine collapses whitespace so snippets render on a single line.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// ftsQuery turns free text into a safe FTS5 expression: every token becomes
// a quoted prefix term joined by implicit AND.
func ftsQuery(q string) string {
	var terms []string
	for _, t := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || isAlnum(r))
	}) {
		t = strings.Trim(t, "._-")
		if t == "" {
			continue
		}
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

func isAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127
}
