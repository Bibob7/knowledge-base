// Package chunk splits text into overlapping pieces along paragraph boundaries.
package chunk

import "strings"

// Split splits text into chunks of about size characters (runes, plus overlap). It
// splits at paragraph boundaries; paragraphs that are too long are split hard.
// Consecutive chunks overlap by about overlap characters.
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
	hasNew := false // cur contains more than just overlap
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
