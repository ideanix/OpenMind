// Package auth defines who is calling and which projects they may touch.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Identity is a resolved caller.
type Identity struct {
	User string
	// Projects the identity may read and write: an exact name, "*" for
	// everything, or a namespace glob such as "team/*".
	Projects []string
}

// Anonymous is used when the server runs without tokens (local mode).
var Anonymous = Identity{User: "anonymous", Projects: []string{"*"}}

// Allows reports whether the identity may access project.
func (id Identity) Allows(project string) bool {
	if project == "" {
		return false
	}
	for _, p := range id.Projects {
		switch {
		case p == "*" || p == project:
			return true
		case strings.HasSuffix(p, "/*") && strings.HasPrefix(project, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}

// Admin reports whether the identity holds the "*" grant. Admins may open
// the control desk and see every client's activity.
func (id Identity) Admin() bool {
	for _, p := range id.Projects {
		if p == "*" {
			return true
		}
	}
	return false
}

// Lister is implemented by resolvers that can enumerate their tokens.
type Lister interface {
	List() []Entry
}

// Resolver turns a presented bearer secret into an identity.
type Resolver interface {
	// Enabled reports whether any token exists. When false the server is in
	// local mode and every caller is Anonymous.
	Enabled() bool
	Resolve(secret string) (Identity, bool)
}

// Hash is how secrets are stored and compared; the clear secret is shown
// once at creation and never written to disk.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// NewSecret returns a random 32-hex-character bearer secret.
func NewSecret() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Tokens is a static resolver keyed by secret hash.
type Tokens map[string]Identity

func (t Tokens) Enabled() bool { return len(t) > 0 }

func (t Tokens) Resolve(secret string) (Identity, bool) {
	id, ok := t[Hash(secret)]
	return id, ok
}

// ParseTokens reads "name:secret[:proj1|proj2|*],...". A token without a
// project list gets access to nothing, so sharing needs an explicit grant.
func ParseTokens(spec string) (Tokens, error) {
	tk := Tokens{}
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, ":", 3)
		if len(parts) < 2 || parts[0] == "" || len(parts[1]) < 8 {
			return nil, fmt.Errorf("token %q: want name:secret[:projects] with a secret of at least 8 characters", entry)
		}
		id := Identity{User: parts[0]}
		if len(parts) == 3 {
			id.Projects = splitProjects(parts[2], "|")
		}
		h := Hash(parts[1])
		if _, dup := tk[h]; dup {
			return nil, fmt.Errorf("token for %q: secret reused", parts[0])
		}
		tk[h] = id
	}
	return tk, nil
}

func splitProjects(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Entry is one row of the token file.
type Entry struct {
	Name     string    `json:"name"`
	Hash     string    `json:"hash"`
	Projects []string  `json:"projects"`
	Created  time.Time `json:"created"`
}

// File is a token store on disk. The server re-reads it when it changes, so
// `openmind token add` takes effect without a restart.
type File struct {
	Path string

	mu      sync.Mutex
	mtime   time.Time
	entries []Entry
	extra   Tokens // static tokens from flags/env, merged in
}

// NewFile returns a resolver backed by path, merged with static tokens.
func NewFile(path string, static Tokens) *File {
	return &File{Path: path, extra: static}
}

func (f *File) load() {
	st, err := os.Stat(f.Path)
	if err != nil {
		f.entries, f.mtime = nil, time.Time{}
		return
	}
	if st.ModTime().Equal(f.mtime) {
		return
	}
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return
	}
	var entries []Entry
	if json.Unmarshal(data, &entries) == nil {
		f.entries, f.mtime = entries, st.ModTime()
	}
}

func (f *File) Enabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	return len(f.entries) > 0 || len(f.extra) > 0
}

func (f *File) Resolve(secret string) (Identity, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	h := Hash(secret)
	for _, e := range f.entries {
		if e.Hash == h {
			return Identity{User: e.Name, Projects: e.Projects}, true
		}
	}
	id, ok := f.extra[h]
	return id, ok
}

// List returns the entries sorted by name.
func (f *File) List() []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.load()
	out := append([]Entry(nil), f.entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Add creates a token for name and returns the clear secret. A name may hold
// several tokens (one per device); projects are required.
func (f *File) Add(name string, projects []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, ":, ") {
		return "", errors.New("token name must be non-empty without ':', ',' or spaces")
	}
	if len(projects) == 0 {
		return "", errors.New("at least one project grant is required (use * for all)")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mtime = time.Time{}
	f.load()
	secret := NewSecret()
	f.entries = append(f.entries, Entry{Name: name, Hash: Hash(secret), Projects: projects, Created: time.Now().UTC()})
	return secret, f.save()
}

// Revoke removes every token of name and returns how many were removed.
func (f *File) Revoke(name string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mtime = time.Time{}
	f.load()
	kept := f.entries[:0]
	removed := 0
	for _, e := range f.entries {
		if e.Name == name {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	f.entries = kept
	if removed == 0 {
		return 0, nil
	}
	return removed, f.save()
}

func (f *File) save() error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f.entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.Path); err != nil {
		return err
	}
	f.mtime = time.Time{}
	return nil
}

// SplitGrants parses a comma-separated grant list from the CLI.
func SplitGrants(s string) []string { return splitProjects(s, ",") }
