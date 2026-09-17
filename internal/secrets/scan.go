// Package secrets rejects notes that look like they contain credentials.
package secrets

import (
	"fmt"
	"regexp"
)

type rule struct {
	name string
	re   *regexp.Regexp
}

var rules = []rule{
	{"aws-access-key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"github-token", regexp.MustCompile(`\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b`)},
	{"anthropic-key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{20,}\b`)},
	{"openai-key", regexp.MustCompile(`\bsk-(proj-)?[A-Za-z0-9_\-]{20,}\b`)},
	{"google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`)},
	{"slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z\-]{10,}\b`)},
	{"private-key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`)},
	{"connection-string", regexp.MustCompile(`\b(postgres|postgresql|mysql|mongodb(\+srv)?|redis|amqp|mssql)://[^:\s/]+:[^@\s]+@`)},
	{"generic-secret-assignment", regexp.MustCompile(`(?i)\b(api[_\-]?key|secret[_\-]?key|access[_\-]?token|auth[_\-]?token|password|passwd)\b\s*[:=]\s*["']?[A-Za-z0-9_\-/+=]{20,}`)},
}

// Finding names the rule that matched.
type Finding struct {
	Rule string
}

func (f Finding) Error() string {
	return fmt.Sprintf("secret detected (%s); notes must not contain credentials", f.Rule)
}

// Scan returns the first matching rule as an error, or nil.
func Scan(text string) error {
	for _, r := range rules {
		if r.re.MatchString(text) {
			return Finding{Rule: r.name}
		}
	}
	return nil
}

// Redact replaces every match with a placeholder naming the rule.
func Redact(text string) string {
	for _, r := range rules {
		text = r.re.ReplaceAllString(text, "[REDACTED:"+r.name+"]")
	}
	return text
}
