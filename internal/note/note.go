// Package note defines the unit of knowledge stored by OpenMind: one markdown
// document with YAML frontmatter. The format is a superset of Claude Code's
// auto-memory files so those import without conversion.
package note

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Types a note can have. Mirrors Claude Code's memory kinds plus "decision".
var Types = []string{"user", "feedback", "project", "reference", "decision"}

// Scopes control visibility.
const (
	ScopePersonal = "personal"
	ScopeProject  = "project"
	ScopeTeam     = "team"
)

// Statuses.
const (
	StatusDraft      = "draft"
	StatusActive     = "active"
	StatusDeprecated = "deprecated"
)

// Source records where a note came from.
type Source struct {
	Tool    string `yaml:"tool,omitempty" json:"tool,omitempty"`
	Session string `yaml:"session,omitempty" json:"session,omitempty"`
	Model   string `yaml:"model,omitempty" json:"model,omitempty"`
}

// Note is one unit of knowledge.
type Note struct {
	ID       string    `yaml:"id" json:"id"`
	Title    string    `yaml:"title" json:"title"`
	Type     string    `yaml:"type" json:"type"`
	Scope    string    `yaml:"scope" json:"scope"`
	Project  string    `yaml:"project" json:"project"`
	Tags     []string  `yaml:"tags,omitempty" json:"tags,omitempty"`
	Author   string    `yaml:"author,omitempty" json:"author,omitempty"`
	Source   Source    `yaml:"source,omitempty" json:"source"`
	Status   string    `yaml:"status" json:"status"`
	Created  time.Time `yaml:"created" json:"created"`
	Updated  time.Time `yaml:"updated" json:"updated"`
	Revision int       `yaml:"revision,omitempty" json:"revision"`
	Body     string    `yaml:"-" json:"body"`

	summaryHint string
}

// NewID returns a time-sortable random id: 12 hex digits of unix millis plus
// 8 random hex digits.
func NewID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%012x%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// Normalize fills defaults and returns a validation error if the note cannot
// be stored.
func (n *Note) Normalize() error {
	n.Title = strings.TrimSpace(n.Title)
	n.Body = strings.TrimSpace(n.Body)
	n.Project = strings.TrimSpace(n.Project)
	if n.Project == "" {
		return errors.New("note: project is required")
	}
	if n.Title == "" {
		return errors.New("note: title is required")
	}
	if n.Body == "" {
		return errors.New("note: body is required")
	}
	if n.Type == "" {
		n.Type = "project"
	}
	if n.Scope == "" {
		n.Scope = ScopeProject
	}
	switch n.Scope {
	case ScopePersonal, ScopeProject, ScopeTeam:
	default:
		return fmt.Errorf("note: unknown scope %q", n.Scope)
	}
	if n.Status == "" {
		n.Status = StatusActive
	}
	switch n.Status {
	case StatusDraft, StatusActive, StatusDeprecated:
	default:
		return fmt.Errorf("note: unknown status %q", n.Status)
	}
	for i, t := range n.Tags {
		n.Tags[i] = strings.ToLower(strings.TrimSpace(t))
	}
	return nil
}

// Parse reads a markdown document with optional YAML frontmatter.
// Unknown frontmatter keys are ignored; Claude Code's `name`, `description`
// and `metadata.type` are mapped onto Title, Body summary and Type.
func Parse(md []byte) (*Note, error) {
	n := &Note{}
	body := md
	if bytes.HasPrefix(md, []byte("---\n")) || bytes.HasPrefix(md, []byte("---\r\n")) {
		rest := md[3:]
		end := bytes.Index(rest, []byte("\n---"))
		if end < 0 {
			return nil, errors.New("note: unterminated frontmatter")
		}
		fm := rest[:end]
		body = rest[end+4:]
		var raw map[string]any
		if err := yaml.Unmarshal(fm, &raw); err != nil {
			return nil, fmt.Errorf("note: frontmatter: %w", err)
		}
		applyRaw(n, raw)
	}
	n.Body = strings.TrimSpace(string(body))
	if n.Title == "" {
		n.Title = firstHeading(n.Body)
	}
	return n, nil
}

func applyRaw(n *Note, raw map[string]any) {
	str := func(k string) string {
		if v, ok := raw[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	n.ID = str("id")
	n.Title = str("title")
	if n.Title == "" {
		n.Title = str("name")
	}
	n.Type = str("type")
	if md, ok := raw["metadata"].(map[string]any); ok && n.Type == "" {
		if t, ok := md["type"]; ok {
			n.Type = fmt.Sprint(t)
		}
	}
	n.Scope = str("scope")
	n.Project = str("project")
	n.Author = str("author")
	n.Status = str("status")
	if d := str("description"); d != "" {
		n.summaryHint = d
	}
	if v, ok := raw["tags"].([]any); ok {
		for _, t := range v {
			n.Tags = append(n.Tags, fmt.Sprint(t))
		}
	}
	if s, ok := raw["source"].(map[string]any); ok {
		if v, ok := s["tool"]; ok {
			n.Source.Tool = fmt.Sprint(v)
		}
		if v, ok := s["session"]; ok {
			n.Source.Session = fmt.Sprint(v)
		}
		if v, ok := s["model"]; ok {
			n.Source.Model = fmt.Sprint(v)
		}
	}
	if v := str("originSessionId"); v != "" && n.Source.Session == "" {
		n.Source.Session = v
		n.Source.Tool = "claude-code"
	}
	if t, ok := raw["created"].(time.Time); ok {
		n.Created = t
	}
	if t, ok := raw["updated"].(time.Time); ok {
		n.Updated = t
	} else if t, ok := raw["modified"].(time.Time); ok {
		n.Updated = t
	}
	if r, ok := raw["revision"].(int); ok {
		n.Revision = r
	}
}

// SummaryHint is the Claude Code `description` field when a note was parsed
// from an auto-memory file. It is not stored; importers may prepend it.
func (n *Note) SummaryHint() string { return n.summaryHint }

func firstHeading(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	if i := strings.IndexByte(body, '\n'); i > 0 {
		return strings.TrimSpace(body[:i])
	}
	return strings.TrimSpace(body)
}

// Markdown renders the note as frontmatter plus body.
func (n *Note) Markdown() []byte {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	fm, _ := yaml.Marshal(n)
	buf.Write(fm)
	buf.WriteString("---\n")
	buf.WriteString(n.Body)
	buf.WriteString("\n")
	return buf.Bytes()
}
