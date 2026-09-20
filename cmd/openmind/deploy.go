package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ideanix/openmind/internal/auth"
	"github.com/ideanix/openmind/internal/httpapi"
)

const serviceLabel = "org.ideanix.openmind"

func homeDir() string {
	if d := os.Getenv("OPENMIND_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openmind")
}

func defaultTokenFile() string { return filepath.Join(homeDir(), "tokens.json") }

// isLoopback reports whether a listen address is reachable only from this
// machine.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---- token ----------------------------------------------------------------

func cmdToken(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: openmind token add NAME --projects a,b | list | revoke NAME")
	}
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	file := fs.String("token-file", env("OPENMIND_TOKEN_FILE", defaultTokenFile()), "token store")
	projects := fs.String("projects", "", "comma-separated grants: project names, team/* namespaces, or *")
	sub := args[0]
	var name string
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	fs.Parse(rest)
	store := auth.NewFile(*file, nil)
	switch sub {
	case "add":
		secret, err := store.Add(name, auth.SplitGrants(*projects))
		if err != nil {
			return err
		}
		fmt.Printf("token for %s (shown once): %s\ngrants: %s\n", name, secret, *projects)
	case "list":
		for _, e := range store.List() {
			fmt.Printf("%-20s %-40s %s\n", e.Name, strings.Join(e.Projects, ","), e.Created.Format("2006-01-02"))
		}
	case "revoke":
		n, err := store.Revoke(name)
		if err != nil {
			return err
		}
		fmt.Printf("revoked %d token(s) of %s\n", n, name)
	default:
		return fmt.Errorf("unknown token command %q", sub)
	}
	return nil
}

// ---- invite ---------------------------------------------------------------

// lanHost prefers the Bonjour name, which survives DHCP changes.
func lanHost() (name, ip string) {
	if out, err := exec.Command("scutil", "--get", "LocalHostName").Output(); err == nil {
		if h := strings.TrimSpace(string(out)); h != "" {
			name = h + ".local"
		}
	}
	if name == "" {
		if h, err := os.Hostname(); err == nil {
			name = h
		}
	}
	if conn, err := net.Dial("udp", "192.0.2.1:9"); err == nil {
		ip = conn.LocalAddr().(*net.UDPAddr).IP.String()
		conn.Close()
	}
	return name, ip
}

// resolves reports whether a host name resolves within two seconds.
func resolves(host string) bool {
	if host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	return err == nil && len(addrs) > 0
}

func cmdInvite(args []string) error {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	file := fs.String("token-file", env("OPENMIND_TOKEN_FILE", defaultTokenFile()), "token store")
	projects := fs.String("projects", "", "comma-separated grants for the invited device")
	host := fs.String("host", "", "address the other device will use (default: this machine's .local name)")
	port := fs.String("port", "7777", "server port")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs.Parse(args)
	if name == "" {
		return errors.New("usage: openmind invite NAME --projects a,b [--host H]")
	}
	grants := auth.SplitGrants(*projects)
	secret, err := auth.NewFile(*file, nil).Add(name, grants)
	if err != nil {
		return err
	}
	bonjour, ip := lanHost()
	note := ""
	if *host == "" {
		if resolves(bonjour) {
			*host = bonjour
			note = fmt.Sprintf("   (fallback by IP: http://%s:%s)", ip, *port)
		} else {
			*host = ip
			note = "   (the .local name does not resolve here, so the IP is used; reserve it in the router's DHCP or pass --host)"
		}
	}
	base := fmt.Sprintf("http://%s:%s", *host, *port)

	fmt.Printf("Invitation for %q. The token is shown once; send this text over a private channel.\n\n", name)
	fmt.Printf("Server:  %s%s\n", base, note)
	fmt.Printf("Token:   %s\nGrants:  %s\n\n", secret, strings.Join(grants, ", "))
	fmt.Println("1. Check the connection from the other device:")
	fmt.Printf("     curl -H 'Authorization: Bearer %s' %s/api/v1/whoami\n\n", secret, base)
	fmt.Println("2. Inside each repository to share, register the MCP server. Scope \"local\" keeps")
	fmt.Println("   the token in ~/.claude.json, outside the repository, so it cannot be committed:")
	for _, g := range grants {
		p := g
		if g == "*" || strings.HasSuffix(g, "/*") {
			p = strings.TrimSuffix(g, "*") + "<project>"
		}
		fmt.Printf("     claude mcp add --transport http --scope local openmind %s/mcp \\\n       --header \"Authorization: Bearer %s\" --header \"%s: %s\"\n", base, secret, httpapi.ProjectHeader, p)
	}
	fmt.Println("\n3. Optional: load the team context automatically at session start. Put this in")
	hookProject := "<project>"
	if len(grants) == 1 && !strings.Contains(grants[0], "*") {
		hookProject = grants[0]
	}
	fmt.Println("   .claude/settings.local.json of the repository:")
	fmt.Printf(`     {"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"curl -fsS -m 5 -H 'Authorization: Bearer %s' '%s/api/v1/context?project=%s'"}]}]}}`+"\n", secret, base, hookProject)
	return nil
}

