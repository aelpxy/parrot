package server_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/server"
)

const testDomain = "parrot.test"

// flakyListener fails its first Accept the way an exhausted fd table does.
type flakyListener struct {
	net.Listener

	once sync.Once
}

func (l *flakyListener) Accept() (net.Conn, error) {
	var failed bool

	l.once.Do(func() { failed = true })

	if failed {
		return nil, errors.New("accept: too many open files")
	}

	return l.Listener.Accept()
}

func listen(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	return ln
}

// serve runs a server on ls until the test ends.
func serve(t *testing.T, opts *server.Options, ls server.Listeners) {
	t.Helper()

	opts.Domain = testDomain
	opts.TCPPorts = server.PortRange{First: 48000, Last: 48999}

	srv, err := server.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Go(func() {
		if err := srv.Serve(ctx, ls); err != nil {
			t.Errorf("Serve() error: %v", err)
		}
	})

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// handshake opens a raw session to addr and sends hello.
func handshake(t *testing.T, addr string, hello *protocol.Hello) error {
	t.Helper()

	var d net.Dialer

	conn, err := d.DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}

	mux, err := yamux.Client(conn, protocol.MuxConfig(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = mux.Close() })

	stream, err := mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}

	_, err = protocol.NewControl(stream).Request(&protocol.Message{Kind: protocol.KindHello, Hello: hello}, protocol.KindWelcome)

	return err
}

func TestServeSurvivesAcceptError(t *testing.T) {
	t.Parallel()

	ln := &flakyListener{Listener: listen(t)}
	serve(t, &server.Options{AllowAnonymous: true}, server.Listeners{Control: ln, HTTP: listen(t)})

	if err := handshake(t, ln.Addr().String(), &protocol.Hello{Version: protocol.Version}); err != nil {
		t.Fatalf("handshake error: %v", err)
	}
}
