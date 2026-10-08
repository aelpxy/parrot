package client_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/server"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const testDomain = "parrot.test"

type harness struct {
	controlAddr string
	httpAddr    string
	httpsAddr   string
	http        *http.Client
	clientTLS   *tls.Config
}

func startServer(t *testing.T) *harness {
	t.Helper()

	return startServerWith(t, &server.Options{AllowAnonymous: true})
}

// startServerWith serves opts on loopback listeners, filling in the test domain and port range.
func startServerWith(t *testing.T, opts *server.Options) *harness {
	t.Helper()

	opts.Domain = testDomain
	opts.TCPPorts = server.PortRange{First: 47000, Last: 47999}

	srv, err := server.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	ls := server.Listeners{Control: listen(t), HTTP: listen(t)}
	h := &harness{
		controlAddr: ls.Control.Addr().String(),
		httpAddr:    ls.HTTP.Addr().String(),
		http:        &http.Client{Timeout: 5 * time.Second},
	}

	if opts.TLS != nil {
		ls.HTTPS = listen(t)
		h.httpsAddr = ls.HTTPS.Addr().String()
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

	return h
}

// startClient runs specs against the harness and returns the bound tunnels by name.
func (h *harness) startClient(t *testing.T, specs ...tunnel.Spec) map[string]client.Tunnel {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	online := make(chan []client.Tunnel, 1)
	done := make(chan error, 1)

	opts := &client.Options{Server: h.controlAddr, TLS: h.clientTLS, OnOnline: func(ts []client.Tunnel) { online <- ts }}

	go func() { done <- client.Run(ctx, opts, specs) }()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("Run() error: %v", err)
		}
	})

	select {
	case ts := <-online:
		byName := make(map[string]client.Tunnel, len(ts))
		for _, tun := range ts {
			byName[tun.Spec.Name] = tun
		}

		return byName
	case err := <-done:
		t.Fatalf("Run() returned before online: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for tunnels")
	}

	return nil
}

func (h *harness) get(t *testing.T, publicURL string, auth string) (int, string) {
	t.Helper()

	u, err := url.Parse(publicURL)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+h.httpAddr+"/hello", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = u.Host

	if user, pass, ok := strings.Cut(auth, ":"); ok {
		req.SetBasicAuth(user, pass)
	}

	resp, err := h.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}

func TestHTTPTunnel(t *testing.T) {
	t.Parallel()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.URL.Path+" host="+r.Host+" xff="+r.Header.Get("X-Forwarded-For")+" authorization="+strconv.Quote(r.Header.Get("Authorization")))
	}))
	t.Cleanup(app.Close)

	appAddr := strings.TrimPrefix(app.URL, "http://")
	h := startServer(t)

	tunnels := h.startClient(t,
		tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: appAddr, Subdomain: "myapp"},
		tunnel.Spec{Name: "private", Proto: tunnel.HTTP, Target: appAddr, Auth: "admin:secret", HostHeader: "api.local"},
		tunnel.Spec{Name: "down", Proto: tunnel.HTTP, Target: closedAddr(t)},
	)

	if got := tunnels["web"].URL; !strings.HasPrefix(got, "http://myapp."+testDomain+":") {
		t.Errorf("web URL = %q, want requested subdomain", got)
	}

	tests := []struct {
		name       string
		tunnel     string
		auth       string
		wantStatus int
		wantBody   string
	}{
		{name: "forwards request", tunnel: "web", wantStatus: http.StatusOK, wantBody: "/hello host=myapp." + testDomain},
		{name: "sets forwarded for", tunnel: "web", wantStatus: http.StatusOK, wantBody: "xff=127.0.0.1"},
		{name: "requires auth", tunnel: "private", wantStatus: http.StatusUnauthorized, wantBody: "authentication required"},
		{name: "rejects wrong auth", tunnel: "private", auth: "admin:nope", wantStatus: http.StatusUnauthorized},
		{name: "accepts auth and rewrites host", tunnel: "private", auth: "admin:secret", wantStatus: http.StatusOK, wantBody: "host=api.local"},
		{name: "strips tunnel credentials", tunnel: "private", auth: "admin:secret", wantStatus: http.StatusOK, wantBody: `authorization=""`},
		{name: "local app down", tunnel: "down", wantStatus: http.StatusBadGateway, wantBody: "nothing is listening"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status, body := h.get(t, tunnels[tt.tunnel].URL, tt.auth)
			if status != tt.wantStatus || !strings.Contains(body, tt.wantBody) {
				t.Errorf("got %d %q, want %d containing %q", status, body, tt.wantStatus, tt.wantBody)
			}
		})
	}

	t.Run("unknown host", func(t *testing.T) {
		t.Parallel()

		status, _ := h.get(t, "http://nope."+testDomain, "")
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want 404", status)
		}
	})
}

