package client_test

import (
	"errors"
	"log/slog"
	"net"
	"testing"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/protocol"
)

func TestSpeedTest(t *testing.T) {
	t.Parallel()

	h := startServer(t)

	st, err := client.DialSpeedTest(t.Context(), &client.Options{Server: h.controlAddr})
	if err != nil {
		t.Fatalf("DialSpeedTest() error: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	latency, err := st.Latency()
	if err != nil || latency <= 0 {
		t.Fatalf("Latency() = %v, %v; want a positive round trip", latency, err)
	}

	down, err := st.Download()
	if err != nil || down <= 0 {
		t.Fatalf("Download() = %v, %v; want a positive rate", down, err)
	}

	up, err := st.Upload()
	if err != nil || up <= 0 {
		t.Fatalf("Upload() = %v, %v; want a positive rate", up, err)
	}
}

func TestSpeedTestUnsupported(t *testing.T) {
	t.Parallel()

	ln := listen(t)

	go serveOldServer(ln)

	st, err := client.DialSpeedTest(t.Context(), &client.Options{Server: ln.Addr().String()})
	if err != nil {
		t.Fatalf("DialSpeedTest() error: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	if _, err := st.Download(); !errors.Is(err, client.ErrSpeedTestUnsupported) {
		t.Errorf("Download() error = %v, want ErrSpeedTestUnsupported", err)
	}
}

// serveOldServer answers one client like a server from before speed tests: it completes the
// handshake, then rejects every message kind it doesn't know.
func serveOldServer(ln net.Listener) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}

	mux, err := yamux.Server(conn, protocol.MuxConfig(slog.New(slog.DiscardHandler)))
	if err != nil {
		return
	}
	defer func() { _ = mux.Close() }()

	stream, err := mux.AcceptStream()
	if err != nil {
		return
	}

	ctrl := protocol.NewControl(stream)

	if _, err := ctrl.Recv(); err != nil {
		return
	}

	if err := ctrl.Send(&protocol.Message{Kind: protocol.KindWelcome}); err != nil {
		return
	}

	for {
		if _, err := ctrl.Recv(); err != nil {
			return
		}

		if err := ctrl.Send(&protocol.Message{Kind: protocol.KindError, Error: "unexpected message"}); err != nil {
			return
		}
	}
}
