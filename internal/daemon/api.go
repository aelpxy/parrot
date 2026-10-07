// Package daemon keeps tunnels running in the background and serves a small JSON API
// over a unix socket that only the current user can reach.
package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/aelpxy/parrot/internal/tunnel"
)

const socketName = "parrot.sock"

// ErrNotRunning is returned by Client when no daemon is listening on the socket.
var ErrNotRunning = errors.New("daemon is not running")

// Status is the connection state of a background tunnel.
type Status int

// Tunnel states.
const (
	StatusInvalid Status = iota
	StatusStarting
	StatusOnline
	StatusReconnecting
)

func (s Status) String() string {
	switch s {
	case StatusStarting:
		return "starting"
	case StatusOnline:
		return "online"
	case StatusReconnecting:
		return "reconnecting"
	case StatusInvalid:
	}

	return "invalid"
}

// MarshalText implements encoding.TextMarshaler.
func (s Status) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Status) UnmarshalText(b []byte) error {
	for _, candidate := range []Status{StatusStarting, StatusOnline, StatusReconnecting} {
		if candidate.String() == string(b) {
			*s = candidate

			return nil
		}
	}

	return fmt.Errorf("unknown status %q", b)
}

// StartRequest asks the daemon to run specs against a server.
type StartRequest struct {
	Server string        `json:"server"`
	Token  string        `json:"token,omitzero"`
	Specs  []tunnel.Spec `json:"specs"`
	TLS    bool          `json:"tls,omitzero"`
}

// TunnelInfo describes a running background tunnel.
type TunnelInfo struct {
	StartedAt time.Time    `json:"started_at"`
	Name      string       `json:"name"`
	URL       string       `json:"url,omitzero"`
	Target    string       `json:"target"`
	LastError string       `json:"last_error,omitzero"`
	Proto     tunnel.Proto `json:"proto"`
	Status    Status       `json:"status"`
}

// LogLine is one entry in a tunnel's log.
type LogLine struct {
	Time    time.Time  `json:"time"`
	Message string     `json:"message"`
	Level   slog.Level `json:"level"`
}

type apiError struct {
	Error string `json:"error"`
}

// SocketPath returns where the daemon listens: under $XDG_RUNTIME_DIR, else the user's state dir.
func SocketPath() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "parrot", socketName), nil
	}

	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "parrot", socketName), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate socket dir: %w", err)
	}

	return filepath.Join(home, ".local", "state", "parrot", socketName), nil
}
