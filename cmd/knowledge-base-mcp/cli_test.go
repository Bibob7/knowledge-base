package main

import "testing"

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://kb:secret@localhost:5433/kb?sslmode=disable": "postgres://kb:xxxxx@localhost:5433/kb?sslmode=disable",
		"postgres://kb@localhost/kb":                             "postgres://kb@localhost/kb",
		"sqlite:///home/u/.local/share/knowledge-base/kb.db":     "sqlite:///home/u/.local/share/knowledge-base/kb.db",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
