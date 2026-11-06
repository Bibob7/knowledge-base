// Package secrets verhindert, dass offensichtliche Zugangsdaten in der
// Knowledge Base landen.
package secrets

import (
	"fmt"
	"regexp"
)

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"privater Schluessel", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{"AWS Access Key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{"API-Key (sk-...)", regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{"GitHub-Token", regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`)},
	{"GitLab-Token", regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`)},
	{"Slack-Token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`)},
	{"Bearer-Token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{30,}`)},
	{"Passwort-Zuweisung", regexp.MustCompile(`(?i)\b(password|passwd|passwort|secret|api[_-]?key|token)\s*[:=]\s*["']?[^\s"']{8,}`)},
}

// Check liefert einen Fehler, wenn text wie ein Secret aussieht.
func Check(text string) error {
	for _, p := range patterns {
		if p.re.MatchString(text) {
			return fmt.Errorf("enthaelt vermutlich ein Secret (%s) und wird nicht gespeichert", p.name)
		}
	}
	return nil
}
