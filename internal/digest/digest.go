// Package digest renders the compact project context an agent loads at the
// start of a session.
package digest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ideanix/openmind/internal/note"
)

// MaxBytes caps the digest so it fits a client's startup budget.
const MaxBytes = 24 * 1024

// Render groups active notes by type and returns markdown. Notes are listed
// newest first within a type; output stops when MaxBytes is reached.
func Render(project string, notes []*note.Note) string {
	order := []string{"decision", "project", "feedback", "reference", "user"}
	groups := map[string][]*note.Note{}
	for _, n := range notes {
		groups[n.Type] = append(groups[n.Type], n)
	}
	var extra []string
	for t := range groups {
		if !contains(order, t) {
			extra = append(extra, t)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	var b strings.Builder
	fmt.Fprintf(&b, "# OpenMind context: %s\n\n", project)
	fmt.Fprintf(&b, "%d active notes. Use openmind_get(id) for the full text, openmind_search for more.\n", len(notes))
	for _, t := range order {
		ns := groups[t]
		if len(ns) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n", t)
		for _, n := range ns {
			line := fmt.Sprintf("- **%s** (%s): %s\n", n.Title, n.ID, summary(n.Body))
			if b.Len()+len(line) > MaxBytes {
				b.WriteString("\n_(truncated: more notes available via openmind_search)_\n")
				return b.String()
			}
			b.WriteString(line)
		}
	}
	return b.String()
}

func summary(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if len(l) > 200 {
			l = l[:200] + "…"
		}
		return l
	}
	return ""
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
