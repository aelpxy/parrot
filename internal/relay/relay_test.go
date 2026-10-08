package relay_test

import (
	"io"
	"net"
	"testing"

	"github.com/aelpxy/parrot/internal/relay"
)

func TestJoin(t *testing.T) {
	t.Parallel()

	userSide, publicConn := net.Pipe()
	localConn, appSide := net.Pipe()

	done := make(chan error, 1)

	go func() {
		done <- relay.Join(publicConn, localConn)
	}()

	go func() {
		buf := make([]byte, 4)
		if _, err := io.ReadFull(appSide, buf); err != nil {
			return
		}

		_, _ = appSide.Write([]byte("pong"))
		_ = appSide.Close()
	}()

	if _, err := userSide.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := io.ReadAll(userSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(got) != "pong" {
		t.Errorf("got %q, want %q", got, "pong")
	}

	if err := <-done; err != nil {
		t.Errorf("Join() error: %v", err)
	}
}

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	var d net.Dialer

	dialed, err := d.DialContext(t.Context(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	accepted, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = dialed.Close()
		_ = accepted.Close()
	})

	return dialed.(*net.TCPConn), accepted.(*net.TCPConn)
}

func TestJoinHalfClose(t *testing.T) {
	t.Parallel()

	userSide, publicConn := tcpPair(t)
	localConn, appSide := tcpPair(t)

	done := make(chan error, 1)

	go func() {
		done <- relay.Join(publicConn, localConn)
	}()

	// The app replies only after the request ends, like a client using shutdown(SHUT_WR).
	go func() {
		if _, err := io.ReadAll(appSide); err != nil {
			return
		}

		_, _ = appSide.Write([]byte("pong"))
		_ = appSide.Close()
	}()

	if _, err := userSide.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := userSide.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}

	got, err := io.ReadAll(userSide)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(got) != "pong" {
		t.Errorf("got %q, want %q", got, "pong")
	}

	if err := <-done; err != nil {
		t.Errorf("Join() error: %v", err)
	}
}
