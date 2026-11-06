// Package secrets prevents obvious credentials from ending up in the
// knowledge base.
package secrets

import (
	"fmt"
	"regexp"
)

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS Access Key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"API-Key (sk-...)", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{"GitHub-Token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`)},
	{"GitLab-Token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"Slack-Token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"Bearer-Token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{30,}`)},
	{"password assignment", regexp.MustCompile(`(?i)\b(password|passwd|passwort|secret|api[_-]?key|token)\s*[:=]\s*["']?[^\s"']{8,}`)},
}

// Check returns an error if text looks like a secret.
func Check(text string) error {
	for _, p := range patterns {
		if p.re.MatchString(text) {
			return fmt.Errorf("probably contains a secret (%s) and will not be stored", p.name)
		}
	}
	return nil
}