// ---- service --------------------------------------------------------------

func cmdService(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: openmind service install [--addr :7777] | uninstall | status")
	}
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	addr := fs.String("addr", ":7777", "listen address of the service (\":7777\" = every interface, IPv4 and IPv6)")
	fs.Parse(args[1:])
	switch args[0] {
	case "install":
		return serviceInstall(*addr)
	case "uninstall":
		return serviceUninstall()
	case "status":
		return serviceStatus(*addr)
	}
	return fmt.Errorf("unknown service command %q", args[0])
}

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
}

func serviceInstall(addr string) error {
	if !isLoopback(addr) && !auth.NewFile(defaultTokenFile(), nil).Enabled() {
		return errors.New("refusing to expose a server without tokens; run `openmind token add <you> --projects '*'` first")
	}
	bin := filepath.Join(homeDir(), "bin", "openmind")
	if err := copySelf(bin); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		fmt.Printf("Installed %s. On Linux, create a systemd user unit:\n\n[Unit]\nDescription=OpenMind\n[Service]\nExecStart=%s serve --addr %s\nRestart=always\n[Install]\nWantedBy=default.target\n", bin, bin, addr)
		return nil
	}
	logPath := filepath.Join(homeDir(), "openmind.log")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key><array>
		<string>%s</string><string>serve</string><string>--addr</string><string>%s</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, serviceLabel, bin, addr, logPath, logPath)
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	target := domain + "/" + serviceLabel
	_ = exec.Command("launchctl", "bootout", target).Run()
	// bootout is asynchronous: wait until the old instance is gone, then
	// retry bootstrap, which fails with EIO while launchd is still tearing down.
	for i := 0; i < 25 && exec.Command("launchctl", "print", target).Run() == nil; i++ {
		time.Sleep(200 * time.Millisecond)
	}
	var out []byte
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if out, err = exec.Command("launchctl", "bootstrap", domain, plistPath()).CombinedOutput(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Printf("service %s installed and started\n  binary: %s\n  log:    %s\n", serviceLabel, bin, logPath)
	// A freshly copied binary can take several seconds to start on macOS.
	var statusErr error
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		if statusErr = serviceStatus(addr); statusErr == nil {
			return nil
		}
	}
	return statusErr
}

func serviceUninstall() error {
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+serviceLabel).Run()
	if err := os.Remove(plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("service removed; database and tokens in", homeDir(), "are kept")
	return nil
}

func serviceStatus(addr string) error {
	_, port, _ := net.SplitHostPort(addr)
	res, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return fmt.Errorf("server is not answering on port %s: %w (see %s)", port, err, filepath.Join(homeDir(), "openmind.log"))
	}
	res.Body.Close()
	name, ip := lanHost()
	fmt.Printf("server is up\n  local:  http://localhost:%s/mcp\n  LAN:    http://%s:%s/mcp\n", port, ip, port)
	if resolves(name) {
		fmt.Printf("          http://%s:%s/mcp\n", name, port)
	}
	return nil
}

func copySelf(dst string) error {
	src, err := os.Executable()
	if err != nil {
		return err
	}
	if src == dst {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
