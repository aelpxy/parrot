package server

import (
	"bytes"
	"io"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

const (
	ctrlC        = 0x03
	ctrlD        = 0x04
	crlf         = "\r\n"
	inputBufSize = 64
)

// sshTerminal is the user's view of an SSH session: tunnel URLs and problems, written once the
// client opens a session channel. Lines written before then are held back.
type sshTerminal struct {
	header []string

	mu          sync.Mutex
	ch          ssh.Channel
	pending     []string
	tunnels     int
	footerShown bool
}

// println writes lines to the session, or keeps them until one is open.
func (t *sshTerminal) println(lines ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.ch == nil {
		t.pending = append(t.pending, lines...)

		return
	}

	t.write(lines)
}

// tunnel writes a tunnel's lines, followed by how to stop once the first tunnel is up.
func (t *sshTerminal) tunnel(lines ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.tunnels++

	if t.ch == nil {
		t.pending = append(t.pending, lines...)

		return
	}

	t.write(lines)
	t.footer()
}

// attach starts showing output on ch, beginning with the header and anything held back.
func (t *sshTerminal) attach(ch ssh.Channel) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.ch = ch
	t.write(slices.Concat(t.header, t.pending))
	t.pending = nil
	t.footer()
}

// footer must be called with mu held.
func (t *sshTerminal) footer() {
	if t.tunnels == 0 || t.footerShown {
		return
	}

	t.footerShown = true
	t.write([]string{"", "  press ctrl+c to stop", ""})
}

func (t *sshTerminal) attached() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.ch != nil
}

// write must be called with mu held.
func (t *sshTerminal) write(lines []string) {
	// The session may already be closing; output is best effort.
	_, _ = io.WriteString(t.ch, strings.Join(lines, crlf)+crlf)
}

// watchInput ends the connection when the user presses ctrl+c or ctrl+d in a raw terminal.
func watchInput(ch ssh.Channel, disconnect func()) {
	buf := make([]byte, inputBufSize)

	for {
		n, err := ch.Read(buf)
		if bytes.IndexByte(buf[:n], ctrlC) >= 0 || bytes.IndexByte(buf[:n], ctrlD) >= 0 {
			disconnect()

			return
		}

		if err != nil {
			return
		}
	}
}

// answerSessionRequests accepts what an interactive ssh asks for, so it shows our output.
// A failed reply means the channel is gone, which ends the loop anyway.
func answerSessionRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "pty-req", "shell", "exec", "window-change":
			_ = req.Reply(true, nil)
		default:
			_ = req.Reply(false, nil)
		}
	}
}
