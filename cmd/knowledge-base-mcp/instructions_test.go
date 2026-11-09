package main

import (
	"strings"
	"testing"
)

// Claude Code cuts server instructions after 4096 characters; keep headroom.
func TestInstructionsFitClaudeCodeLimit(t *testing.T) {
	if n := len(instructions); n == 0 || n > 3800 {
		t.Fatalf("instructions are %d bytes, want 1..3800", n)
	}
	long := sessionInstructions(strings.Repeat("p", 100)) // the longest project name
	if n := len(long); n > 4000 {
		t.Fatalf("instructions with the session project are %d bytes, want at most 4000", n)
	}
	if !strings.Contains(long, `"`+strings.Repeat("p", 100)+`"`) || sessionInstructions("") != instructions {
		t.Error("the session project must be named, and only if known")
	}
}
