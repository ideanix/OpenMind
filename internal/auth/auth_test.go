package auth

import (
	"path/filepath"
	"testing"
)

func must(t Tokens, secret string) Identity {
	id, _ := t.Resolve(secret)
	return id
}

func TestFileAddResolveRevoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	f := NewFile(path, nil)
	if f.Enabled() {
		t.Fatal("empty file must not enable auth")
	}
	secret, err := f.Add("laptop2", []string{"logdoc", "home/*"})
	if err != nil {
		t.Fatal(err)
	}
	// A second resolver (the running server) sees the new token from disk.
	srv := NewFile(path, nil)
	id, ok := srv.Resolve(secret)
	if !ok || id.User != "laptop2" || !id.Allows("home/notes") || id.Allows("work") {
		t.Fatalf("resolve: %+v %v", id, ok)
	}
	if _, ok := srv.Resolve("wrong-secret"); ok {
		t.Fatal("wrong secret accepted")
	}
	if _, err := f.Add("x", nil); err == nil {
		t.Fatal("token without grants accepted")
	}
	if n, _ := f.Revoke("laptop2"); n != 1 {
		t.Fatalf("revoke: %d", n)
	}
	if _, ok := NewFile(path, nil).Resolve(secret); ok {
		t.Fatal("revoked token still works")
	}
}

func TestParseAndAllows(t *testing.T) {
	tk, err := ParseTokens("alice:alicesecret1:logdoc|openmind, bob:bobsecret12:logdoc, root:rootsecret1:*, none:nonesecret1")
	if err != nil {
		t.Fatal(err)
	}
	if !must(tk, "alicesecret1").Allows("openmind") || must(tk, "bobsecret12").Allows("openmind") {
		t.Error("project grant wrong")
	}
	if !must(tk, "rootsecret1").Allows("anything") {
		t.Error("wildcard wrong")
	}
	if must(tk, "nonesecret1").Allows("logdoc") {
		t.Error("token without grant must allow nothing")
	}
	ns := Identity{User: "n", Projects: []string{"emcd/*"}}
	if !ns.Allows("emcd/fiat-orc") || ns.Allows("solar/util") || ns.Allows("emcd") || ns.Allows("emcdx/y") {
		t.Error("namespace grant wrong")
	}
	if _, err := ParseTokens("a:short"); err == nil {
		t.Error("short secret accepted")
	}
}
