package main

import "testing"

// Claude Code cuts server instructions after 4096 characters; keep headroom.
func TestInstructionsFitClaudeCodeLimit(t *testing.T) {
	if n := len(instructions); n == 0 || n > 3800 {
		t.Fatalf("instructions are %d bytes, want 1..3800", n)
	}
}
