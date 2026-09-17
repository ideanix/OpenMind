package secrets

import "testing"

func TestScan(t *testing.T) {
	bad := []string{
		"key AKIAIOSFODNN7EXAMPLE here",
		"token ghp_" + repeat("a", 36),
		"-----BEGIN RSA PRIVATE KEY-----",
		"postgres://user:pass@db:5432/x",
		"api_key = abcdefghijklmnopqrstuvwxyz12",
	}
	for _, s := range bad {
		if Scan(s) == nil {
			t.Errorf("expected finding for %q", s)
		}
	}
	good := []string{
		"run make redis before tests",
		"token is read from OPENMIND_TOKEN env",
		"password: ask ops for it",
	}
	for _, s := range good {
		if err := Scan(s); err != nil {
			t.Errorf("false positive for %q: %v", s, err)
		}
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}
