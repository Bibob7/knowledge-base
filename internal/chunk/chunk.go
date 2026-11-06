// Package chunk teilt Text absatzbasiert in ueberlappende Stuecke.
package chunk

import "strings"

// Split teilt text in Chunks von etwa size Zeichen (Runes, plus Ueberlappung). Es wird an
// Absatzgrenzen getrennt; zu lange Absaetze werden hart geteilt. Zwischen
// aufeinanderfolgenden Chunks liegt eine Ueberlappung von etwa overlap Zeichen.
func Split(text string, size, overlap int) []string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return nil
	}

	var units []string
	for p := range strings.SplitSeq(text, "\n\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		r := []rune(p)
		for len(r) > size {
			units = append(units, string(r[:size]))
			r = r[size-overlap:]
		}
		units = append(units, string(r))
	}

	var chunks []string
	var cur []rune
	hasNew := false // cur enthaelt mehr als nur Ueberlappung
	flush := func() {
		if !hasNew {
			return
		}
		if s := strings.TrimSpace(string(cur)); s != "" {
			chunks = append(chunks, s)
		}
	}
	for _, u := range units {
		ur := []rune(u)
		if len(cur) > 0 && len(cur)+2+len(ur) > size {
			flush()
			if overlap > 0 && len(cur) > overlap {
				cur = append([]rune(nil), cur[len(cur)-overlap:]...)
			} else if overlap == 0 {
				cur = cur[:0]
			}
			hasNew = false
		}
		if len(cur) > 0 {
			cur = append(cur, '\n', '\n')
		}
		cur = append(cur, ur...)
		hasNew = true
	}
	flush()
	return chunks
}
