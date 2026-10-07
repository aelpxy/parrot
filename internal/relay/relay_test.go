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
