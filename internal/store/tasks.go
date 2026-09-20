package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/secrets"
)

// Task statuses.
const (
	TaskQueued    = "queued"
	TaskRunning   = "running"
	TaskDone      = "done"
	TaskFailed    = "failed"
	TaskCancelled = "cancelled"
)

// Task is a unit of work one user assigns to a client machine. The client's
// worker claims it, runs a coding agent on it, and posts a report.
type Task struct {
	ID        string    `json:"id"`
	Project   string    `json:"project"`
	Title     string    `json:"title"`
	Prompt    string    `json:"prompt"`
	Assignee  string    `json:"assignee"`
	CreatedBy string    `json:"created_by"`
	Status    string    `json:"status"`
	Report    string    `json:"report"`
	Created   time.Time `json:"created"`
	Claimed   time.Time `json:"claimed,omitzero"`
	Heartbeat time.Time `json:"heartbeat,omitzero"`
	Finished  time.Time `json:"finished,omitzero"`
	Worker    string    `json:"worker"` // host and directory the worker reported
}

// ErrTaskState is returned when a transition is not allowed from the task's
// current status.
var ErrTaskState = errors.New("task is not in a state that allows this")

func (s *Store) migrateTasks() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		project TEXT NOT NULL,
		title TEXT NOT NULL,
		prompt TEXT NOT NULL,
		assignee TEXT NOT NULL,
		created_by TEXT NOT NULL,
		status TEXT NOT NULL,
		report TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		claimed TEXT NOT NULL DEFAULT '',
		heartbeat TEXT NOT NULL DEFAULT '',
		finished TEXT NOT NULL DEFAULT '',
		worker TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS tasks_queue ON tasks(assignee, status, created)`)
	return err
}

const taskCols = `id, project, title, prompt, assignee, created_by, status, report, created, claimed, heartbeat, finished, worker`

func scanTask(r scanner) (*Task, error) {
	var t Task
	var created, claimed, hb, finished string
	if err := r.Scan(&t.ID, &t.Project, &t.Title, &t.Prompt, &t.Assignee, &t.CreatedBy, &t.Status, &t.Report, &created, &claimed, &hb, &finished, &t.Worker); err != nil {
		return nil, err
	}
	t.Created = parseTime(created)
	t.Claimed = parseTime(claimed)
	t.Heartbeat = parseTime(hb)
	t.Finished = parseTime(finished)
	return &t, nil
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// CreateTask queues a task. Prompts pass the same secret scanner as notes.
func (s *Store) CreateTask(ctx context.Context, t *Task) (*Task, error) {
	t.Project, t.Title, t.Prompt, t.Assignee = strings.TrimSpace(t.Project), strings.TrimSpace(t.Title), strings.TrimSpace(t.Prompt), strings.TrimSpace(t.Assignee)
	switch {
	case t.Project == "":
		return nil, errors.New("task: project is required")
	case t.Prompt == "":
		return nil, errors.New("task: prompt is required")
	case t.Assignee == "":
		return nil, errors.New("task: assignee is required")
	}
	if t.Title == "" {
		t.Title = firstLine(t.Prompt)
		if r := []rune(t.Title); len(r) > 80 {
			t.Title = string(r[:80]) + "…"
		}
	}
	if err := secrets.Scan(t.Title + "\n" + t.Prompt); err != nil {
		return nil, err
	}
	t.ID = note.NewID()
	t.Status = TaskQueued
	t.Created = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO tasks(id, project, title, prompt, assignee, created_by, status, created) VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, t.Project, t.Title, t.Prompt, t.Assignee, t.CreatedBy, t.Status, t.Created.Format(time.RFC3339Nano))
	return t, err
}

// GetTask returns one task.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// TaskFilter narrows ListTasks.
type TaskFilter struct {
	Project  string
	Assignee string
	Status   string
	Limit    int
}

// ListTasks returns tasks newest first.
func (s *Store) ListTasks(ctx context.Context, f TaskFilter) ([]*Task, error) {
	var w []string
	var args []any
	for col, v := range map[string]string{"project": f.Project, "assignee": f.Assignee, "status": f.Status} {
		if v != "" {
			w = append(w, col+" = ?")
			args = append(args, v)
		}
	}
	q := `SELECT ` + taskCols + ` FROM tasks`
	if len(w) > 0 {
		q += " WHERE " + strings.Join(w, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY created DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ClaimTask atomically hands the oldest queued task for assignee (within
// the allowed projects) to a worker. It returns nil when the queue is empty.
func (s *Store) ClaimTask(ctx context.Context, assignee, worker string, allowed func(project string) bool) (*Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE assignee = ? AND status = ? ORDER BY created`, assignee, TaskQueued)
	if err != nil {
		return nil, err
	}
	var pick *Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if allowed(t.Project) {
			pick = t
			break
		}
	}
	rows.Close()
	if pick == nil {
		return nil, nil
	}
	ts := now()
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET status = ?, claimed = ?, heartbeat = ?, worker = ? WHERE id = ? AND status = ?`,
		TaskRunning, ts, ts, worker, pick.ID, TaskQueued); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	pick.Status, pick.Worker = TaskRunning, worker
	pick.Claimed, pick.Heartbeat = parseTime(ts), parseTime(ts)
	return pick, nil
}

// HeartbeatTask marks a running task as alive and reports whether it is
// still wanted (false once cancelled).
func (s *Store) HeartbeatTask(ctx context.Context, id, assignee string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET heartbeat = ? WHERE id = ? AND assignee = ? AND status = ?`, now(), id, assignee, TaskRunning)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FinishTask stores the worker's report. Secrets in the report are redacted
// rather than rejected, so a report is never lost.
func (s *Store) FinishTask(ctx context.Context, id, assignee, status, report string) (*Task, error) {
	if status != TaskDone && status != TaskFailed {
		return nil, errors.New("task: status must be done or failed")
	}
	if len(report) > 256*1024 {
		report = report[:256*1024] + "\n\n_(report truncated)_"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, report = ?, finished = ? WHERE id = ? AND assignee = ? AND status = ?`,
		status, secrets.Redact(report), now(), id, assignee, TaskRunning)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrTaskState
	}
	return s.GetTask(ctx, id)
}

// CancelTask stops a queued or running task.
func (s *Store) CancelTask(ctx context.Context, id string) (*Task, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, finished = ? WHERE id = ? AND status IN (?, ?)`,
		TaskCancelled, now(), id, TaskQueued, TaskRunning)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrTaskState
	}
	return s.GetTask(ctx, id)
}

// RequeueTask puts a finished, failed, cancelled or stuck task back in the queue.
func (s *Store) RequeueTask(ctx context.Context, id string) (*Task, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tasks SET status = ?, claimed = '', heartbeat = '', finished = '', worker = '' WHERE id = ? AND status != ?`,
		TaskQueued, id, TaskQueued)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrTaskState
	}
	return s.GetTask(ctx, id)
}
