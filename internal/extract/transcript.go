// Package extract wertet Claude-Code-Transkripte am Sitzungsende aus und speichert
// dauerhaft nuetzliches Wissen in der knowledge-base (Sicherheitsnetz zum
// sofortigen remember waehrend der Sitzung).
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

// Conversation ist der fuer die Extraktion relevante Teil eines Transkripts.
type Conversation struct {
	Text     string // "### USER" / "### CLAUDE"-Abschnitte
	UserMsgs int    // Anzahl echter Nutzer-Nachrichten
	LastLine int    // Anzahl gelesener Zeilen (fuer inkrementelles Auswerten)
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

// ParseTranscript liest eine JSONL-Transkriptdatei ab Zeile fromLine (0-basiert).
// Beruecksichtigt werden nur Text-Nachrichten von Nutzer und Claude sowie
// remember-Aufrufe; Tool-Ergebnisse, Denkbloecke und Subagenten bleiben aussen vor.
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
	// Schneller Vorfilter: die meisten Zeilen sind weder user noch assistant.
	// (unabhaengig von der Leerzeichen-Formatierung des JSON)
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
			case bl.Type == "tool_use" && bl.Name == "mcp__knowledge-base__remember":
				var in struct {
					Content string `json:"content"`
				}
				if json.Unmarshal(bl.Input, &in) == nil && in.Content != "" {
					add("CLAUDE", "[bereits in der knowledge-base gespeichert] "+truncateRunes(in.Content, 600))
				}
			}
		}
	}
}

// userText liefert den Text einer echten Nutzer-Eingabe (keine Tool-Ergebnisse,
// keine System-/Command-Meldungen).
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

// Condense kuerzt sehr lange Gespraeche auf maxRunes: Anfang (Ziel der Sitzung)
// und Ende (Ergebnis) bleiben erhalten, die Mitte wird gekuerzt.
func Condense(text string, maxRunes int) string {
	if utf8.RuneCountInString(text) <= maxRunes {
		return text
	}
	r := []rune(text)
	head := maxRunes / 5
	tail := maxRunes - head
	return string(r[:head]) + "\n\n[... mittlerer Teil des Gespraechs gekuerzt ...]\n\n" + string(r[len(r)-tail:])
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + " [...]"
}
