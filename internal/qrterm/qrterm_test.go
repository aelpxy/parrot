package qrterm_test

import (
	"testing"
	"unicode/utf8"

	"github.com/aelpxy/parrot/internal/qrterm"
)

func TestLines(t *testing.T) {
	t.Parallel()

	lines, err := qrterm.Lines("https://brave-otter-k3j9x2mq.tryparrot.site")
	if err != nil {
		t.Fatalf("Lines() error: %v", err)
	}

	if len(lines) < 10 {
		t.Fatalf("Lines() returned %d lines, want a full code", len(lines))
	}

	width := utf8.RuneCountInString(lines[0])
	for i, l := range lines {
		if n := utf8.RuneCountInString(l); n != width {
			t.Errorf("line %d is %d wide, want %d", i, n, width)
		}
	}

	// Two modules per row: a square code is twice as wide as it is tall, give or take the odd row.
	if height := len(lines); width < 2*height-2 || width > 2*height {
		t.Errorf("code is %dx%d characters, want about twice as wide as tall", width, height)
	}
}
