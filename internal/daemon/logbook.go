package daemon

import (
	"log/slog"
	"sync"
	"time"
)

const (
	maxLogLines   = 500
	followBacklog = 64
)

// logbook keeps the most recent lines of one tunnel's log and fans new lines out to followers.
type logbook struct {
	mu        sync.Mutex
	lines     []LogLine
	followers map[chan LogLine]struct{}
}

func newLogbook() *logbook {
	return &logbook{followers: make(map[chan LogLine]struct{})}
}

func (b *logbook) add(level slog.Level, msg string) {
	line := LogLine{Time: time.Now(), Level: level, Message: msg}

	b.mu.Lock()
	defer b.mu.Unlock()

	b.lines = append(b.lines, line)
	if len(b.lines) > maxLogLines {
		b.lines = b.lines[len(b.lines)-maxLogLines:]
	}

	for ch := range b.followers {
		select {
		case ch <- line:
		default:
			// A follower that stopped reading loses lines rather than blocking the tunnel.
		}
	}
}

func (b *logbook) snapshot() []LogLine {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]LogLine(nil), b.lines...)
}

// follow returns the current lines and a channel of new ones; call stop when done.
func (b *logbook) follow() ([]LogLine, <-chan LogLine, func()) {
	ch := make(chan LogLine, followBacklog)

	b.mu.Lock()
	defer b.mu.Unlock()

	b.followers[ch] = struct{}{}
	backlog := append([]LogLine(nil), b.lines...)

	stop := func() {
		b.mu.Lock()
		defer b.mu.Unlock()

		delete(b.followers, ch)
	}

	return backlog, ch, stop
}
