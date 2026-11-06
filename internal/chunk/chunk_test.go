package chunk

import (
	"strings"
	"testing"
)

func TestSplitEmpty(t *testing.T) {
	if got := Split("  \n\n ", 100, 10); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func TestSplitParagraphsAndOverlap(t *testing.T) {
	text := strings.Repeat("a", 60) + "\n\n" + strings.Repeat("b", 60) + "\n\n" + strings.Repeat("c", 60)
	got := Split(text, 100, 10)
	if len(got) < 2 {
		t.Fatalf("want >=2 chunks, got %d: %v", len(got), got)
	}
	if !strings.HasSuffix(got[0], "a") || !strings.Contains(got[1], "b") {
		t.Fatalf("unexpected chunks: %v", got)
	}
	// Overlap: the end of chunk 0 shows up at the start of chunk 1.
	if !strings.HasPrefix(got[1], strings.Repeat("a", 10)) {
		t.Fatalf("missing overlap: %q", got[1])
	}
}

func TestSplitLongParagraph(t *testing.T) {
	got := Split(strings.Repeat("x", 500), 100, 20)
	if len(got) < 5 {
		t.Fatalf("want >=5 chunks, got %d", len(got))
	}
	for _, c := range got {
		if len([]rune(c)) > 100+20+2 {
			t.Fatalf("chunk too long: %d", len(c))
		}
	}
}