func TestTCPTunnel(t *testing.T) {
	t.Parallel()

	echo := listen(t)

	go serveEcho(echo)

	h := startServer(t)
	tunnels := h.startClient(t, tunnel.Spec{Name: "echo", Proto: tunnel.TCP, Target: echo.Addr().String()})

	u, err := url.Parse(tunnels["echo"].URL)
	if err != nil {
		t.Fatal(err)
	}

	d := net.Dialer{Timeout: 5 * time.Second}

	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", u.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "ping" {
		t.Errorf("echo = %q, want %q", got, "ping")
	}
}

func TestSubdomainTaken(t *testing.T) {
	t.Parallel()

	h := startServer(t)
	h.startClient(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: "localhost:1", Subdomain: "taken"})

	opts := &client.Options{Server: h.controlAddr}
	err := client.Run(t.Context(), opts, []tunnel.Spec{{Name: "web", Proto: tunnel.HTTP, Target: "localhost:1", Subdomain: "taken"}})

	var remote *protocol.RemoteError
	if !errors.As(err, &remote) || !strings.Contains(remote.Message, "not available") {
		t.Errorf("Run() error = %v, want subdomain unavailable", err)
	}
}

func TestServerUnreachable(t *testing.T) {
	t.Parallel()

	opts := &client.Options{Server: closedAddr(t)}

	err := client.Run(t.Context(), opts, []tunnel.Spec{{Name: "web", Proto: tunnel.HTTP, Target: "localhost:1"}})
	if err == nil {
		t.Error("Run() error = nil, want connection error")
	}
}

func listen(t *testing.T) net.Listener {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	return ln
}

// serveEcho replies with each request only after it ends, so half-closes must cross the tunnel.
func serveEcho(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		go func() {
			defer func() { _ = conn.Close() }()

			if data, err := io.ReadAll(conn); err == nil {
				_, _ = conn.Write(data)
			}
		}()
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()

	ln := listen(t)
	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	return addr
}

func TestReconnectKeepsSubdomain(t *testing.T) {
	t.Parallel()

	controlLn := listen(t)
	controlAddr := controlLn.Addr().String()
	stopFirst := serveOn(t, controlLn, &server.Options{AllowAnonymous: true})

	online := make(chan []client.Tunnel, 2)
	offline := make(chan struct{}, 1)
	done := make(chan error, 1)

	ctx, cancel := context.WithCancel(t.Context())
	opts := &client.Options{
		Server:    controlAddr,
		OnOnline:  func(ts []client.Tunnel) { online <- ts },
		OnOffline: func(error, time.Duration) { offline <- struct{}{} },
	}

	go func() {
		done <- client.Run(ctx, opts, []tunnel.Spec{{Name: "web", Proto: tunnel.HTTP, Target: "localhost:1"}})
	}()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("Run() error: %v", err)
		}
	})

	first := waitFor(t, online)

	stopFirst()
	waitFor(t, offline)

	var lc net.ListenConfig

	relisten, err := lc.Listen(t.Context(), "tcp", controlAddr)
	if err != nil {
		t.Skipf("could not rebind %s: %v", controlAddr, err)
	}

	t.Cleanup(func() { _ = relisten.Close() })
	serveOn(t, relisten, &server.Options{AllowAnonymous: true})

	second := waitFor(t, online)
	if first[0].Spec.Subdomain == "" || first[0].Spec.Subdomain != second[0].Spec.Subdomain {
		t.Errorf("subdomain changed across reconnect: %q -> %q", first[0].Spec.Subdomain, second[0].Spec.Subdomain)
	}
}

// serveOn runs a server for opts on controlLn and returns a func that stops it and waits.
func serveOn(t *testing.T, controlLn net.Listener, opts *server.Options) func() {
	t.Helper()

	opts.Domain = testDomain
	opts.TCPPorts = server.PortRange{First: 47000, Last: 47999}

	srv, err := server.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	httpLn := listen(t)

	var (
		wg   sync.WaitGroup
		once sync.Once
	)

	wg.Go(func() {
		if err := srv.Serve(ctx, server.Listeners{Control: controlLn, HTTP: httpLn}); err != nil {
			t.Errorf("Serve() error: %v", err)
		}
	})

	stop := func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}

	t.Cleanup(stop)

	return stop
}

func waitFor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()

	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}

	var zero T

	return zero
}
