package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ideanix/openmind/internal/client"
	"github.com/ideanix/openmind/internal/store"
)

const workerPreamble = `You are running unattended as an OpenMind worker on this machine. A teammate assigned you the task below; nobody is watching, so do not ask questions: make reasonable assumptions and state them.

Rules:
- Work only inside the current directory.
- Do not commit, push, deploy, delete data, or touch credentials unless the task explicitly says so.
- If the OpenMind tools are available, call openmind_context first, and save durable learnings with openmind_put.

When you finish, end your reply with a report in this exact shape:

## Report
**Result:** done | partially done | blocked
**What I did:** …
**Files changed:** …
**How I verified it:** …
**Left to do / needs a human:** …

Task: `

// cmdWorker polls the server for tasks assigned to this token and runs each
// with a headless coding agent in --dir.
func cmdWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	server := fs.String("server", os.Getenv("OPENMIND_SERVER"), "server URL")
	token := fs.String("token", os.Getenv("OPENMIND_TOKEN"), "bearer token of this client")
	dir := fs.String("dir", ".", "directory the agent works in")
	interval := fs.Duration("interval", 10*time.Second, "how often to ask for work")
	timeout := fs.Duration("timeout", 45*time.Minute, "maximum time for one task")
	mode := fs.String("permission-mode", "acceptEdits", "Claude Code permission mode for unattended runs: acceptEdits (edits allowed, shell commands need --allow) | bypassPermissions (anything, use only in a sandbox) | plan (read-only analysis)")
	allow := fs.String("allow", "", `extra allowed tools, e.g. "Bash(go test:*),Bash(git status:*)"`)
	bin := fs.String("claude", "claude", "agent executable")
	once := fs.Bool("once", false, "run at most one task, then exit")
	fs.Parse(args)
	if *server == "" || *token == "" {
		return errors.New("usage: openmind worker --server URL --token T --dir PATH")
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("--dir %s is not a directory", abs)
	}
	if _, err := exec.LookPath(*bin); err != nil {
		return fmt.Errorf("%s not found in PATH", *bin)
	}
	c := client.New(*server, *token)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	who, err := c.Whoami(ctx)
	if err != nil {
		return fmt.Errorf("cannot reach the server: %w", err)
	}
	host, _ := os.Hostname()
	label := host + ":" + abs
	fmt.Printf("worker %q ready on %s, mode %s, polling every %s. Ctrl+C to stop.\n", who, label, *mode, *interval)

	for {
		var t store.Task
		got, err := claim(ctx, c, label, &t)
		switch {
		case err != nil:
			fmt.Fprintln(os.Stderr, "claim:", err)
		case got:
			fmt.Printf("%s  task %s: %s\n", time.Now().Format("15:04:05"), t.ID, t.Title)
			status, report := runTask(ctx, c, &t, abs, *bin, *mode, *allow, *timeout)
			if err := c.Do(context.WithoutCancel(ctx), "POST", "/api/v1/tasks/"+t.ID+"/report", map[string]string{"status": status, "report": report}, nil); err != nil {
				fmt.Fprintln(os.Stderr, "report:", err)
			}
			fmt.Printf("%s  task %s → %s\n", time.Now().Format("15:04:05"), t.ID, status)
			if *once {
				return nil
			}
			continue
		case *once:
			fmt.Println("no queued tasks")
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}

func claim(ctx context.Context, c *client.Client, label string, t *store.Task) (bool, error) {
	var raw string
	if err := c.Do(ctx, "POST", "/api/v1/tasks/claim", map[string]string{"worker": label}, &raw); err != nil {
		return false, err
	}
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	return true, jsonUnmarshal(raw, t)
}

func runTask(ctx context.Context, c *client.Client, t *store.Task, dir, bin, mode, allow string, timeout time.Duration) (status, report string) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Heartbeat doubles as the cancel channel: the server answers
	// continue=false once the task is cancelled from the control desk.
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-tick.C:
				var out struct {
					Continue bool `json:"continue"`
				}
				if err := c.Do(runCtx, "POST", "/api/v1/tasks/"+t.ID+"/heartbeat", nil, &out); err == nil && !out.Continue {
					cancel()
					return
				}
			}
		}
	}()

	before := gitState(dir)
	args := []string{"-p", workerPreamble + t.Title + "\n\n" + t.Prompt, "--permission-mode", mode}
	if allow != "" {
		args = append(args, "--allowedTools", allow)
	}
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "OPENMIND_TASK_ID="+t.ID)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start).Round(time.Second)

	var b strings.Builder
	b.WriteString(strings.TrimSpace(out.String()))
	status = store.TaskDone
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		status = store.TaskFailed
		fmt.Fprintf(&b, "\n\n**Worker:** stopped after the %s time limit.", timeout)
	case runCtx.Err() != nil:
		status = store.TaskFailed
		b.WriteString("\n\n**Worker:** stopped: the task was cancelled or the worker was shut down.")
	case err != nil:
		status = store.TaskFailed
		fmt.Fprintf(&b, "\n\n**Worker:** the agent exited with an error: %v\n\n```\n%s\n```", err, tail(errb.String(), 2000))
	}
	fmt.Fprintf(&b, "\n\n---\n_Worker %s · %s · mode %s · took %s_\n", t.Worker, dir, mode, took)
	if after := gitState(dir); after != "" && after != before {
		fmt.Fprintf(&b, "\n**Working tree after the run:**\n```\n%s\n```\n", after)
	}
	return status, b.String()
}

// gitState summarizes uncommitted changes, or "" outside a repository.
func gitState(dir string) string {
	cmd := exec.Command("git", "status", "--short")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return tail(strings.TrimRight(string(out), "\n"), 4000)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
