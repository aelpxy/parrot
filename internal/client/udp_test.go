package client_test

import (
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/tunnel"
)

// udpEcho answers every packet with "echo:" and the packet, so each sender can check its own replies.
func udpEcho(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig

	pc, err := lc.ListenPacket(t.Context(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = pc.Close() })

	go func() {
		buf := make([]byte, 2048)

		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}

			_, _ = pc.WriteTo(append([]byte("echo:"), buf[:n]...), addr)
		}
	}()

	return pc.LocalAddr().String()
}

// sendUDP sends msg from a fresh socket to addr and returns the reply.
func sendUDP(t *testing.T, addr, msg string) string {
	t.Helper()

	d := net.Dialer{Timeout: 5 * time.Second}

	conn, err := d.DialContext(t.Context(), "udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 2048)

	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("no reply to %q: %v", msg, err)
	}

	return string(buf[:n])
}

func TestUDPTunnel(t *testing.T) {
	t.Parallel()

	h := startServer(t)

	_, port, err := net.SplitHostPort(udpEcho(t))
	if err != nil {
		t.Fatal(err)
	}

	// The echo listens on IPv4 only, like most UDP apps, while localhost often resolves to ::1 first.
	tunnels := h.startClient(t, tunnel.Spec{Name: "game", Proto: tunnel.UDP, Target: net.JoinHostPort("localhost", port)})

	u, err := url.Parse(tunnels["game"].URL)
	if err != nil || u.Scheme != "udp" {
		t.Fatalf("URL = %q, want a udp:// address", tunnels["game"].URL)
	}

	public := net.JoinHostPort("127.0.0.1", u.Port())

	// Separate sockets are separate senders; each must get only its own replies.
	for _, msg := range []string{"alice", "bob", "alice again"} {
		if got := sendUDP(t, public, msg); got != "echo:"+msg {
			t.Errorf("reply to %q = %q, want %q", msg, got, "echo:"+msg)
		}
	}
}

func TestUDPUnsupportedServer(t *testing.T) {
	t.Parallel()

	ln := listen(t)

	go serveOldServer(ln)

	err := client.Run(t.Context(), &client.Options{Server: ln.Addr().String()}, []tunnel.Spec{{Name: "game", Proto: tunnel.UDP, Target: "localhost:27015"}})
	if !errors.Is(err, client.ErrUDPUnsupported) {
		t.Errorf("Run() error = %v, want ErrUDPUnsupported", err)
	}
}
