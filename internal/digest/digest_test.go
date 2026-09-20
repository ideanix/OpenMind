package digest

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ideanix/openmind/internal/note"
)

func TestSummaryKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("я", 300)
	out := Render("p", []*note.Note{{ID: "1", Title: "t", Type: "project", Body: body}})
	if !utf8.ValidString(out) {
		t.Fatal("digest cut a multi-byte character")
	}
}
