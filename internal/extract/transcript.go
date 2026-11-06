// Package extract evaluates Claude Code transcripts at the end of a session and
// stores lastingly useful knowledge in the knowledge base (a safety net for the
// immediate remember during the session).
package extract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Conversation is the part of a transcript that is relevant for extraction.
type Conversation struct {
	Text     string // "### USER" / "### CLAUDE" sections
	UserMsgs int    // number of real user messages
	LastLine int    // number of lines read (for incremental evaluation)
}

const (
	maxUserRunes      = 6000
	maxAssistantRunes = 2500
)

var (
	reminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	skipPrefix = []string{
		"<local-command", "<command-name>", "<command-message>", "Caveat:",
		"<system-reminder>", "[Request interrupted", "<task-notification",
	}
)

type entry struct {
	Type        string `json:"type"`
	IsMeta      bool   `json:"isMeta"`
	IsSidechain bool   `json:"isSidechain"`
	Message     struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ParseTranscript reads a JSONL transcript file starting after line fromLine
// (0-based). Only text messages from the user and Claude as well as remember
// calls are considered; tool results, thinking blocks and subagents are left out.
func ParseTranscript(r io.Reader, fromLine int) (Conversation, error) {
	var conv Conversation
	var b strings.Builder
	br := bufio.NewReaderSize(r, 1<<20)
	line := 0
	lastRole := ""
	add := func(role, text string) {
		if role != lastRole {
			b.WriteString("\n### " + role + "\n")
			lastRole = role
		}
		b.WriteString(text + "\n")
	}
	for {
		raw, err := br.ReadBytes('\n')
		if len(raw) > 0 {
			line++
			if line > fromLine {
				handleLine(raw, &conv, add)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return conv, err
		}
	}
	conv.LastLine = line
	conv.Text = strings.TrimSpace(b.String())
	return conv, nil
}

func handleLine(raw []byte, conv *Conversation, add func(role, text string)) {
	// Quick pre-filter: most lines are neither user nor assistant.
	// (independent of the whitespace formatting of the JSON)
	if !bytes.Contains(raw, []byte(`"user"`)) && !bytes.Contains(raw, []byte(`"assistant"`)) {
		return
	}
	var e entry
	if json.Unmarshal(raw, &e) != nil || e.IsSidechain {
		return
	}
	switch e.Type {
	case "user":
		if e.IsMeta {
			return
		}
		if text, ok := userText(e.Message.Content); ok {
			conv.UserMsgs++
			add("USER", truncateRunes(text, maxUserRunes))
		}
	case "assistant":
		var blocks []block
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return
		}
		for _, bl := range blocks {
			switch {
			case bl.Type == "text" && strings.TrimSpace(bl.Text) != "":
				add("CLAUDE", truncateRunes(strings.TrimSpace(bl.Text), maxAssistantRunes))
			case bl.Type == "tool_use" && isRememberTool(bl.Name):
				var in struct {
					Content string `json:"content"`
				}
				if json.Unmarshal(bl.Input, &in) == nil && in.Content != "" {
					add("CLAUDE", "[already saved in the knowledge base] "+truncateRunes(in.Content, 600))
				}
			}
		}
	}
}

// userText returns the text of a real user input (no tool results,
// no system/command messages).
func userText(content json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(content, &s) != nil {
		var blocks []block
		if json.Unmarshal(content, &blocks) != nil {
			return "", false
		}
		var parts []string
		for _, bl := range blocks {
			if bl.Type == "tool_result" {
				return "", false
			}
			if bl.Type == "text" {
				parts = append(parts, bl.Text)
			}
		}
		s = strings.Join(parts, "\n")
	}
	s = strings.TrimSpace(reminderRe.ReplaceAllString(s, ""))
	if s == "" {
		return "", false
	}
	for _, p := range skipPrefix {
		if strings.HasPrefix(s, p) {
			return "", false
		}
	}
	return s, true
}

// Condense shortens very long conversations to maxRunes: the beginning (goal of
// the session) and the end (result) are kept, the middle is cut.
func Condense(text string, maxRunes int) string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	r := []rune(text)
	head := maxRunes / 5
	tail := maxRunes - head
	return string(r[:head]) + "\n\n[... middle part of the conversation omitted ...]\n\n" + string(r[len(r)-tail:])
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + " [...]"
}

// isRememberTool recognizes our remember tool, both as a plugin server
// (mcp__plugin_knowledge-base_<server>__remember) and as a server registered by hand
// (mcp__knowledge-base__remember).
func isRememberTool(name string) bool {
	return name == "mcp__knowledge-base__remember" ||
		strings.HasPrefix(name, "mcp__plugin_knowledge-base_") && strings.HasSuffix(name, "__remember")
}
