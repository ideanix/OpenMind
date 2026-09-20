package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/ideanix/openmind/internal/store"
)

func (a *api) taskVisible(r *http.Request, id string) (*store.Task, error) {
	t, err := a.st.GetTask(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if !identityFrom(r).Allows(t.Project) {
		return nil, store.ErrNotFound
	}
	return t, nil
}

func taskStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 404
	case errors.Is(err, store.ErrTaskState):
		return 409
	}
	return 422
}

func (a *api) taskCreate(w http.ResponseWriter, r *http.Request) {
	var t store.Task
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&t); err != nil {
		writeErr(w, 400, err)
		return
	}
	if !a.allow(w, r, t.Project) {
		return
	}
	t.CreatedBy = userFrom(r)
	out, err := a.st.CreateTask(r.Context(), &t)
	if err != nil {
		writeErr(w, 422, err)
		return
	}
	a.record(r, "task:create", store.KindWrite, out.Project, out.Title+" → "+out.Assignee, out.ID)
	writeJSON(w, 200, out)
}

func (a *api) taskList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TaskFilter{Project: q.Get("project"), Assignee: q.Get("assignee"), Status: q.Get("status")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	tasks, err := a.st.ListTasks(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	id := identityFrom(r)
	out := tasks[:0]
	for _, t := range tasks {
		if id.Allows(t.Project) {
			out = append(out, t)
		}
	}
	writeJSON(w, 200, out)
}

func (a *api) taskGet(w http.ResponseWriter, r *http.Request) {
	t, err := a.taskVisible(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, taskStatus(err), err)
		return
	}
	writeJSON(w, 200, t)
}

// taskClaim hands the caller its next queued task, or 204 when idle.
func (a *api) taskClaim(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker string `json:"worker"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in)
	id := identityFrom(r)
	t, err := a.st.ClaimTask(r.Context(), id.User, in.Worker, id.Allows)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if t == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.record(r, "task:claim", store.KindWrite, t.Project, t.Title, t.ID)
	writeJSON(w, 200, t)
}

func (a *api) taskHeartbeat(w http.ResponseWriter, r *http.Request) {
	alive, err := a.st.HeartbeatTask(r.Context(), r.PathValue("id"), userFrom(r))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"continue": alive})
}

func (a *api) taskReport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Status string `json:"status"`
		Report string `json:"report"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	t, err := a.st.FinishTask(r.Context(), r.PathValue("id"), userFrom(r), in.Status, in.Report)
	if err != nil {
		writeErr(w, taskStatus(err), err)
		return
	}
	a.record(r, "task:report", store.KindWrite, t.Project, t.Title+" → "+t.Status, t.ID)
	writeJSON(w, 200, t)
}

func (a *api) taskCancel(w http.ResponseWriter, r *http.Request) {
	a.taskTransition(w, r, "task:cancel", a.st.CancelTask)
}

func (a *api) taskRequeue(w http.ResponseWriter, r *http.Request) {
	a.taskTransition(w, r, "task:requeue", a.st.RequeueTask)
}

func (a *api) taskTransition(w http.ResponseWriter, r *http.Request, action string, fn func(ctx ctxT, id string) (*store.Task, error)) {
	if _, err := a.taskVisible(r, r.PathValue("id")); err != nil {
		writeErr(w, taskStatus(err), err)
		return
	}
	t, err := fn(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, taskStatus(err), err)
		return
	}
	a.record(r, action, store.KindWrite, t.Project, t.Title, t.ID)
	writeJSON(w, 200, t)
}
