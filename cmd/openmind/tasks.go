package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/ideanix/openmind/internal/client"
	"github.com/ideanix/openmind/internal/store"
)

func jsonUnmarshal(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }

// ownerClient talks to the local server as the owner when no --server/--token
// is given, so task and ui commands work out of the box on the host machine.
func ownerClient(server, token string) (*client.Client, error) {
	if server == "" {
		server = "http://127.0.0.1:7777"
	}
	if token == "" {
		if b, err := os.ReadFile(homeDir() + "/owner.token"); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	return client.New(server, token), nil
}

func cmdTask(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: openmind task add --project P --to CLIENT \"prompt\" | list | show ID | cancel ID | requeue ID")
	}
	fs := flag.NewFlagSet("task", flag.ExitOnError)
	server := fs.String("server", os.Getenv("OPENMIND_SERVER"), "server URL (default: local server)")
	token := fs.String("token", os.Getenv("OPENMIND_TOKEN"), "bearer token (default: owner token)")
	project := fs.String("project", "", "project")
	to := fs.String("to", "", "client (token name) that should run the task")
	title := fs.String("title", "", "short title (default: first line of the prompt)")
	status := fs.String("status", "", "filter for list")
	sub := args[0]
	fs.Parse(args[1:])
	c, _ := ownerClient(*server, *token)
	ctx := context.Background()
	switch sub {
	case "add":
		prompt := strings.Join(fs.Args(), " ")
		if prompt == "" {
			p, err := readAll(os.Stdin)
			if err != nil {
				return err
			}
			prompt = p
		}
		var out store.Task
		if err := c.Do(ctx, "POST", "/api/v1/tasks", store.Task{Project: *project, Assignee: *to, Title: *title, Prompt: prompt}, &out); err != nil {
			return err
		}
		fmt.Printf("queued %s for %s: %s\n", out.ID, out.Assignee, out.Title)
	case "list":
		q := url.Values{}
		for k, v := range map[string]string{"project": *project, "assignee": *to, "status": *status} {
			if v != "" {
				q.Set(k, v)
			}
		}
		var out []store.Task
		if err := c.Do(ctx, "GET", "/api/v1/tasks?"+q.Encode(), nil, &out); err != nil {
			return err
		}
		for _, t := range out {
			fmt.Printf("%s  %-9s %-12s %-14s %s\n", t.ID, t.Status, t.Assignee, t.Project, trunc(t.Title, 60))
		}
	case "show", "cancel", "requeue":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: openmind task %s ID", sub)
		}
		method, path := "GET", "/api/v1/tasks/"+fs.Arg(0)
		if sub != "show" {
			method, path = "POST", path+"/"+sub
		}
		var t store.Task
		if err := c.Do(ctx, method, path, nil, &t); err != nil {
			return err
		}
		fmt.Printf("%s  %s  → %s (%s)\n%s\n", t.ID, t.Status, t.Assignee, t.Project, t.Title)
		if sub == "show" {
			fmt.Printf("\n--- prompt\n%s\n", t.Prompt)
			if t.Report != "" {
				fmt.Printf("\n--- report\n%s\n", t.Report)
			}
		}
	default:
		return fmt.Errorf("unknown task command %q", sub)
	}
	return nil
}

// cmdUI opens the control desk. It asks the server for a one-time sign-in
// code with the owner token and opens /ui?code=…; the page trades the code
// for an in-memory session, so the real token never reaches the browser.
func cmdUI(args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	server := fs.String("server", "http://localhost:7777", "server URL")
	token := fs.String("token", os.Getenv("OPENMIND_TOKEN"), "admin token (default: owner token)")
	print := fs.Bool("print", false, "print the sign-in URL instead of opening a browser")
	fs.Parse(args)
	c, _ := ownerClient(*server, *token)
	u := strings.TrimRight(*server, "/") + "/ui"
	var out struct {
		Code string `json:"code"`
	}
	if err := c.Do(context.Background(), "POST", "/api/v1/admin/login-code", nil, &out); err != nil {
		fmt.Fprintln(os.Stderr, "could not get a sign-in code ("+err.Error()+"); the desk will ask for a token")
	} else if out.Code != "" {
		u += "?code=" + url.QueryEscape(out.Code)
	}
	if *print {
		fmt.Println(u)
		return nil
	}
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	return exec.Command(opener, u).Start()
}
