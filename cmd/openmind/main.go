// Command openmind is a self-hosted shared memory server for AI coding agents.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ideanix/openmind/internal/client"
	"github.com/ideanix/openmind/internal/digest"
	"github.com/ideanix/openmind/internal/httpapi"
	"github.com/ideanix/openmind/internal/mcpserver"
	"github.com/ideanix/openmind/internal/note"
	"github.com/ideanix/openmind/internal/store"
)

var version = "dev"

const usage = `openmind — shared memory for AI coding agents

Server:
  openmind serve   [--addr :7777] [--db PATH] [--tokens name:secret,...]
  openmind mcp     [--db PATH] [--author NAME]        MCP over stdio, local database

Notes (local --db or remote --server URL --token T):
  openmind put     --project P --title T [--type ..] [--scope ..] [--tags a,b] (body from stdin or --body)
  openmind search  --project P QUERY
  openmind list    --project P [--status any]
  openmind get     ID
  openmind context --project P
  openmind projects

Import / export:
  openmind import claude-code --project P [--dir ~/.claude/projects/<x>/memory]
  openmind export claude-md   --project P [--out CLAUDE.md]

Setup:
  openmind setup claude-code  --server URL --token T          prints the claude mcp add command

Environment: OPENMIND_DB, OPENMIND_SERVER, OPENMIND_TOKEN, OPENMIND_TOKENS, OPENMIND_AUTHOR
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	mcpserver.Version = version
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "mcp":
		err = cmdMCP(os.Args[2:])
	case "put":
		err = cmdPut(os.Args[2:])
	case "search":
		err = cmdSearch(os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "get":
		err = cmdGet(os.Args[2:])
	case "context":
		err = cmdContext(os.Args[2:])
	case "projects":
		err = cmdProjects(os.Args[2:])
	case "import":
		err = cmdImport(os.Args[2:])
	case "export":
		err = cmdExport(os.Args[2:])
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("openmind", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- backend selection ----------------------------------------------------

// backend abstracts a local store and a remote server for the note commands.
type backend interface {
	Put(ctx context.Context, n *note.Note) (*note.Note, error)
	Get(ctx context.Context, id string) (*note.Note, error)
	List(ctx context.Context, f store.Filter) ([]*note.Note, error)
	Search(ctx context.Context, q string, f store.Filter) ([]store.Hit, error)
	Context(ctx context.Context, project string) (string, error)
	Projects(ctx context.Context) (map[string]int, error)
	Close() error
}

type localBackend struct {
	st     *store.Store
	author string
}

func (l localBackend) Put(ctx context.Context, n *note.Note) (*note.Note, error) {
	if n.Author == "" {
		n.Author = l.author
	}
	return l.st.Put(ctx, n)
}
func (l localBackend) Get(ctx context.Context, id string) (*note.Note, error) {
	return l.st.Get(ctx, id, l.author)
}
func (l localBackend) List(ctx context.Context, f store.Filter) ([]*note.Note, error) {
	f.Viewer = l.author
	return l.st.List(ctx, f)
}
func (l localBackend) Search(ctx context.Context, q string, f store.Filter) ([]store.Hit, error) {
	f.Viewer = l.author
	return l.st.Search(ctx, q, f)
}
func (l localBackend) Context(ctx context.Context, project string) (string, error) {
	notes, err := l.st.List(ctx, store.Filter{Project: project, Limit: 500, Viewer: l.author})
	if err != nil {
		return "", err
	}
	return digest.Render(project, notes), nil
}
func (l localBackend) Projects(ctx context.Context) (map[string]int, error) {
	return l.st.Projects(ctx)
}
func (l localBackend) Close() error { return l.st.Close() }

type remoteBackend struct{ *client.Client }

func (r remoteBackend) Projects(ctx context.Context) (map[string]int, error) {
	var out map[string]int
	err := r.Client.Do(ctx, "GET", "/api/v1/projects", nil, &out)
	return out, err
}
func (r remoteBackend) Close() error { return nil }

type common struct {
	db, server, token, author string
}

func (c *common) flags(fs *flag.FlagSet) {
	fs.StringVar(&c.db, "db", env("OPENMIND_DB", defaultDB()), "SQLite database path (local mode)")
	fs.StringVar(&c.server, "server", os.Getenv("OPENMIND_SERVER"), "remote server URL; when set, --db is ignored")
	fs.StringVar(&c.token, "token", os.Getenv("OPENMIND_TOKEN"), "bearer token for --server")
	fs.StringVar(&c.author, "author", env("OPENMIND_AUTHOR", os.Getenv("USER")), "author name for local writes")
}

func (c *common) open() (backend, error) {
	if c.server != "" {
		return remoteBackend{client.New(c.server, c.token)}, nil
	}
	st, err := openStore(c.db)
	if err != nil {
		return nil, err
	}
	return localBackend{st: st, author: c.author}, nil
}

func openStore(path string) (*store.Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return store.Open(path)
}

func defaultDB() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openmind", "openmind.db")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- serve ----------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("OPENMIND_ADDR", ":7777"), "listen address")
	db := fs.String("db", env("OPENMIND_DB", defaultDB()), "SQLite database path")
	tokens := fs.String("tokens", os.Getenv("OPENMIND_TOKENS"), "comma-separated name:secret pairs; empty disables auth")
	fs.Parse(args)

	tk, err := parseTokens(*tokens)
	if err != nil {
		return err
	}
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if len(tk) == 0 {
		log.Warn("auth disabled: no --tokens given; every caller is 'anonymous'. Fine on localhost, not on a network.")
	}
	srv := &http.Server{Addr: *addr, Handler: httpapi.Handler(st, tk, log), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	log.Info("openmind listening", "addr", *addr, "db", *db, "mcp", "http://"+displayAddr(*addr)+"/mcp", "users", len(tk))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func displayAddr(a string) string {
	if strings.HasPrefix(a, ":") {
		return "localhost" + a
	}
	return a
}

func parseTokens(s string) (httpapi.Tokens, error) {
	tk := httpapi.Tokens{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, secret, ok := strings.Cut(pair, ":")
		if !ok || name == "" || len(secret) < 8 {
			return nil, fmt.Errorf("token %q: want name:secret with a secret of at least 8 characters", pair)
		}
		tk[secret] = name
	}
	return tk, nil
}

// ---- mcp (stdio) ----------------------------------------------------------

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	db := fs.String("db", env("OPENMIND_DB", defaultDB()), "SQLite database path")
	author := fs.String("author", env("OPENMIND_AUTHOR", os.Getenv("USER")), "author for notes written in this session")
	fs.Parse(args)
	st, err := openStore(*db)
	if err != nil {
		return err
	}
	defer st.Close()
	return mcpserver.New(st, *author).Run(context.Background(), &mcp.StdioTransport{})
}

// ---- note commands --------------------------------------------------------

func cmdPut(args []string) error {
	var c common
	fs := flag.NewFlagSet("put", flag.ExitOnError)
	c.flags(fs)
	var n note.Note
	var tags string
	fs.StringVar(&n.ID, "id", "", "update this note instead of creating")
	fs.StringVar(&n.Project, "project", "", "project name")
	fs.StringVar(&n.Title, "title", "", "one-line fact")
	fs.StringVar(&n.Body, "body", "", "body (default: read stdin)")
	fs.StringVar(&n.Type, "type", "", "user|feedback|project|reference|decision")
	fs.StringVar(&n.Scope, "scope", "", "personal|project|team")
	fs.StringVar(&tags, "tags", "", "comma-separated tags")
	fs.Parse(args)
	if n.Body == "" {
		b, err := readAll(os.Stdin)
		if err != nil {
			return err
		}
		n.Body = b
	}
	if tags != "" {
		n.Tags = strings.Split(tags, ",")
	}
	n.Source.Tool = "openmind-cli"
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	out, err := be.Put(context.Background(), &n)
	if err != nil {
		return err
	}
	fmt.Printf("saved %s (revision %d)\n", out.ID, out.Revision)
	return nil
}

func cmdSearch(args []string) error {
	var c common
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	c.flags(fs)
	project := fs.String("project", "", "project name")
	limit := fs.Int("limit", 20, "max results")
	fs.Parse(args)
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	hits, err := be.Search(context.Background(), strings.Join(fs.Args(), " "), store.Filter{Project: *project, Limit: *limit})
	if err != nil {
		return err
	}
	for _, h := range hits {
		fmt.Printf("%s  %-50s  %s\n    %s\n", h.Note.ID, trunc(h.Note.Title, 50), h.Note.Updated.Format("2006-01-02"), h.Snippet)
	}
	return nil
}

func cmdList(args []string) error {
	var c common
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	c.flags(fs)
	project := fs.String("project", "", "project name")
	status := fs.String("status", "", "active|draft|deprecated|any")
	limit := fs.Int("limit", 50, "max results")
	fs.Parse(args)
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	notes, err := be.List(context.Background(), store.Filter{Project: *project, Status: *status, Limit: *limit})
	if err != nil {
		return err
	}
	for _, n := range notes {
		fmt.Printf("%s  %-9s %-10s %-50s %s\n", n.ID, n.Type, n.Status, trunc(n.Title, 50), n.Updated.Format("2006-01-02"))
	}
	return nil
}

func cmdGet(args []string) error {
	var c common
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	c.flags(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: openmind get ID")
	}
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	n, err := be.Get(context.Background(), fs.Arg(0))
	if err != nil {
		return err
	}
	os.Stdout.Write(n.Markdown())
	return nil
}

func cmdContext(args []string) error {
	var c common
	fs := flag.NewFlagSet("context", flag.ExitOnError)
	c.flags(fs)
	project := fs.String("project", "", "project name")
	fs.Parse(args)
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	s, err := be.Context(context.Background(), *project)
	if err != nil {
		return err
	}
	fmt.Print(s)
	return nil
}

func cmdProjects(args []string) error {
	var c common
	fs := flag.NewFlagSet("projects", flag.ExitOnError)
	c.flags(fs)
	fs.Parse(args)
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	p, err := be.Projects(context.Background())
	if err != nil {
		return err
	}
	for name, n := range p {
		fmt.Printf("%-30s %d notes\n", name, n)
	}
	return nil
}

// ---- import / export ------------------------------------------------------

func cmdImport(args []string) error {
	if len(args) < 1 || args[0] != "claude-code" {
		return errors.New("usage: openmind import claude-code --project P [--dir DIR]")
	}
	var c common
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	c.flags(fs)
	project := fs.String("project", "", "project name to file notes under")
	dir := fs.String("dir", "", "Claude Code memory directory (default: derived from the current git repo or cwd)")
	scope := fs.String("scope", note.ScopeProject, "scope for imported notes")
	fs.Parse(args[1:])
	if *project == "" {
		return errors.New("--project is required")
	}
	if *dir == "" {
		d, err := claudeMemoryDir()
		if err != nil {
			return err
		}
		*dir = d
	}
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()

	entries, err := os.ReadDir(*dir)
	if err != nil {
		return err
	}
	ctx := context.Background()
	imported := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "MEMORY.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(*dir, name))
		if err != nil {
			return err
		}
		n, err := note.Parse(data)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", name, err)
			continue
		}
		n.ID = ""
		n.Project = *project
		n.Scope = *scope
		if n.Type == "" {
			n.Type = typeFromFilename(name)
		}
		if n.Source.Tool == "" {
			n.Source.Tool = "claude-code"
		}
		if h := n.SummaryHint(); h != "" && !strings.Contains(n.Body, h) {
			n.Body = h + "\n\n" + n.Body
		}
		n.Tags = append(n.Tags, "imported")
		out, err := be.Put(ctx, n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", name, err)
			continue
		}
		fmt.Printf("imported %s -> %s  %s\n", name, out.ID, out.Title)
		imported++
	}
	fmt.Printf("%d notes imported into project %q\n", imported, *project)
	return nil
}

func typeFromFilename(name string) string {
	for _, t := range note.Types {
		if strings.HasPrefix(name, t+"_") || strings.HasPrefix(name, t+"-") {
			return t
		}
	}
	return "project"
}

// claudeMemoryDir mirrors Claude Code's project-dir derivation: the absolute
// path with every non-alphanumeric character replaced by '-'.
func claudeMemoryDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	d := filepath.Join(home, ".claude", "projects", b.String(), "memory")
	if _, err := os.Stat(d); err != nil {
		return "", fmt.Errorf("no Claude Code memory directory at %s; pass --dir", d)
	}
	return d, nil
}

const (
	beginMarker = "<!-- openmind:begin -->"
	endMarker   = "<!-- openmind:end -->"
)

func cmdExport(args []string) error {
	if len(args) < 1 || args[0] != "claude-md" {
		return errors.New("usage: openmind export claude-md --project P [--out CLAUDE.md]")
	}
	var c common
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	c.flags(fs)
	project := fs.String("project", "", "project name")
	out := fs.String("out", "", "file to update in place between openmind markers (default: stdout)")
	fs.Parse(args[1:])
	if *project == "" {
		return errors.New("--project is required")
	}
	be, err := c.open()
	if err != nil {
		return err
	}
	defer be.Close()
	notes, err := be.List(context.Background(), store.Filter{Project: *project, Limit: 500})
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(beginMarker + "\n")
	fmt.Fprintf(&b, "## Team knowledge (OpenMind, project %s, %s)\n\n", *project, time.Now().UTC().Format("2006-01-02"))
	b.WriteString("Generated by `openmind export claude-md`; edit notes in OpenMind, not here.\n\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", n.Title, n.Body)
	}
	b.WriteString(endMarker + "\n")
	section := b.String()
	if *out == "" {
		fmt.Print(section)
		return nil
	}
	existing, err := os.ReadFile(*out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	text := string(existing)
	if i := strings.Index(text, beginMarker); i >= 0 {
		if j := strings.Index(text, endMarker); j > i {
			text = text[:i] + section + text[j+len(endMarker)+1:]
		} else {
			return fmt.Errorf("%s: begin marker without end marker", *out)
		}
	} else {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n" + section
	}
	if err := os.WriteFile(*out, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d notes into %s\n", len(notes), *out)
	return nil
}

// ---- setup ----------------------------------------------------------------

func cmdSetup(args []string) error {
	if len(args) < 1 || args[0] != "claude-code" {
		return errors.New("usage: openmind setup claude-code [--server URL --token T | --db PATH]")
	}
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	server := fs.String("server", os.Getenv("OPENMIND_SERVER"), "remote server URL")
	token := fs.String("token", os.Getenv("OPENMIND_TOKEN"), "bearer token")
	db := fs.String("db", env("OPENMIND_DB", defaultDB()), "local database (stdio mode)")
	scope := fs.String("scope", "user", "claude mcp add scope: user|project|local")
	fs.Parse(args[1:])
	if *server != "" {
		fmt.Printf("claude mcp add --transport http --scope %s openmind %s/mcp", *scope, strings.TrimRight(*server, "/"))
		if *token != "" {
			fmt.Printf(" --header \"Authorization: Bearer %s\"", *token)
		}
		fmt.Println()
		return nil
	}
	exe, _ := os.Executable()
	fmt.Printf("claude mcp add --scope %s openmind -- %s mcp --db %s\n", *scope, exe, *db)
	return nil
}

// ---- helpers --------------------------------------------------------------

func readAll(f *os.File) (string, error) {
	var b strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			if errors.Is(err, os.ErrClosed) || err.Error() == "EOF" {
				break
			}
			return "", err
		}
	}
	return b.String(), nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
