package store

import (
	"context"
	"strings"
	"sync/atomic"
	"time"
)

// Kinds of activity events.
const (
	KindRead   = "read"
	KindWrite  = "write"
	KindDenied = "denied"
	KindError  = "error"
)

// Event is one recorded operation: who did what, from where.
type Event struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	User    string    `json:"user"`
	Project string    `json:"project"`
	Action  string    `json:"action"` // e.g. "openmind_search", "rest:list", "auth"
	Kind    string    `json:"kind"`   // read | write | denied | error
	Detail  string    `json:"detail"` // query, note title, or reason
	NoteID  string    `json:"note_id,omitempty"`
	Remote  string    `json:"remote"` // IP, or "stdio"
	Agent   string    `json:"agent"`  // client user agent
}

const maxActivityRows = 50000

var activityInserts atomic.Int64

func (s *Store) migrateActivity() error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS activity (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			ts TEXT NOT NULL,
			user TEXT NOT NULL,
			project TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			kind TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			note_id TEXT NOT NULL DEFAULT '',
			remote TEXT NOT NULL DEFAULT '',
			agent TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS activity_user_ts ON activity(user, ts)`,
		`CREATE INDEX IF NOT EXISTS activity_ts ON activity(ts)`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// LogActivity records an event. Failures are swallowed: auditing must never
// break the operation being audited.
func (s *Store) LogActivity(ctx context.Context, e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if r := []rune(e.Detail); len(r) > 200 {
		e.Detail = string(r[:200]) + "…"
	}
	e.Detail = strings.Join(strings.Fields(e.Detail), " ")
	_, err := s.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO activity(ts, user, project, action, kind, detail, note_id, remote, agent) VALUES (?,?,?,?,?,?,?,?,?)`,
		e.Time.UTC().Format(time.RFC3339Nano), e.User, e.Project, e.Action, e.Kind, e.Detail, e.NoteID, e.Remote, e.Agent)
	if err != nil {
		return
	}
	if activityInserts.Add(1)%500 == 0 {
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM activity WHERE id <= (SELECT MAX(id) FROM activity) - ?`, maxActivityRows)
	}
}

// ActivityFilter narrows Activity.
type ActivityFilter struct {
	User    string
	Project string
	AfterID int64     // only events newer than this id (for live tailing)
	Since   time.Time // only events at or after this time
	Limit   int
}

// Activity returns events newest first.
func (s *Store) Activity(ctx context.Context, f ActivityFilter) ([]Event, error) {
	var w []string
	var args []any
	if f.User != "" {
		w = append(w, "user = ?")
		args = append(args, f.User)
	}
	if f.Project != "" {
		w = append(w, "project = ?")
		args = append(args, f.Project)
	}
	if f.AfterID > 0 {
		w = append(w, "id > ?")
		args = append(args, f.AfterID)
	}
	if !f.Since.IsZero() {
		w = append(w, "ts >= ?")
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	q := `SELECT id, ts, user, project, action, kind, detail, note_id, remote, agent FROM activity`
	if len(w) > 0 {
		q += " WHERE " + strings.Join(w, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 5000 {
		limit = 200
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var ts string
		if err := rows.Scan(&e.ID, &ts, &e.User, &e.Project, &e.Action, &e.Kind, &e.Detail, &e.NoteID, &e.Remote, &e.Agent); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ClientStat summarizes one user's activity.
type ClientStat struct {
	User      string    `json:"user"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Reads24h  int       `json:"reads_24h"`
	Writes24h int       `json:"writes_24h"`
	Denied24h int       `json:"denied_24h"`
	Last      *Event    `json:"last,omitempty"`
}

// ClientStats returns one summary per user that ever produced an event.
func (s *Store) ClientStats(ctx context.Context) (map[string]*ClientStat, error) {
	day := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, `SELECT user, MIN(ts), MAX(ts),
			SUM(CASE WHEN kind='read'   AND ts >= ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN kind='write'  AND ts >= ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN kind='denied' AND ts >= ? THEN 1 ELSE 0 END),
			MAX(id)
		FROM activity GROUP BY user`, day, day, day)
	if err != nil {
		return nil, err
	}
	out := map[string]*ClientStat{}
	lastIDs := map[int64]string{}
	for rows.Next() {
		var c ClientStat
		var first, last string
		var lastID int64
		if err := rows.Scan(&c.User, &first, &last, &c.Reads24h, &c.Writes24h, &c.Denied24h, &lastID); err != nil {
			rows.Close()
			return nil, err
		}
		c.FirstSeen, _ = time.Parse(time.RFC3339Nano, first)
		c.LastSeen, _ = time.Parse(time.RFC3339Nano, last)
		out[c.User] = &c
		lastIDs[lastID] = c.User
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, user := range lastIDs {
		evs, err := s.Activity(ctx, ActivityFilter{AfterID: id - 1, Limit: 1, User: user})
		if err == nil && len(evs) == 1 {
			e := evs[0]
			out[user].Last = &e
		}
	}
	return out, nil
}
