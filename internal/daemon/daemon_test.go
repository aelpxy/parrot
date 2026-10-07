package daemon_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/server"
	"github.com/aelpxy/parrot/internal/tunnel"
)

func listen(t *testing.T, network, addr string) net.Listener {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), network, addr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

// serve runs fn in the background until the test ends.
func serve(t *testing.T, fn func(ctx context.Context) error) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup

	wg.Go(func() {
		if err := fn(ctx); err != nil {
			t.Errorf("serve error: %v", err)
		}
	})

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

type env struct {
	client  *daemon.Client
	server  string
	httpURL string
	app     string
}

func setup(t *testing.T) *env {
	t.Helper()

	srv, err := server.New(&server.Options{Domain: "parrot.test", TCPPorts: server.PortRange{First: 48000, Last: 48999}, AllowAnonymous: true})
	if err != nil {
		t.Fatal(err)
	}

	ls := server.Listeners{Control: listen(t, "tcp", "127.0.0.1:0"), HTTP: listen(t, "tcp", "127.0.0.1:0")}
	serve(t, func(ctx context.Context) error { return srv.Serve(ctx, ls) })

	socket := filepath.Join(t.TempDir(), "parrot.sock")
	sockLn := listen(t, "unix", socket)
	d := daemon.New(nil)

	serve(t, func(ctx context.Context) error { return d.Serve(ctx, sockLn) })

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "from app")
	}))
	t.Cleanup(app.Close)

	return &env{
		client:  daemon.NewClient(socket),
		server:  ls.Control.Addr().String(),
		httpURL: "http://" + ls.HTTP.Addr().String(),
		app:     strings.TrimPrefix(app.URL, "http://"),
	}
}

func (e *env) start(t *testing.T, specs ...tunnel.Spec) ([]daemon.TunnelInfo, error) {
	t.Helper()

	return e.client.Start(t.Context(), &daemon.StartRequest{Server: e.server, Specs: specs})
}

func TestDaemonLifecycle(t *testing.T) {
	t.Parallel()

	e := setup(t)

	infos, err := e.start(t,
		tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: e.app, Subdomain: "bg"},
		tunnel.Spec{Name: "db", Proto: tunnel.TCP, Target: e.app},
	)
	if err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if len(infos) != 2 || infos[0].Status != daemon.StatusOnline || !strings.HasPrefix(infos[0].URL, "http://bg.parrot.test") {
		t.Fatalf("Start() = %+v, want two online tunnels", infos)
	}

	if body := get(t, e.httpURL, "bg.parrot.test"); body != "from app" {
		t.Errorf("tunnel body = %q, want %q", body, "from app")
	}

	if _, err := e.start(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: e.app}); !isStatus(err, http.StatusConflict) {
		t.Errorf("Start() duplicate error = %v, want 409", err)
	}

	listed, err := e.client.List(t.Context())
	if err != nil || len(listed) != 2 || listed[0].Name != "db" || listed[1].Name != "web" {
		t.Fatalf("List() = %+v, %v; want db and web", listed, err)
	}

	var lines []string

	if err := e.client.Logs(t.Context(), "web", func(l daemon.LogLine) { lines = append(lines, l.Message) }); err != nil {
		t.Fatal(err)
	}

	if !slicesContain(lines, "online at http://bg.parrot.test") {
		t.Errorf("Logs() = %q, want an online line", lines)
	}

	followUntilStopped(t, e)

	names, err := e.client.StopAll(t.Context())
	if err != nil || len(names) != 1 || names[0] != "db" {
		t.Errorf("StopAll() = %v, %v; want [db]", names, err)
	}

	if err := e.client.Stop(t.Context(), "web"); !isStatus(err, http.StatusNotFound) {
		t.Errorf("Stop() unknown error = %v, want 404", err)
	}
}

// followUntilStopped follows web's logs and checks that stopping it ends the stream.
func followUntilStopped(t *testing.T, e *env) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	got := make(chan daemon.LogLine, 16)
	done := make(chan error, 1)

	go func() { done <- e.client.FollowLogs(ctx, "web", func(l daemon.LogLine) { got <- l }) }()

	select {
	case <-got:
	case <-ctx.Done():
		t.Fatal("FollowLogs() sent no backlog")
	}

	if err := e.client.Stop(t.Context(), "web"); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("FollowLogs() error: %v", err)
		}
	case <-ctx.Done():
		t.Error("FollowLogs() did not end after Stop")
	}
}

func TestDaemonStartFailure(t *testing.T) {
	t.Parallel()

	e := setup(t)
	e.server = "127.0.0.1:1"

	_, err := e.start(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: e.app})
	if !isStatus(err, http.StatusBadGateway) || !strings.Contains(err.Error(), "connect") {
		t.Errorf("Start() error = %v, want 502 connect failure", err)
	}

	listed, err := e.client.List(t.Context())
	if err != nil || len(listed) != 0 {
		t.Errorf("List() after failed start = %+v, %v; want empty", listed, err)
	}
}

func TestShutdown(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(t.TempDir(), "parrot.sock")
	ln := listen(t, "unix", socket)
	done := make(chan error, 1)

	go func() { done <- daemon.New(nil).Serve(t.Context(), ln) }()

	if err := daemon.NewClient(socket).Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() error after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve() did not return after Shutdown")
	}
}

func TestNotRunning(t *testing.T) {
	t.Parallel()

	c := daemon.NewClient(filepath.Join(t.TempDir(), "missing.sock"))
	if _, err := c.List(t.Context()); !errors.Is(err, daemon.ErrNotRunning) {
		t.Errorf("List() error = %v, want ErrNotRunning", err)
	}
}

func get(t *testing.T, base, host string) string {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = host

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

func isStatus(err error, status int) bool {
	var apiErr *daemon.APIError

	return errors.As(err, &apiErr) && apiErr.Status == status
}

func slicesContain(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}

	return false
}
