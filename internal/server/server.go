// Package server implements the public side of parrot: it accepts client sessions and
// forwards public HTTP requests and TCP connections to them.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/aelpxy/parrot/internal/tunnel"
)

const (
	handshakeTimeout  = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 90 * time.Second
	shutdownTimeout   = 5 * time.Second
	maxNameAttempts   = 16
	defaultHTTPPort   = "80"
	defaultHTTPSPort  = "443"
)

// ErrUnavailable is returned when a requested subdomain or port is taken.
var ErrUnavailable = errors.New("not available")

// Options configures a Server.
type Options struct {
	Logger *slog.Logger
	// Auth verifies client tokens; it may only be nil when AllowAnonymous is set.
	Auth Authenticator
	// TLS enables TLS for the control channel and public HTTPS; HTTP then redirects to HTTPS.
	TLS            *tls.Config
	Domain         string
	ControlAddr    string
	HTTPAddr       string
	HTTPSAddr      string
	TCPPorts       PortRange
	AllowAnonymous bool
}

// Authenticator verifies a client token and returns a label for logs.
type Authenticator interface {
	Authenticate(token string) (string, error)
}

// Listeners are the sockets Serve accepts on. HTTPS is required when Options.TLS is set.
type Listeners struct {
	Control net.Listener
	HTTP    net.Listener
	HTTPS   net.Listener
}

// PortRange is an inclusive range of ports.
type PortRange struct {
	First int
	Last  int
}

// Server forwards public traffic to connected clients.
type Server struct {
	logger     *slog.Logger
	domain     string
	opts       Options
	scheme     string
	publicPort string

	mu       sync.Mutex
	hosts    map[string]*httpTunnel
	ports    map[int]bool
	sessions map[*session]struct{}
	closing  bool
}

// ParsePortRange parses "30000-40000".
func ParsePortRange(s string) (PortRange, error) {
	first, last, ok := strings.Cut(s, "-")
	if !ok {
		return PortRange{}, fmt.Errorf("port range %q: expected first-last", s)
	}

	r := PortRange{}

	var errFirst, errLast error

	r.First, errFirst = strconv.Atoi(first)
	r.Last, errLast = strconv.Atoi(last)

	if errFirst != nil || errLast != nil || r.First < 1 || r.Last > 65535 || r.First > r.Last {
		return PortRange{}, fmt.Errorf("port range %q: expected first-last within 1-65535", s)
	}

	return r, nil
}

func (r PortRange) String() string {
	return strconv.Itoa(r.First) + "-" + strconv.Itoa(r.Last)
}

func (r PortRange) contains(port int) bool {
	return port >= r.First && port <= r.Last
}

// New returns a Server for opts.
func New(opts *Options) (*Server, error) {
	domain := strings.ToLower(strings.Trim(opts.Domain, "."))
	if domain == "" {
		return nil, errors.New("missing domain")
	}

	if opts.TCPPorts == (PortRange{}) {
		return nil, errors.New("missing tcp port range")
	}

	if opts.Auth == nil && !opts.AllowAnonymous {
		return nil, errors.New("missing authenticator; set AllowAnonymous to accept any client")
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Server{
		logger:   logger,
		domain:   domain,
		opts:     *opts,
		hosts:    make(map[string]*httpTunnel),
		ports:    make(map[int]bool),
		sessions: make(map[*session]struct{}),
	}, nil
}

// Run listens on the configured addresses and serves until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	addrs := []string{s.opts.ControlAddr, s.opts.HTTPAddr}
	if s.opts.TLS != nil {
		addrs = append(addrs, s.opts.HTTPSAddr)
	}

	var (
		lc  net.ListenConfig
		lns []net.Listener
	)

	for _, addr := range addrs {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, open := range lns {
				err = errors.Join(err, open.Close())
			}

			return fmt.Errorf("listen %s: %w", addr, err)
		}

		lns = append(lns, ln)
	}

	ls := Listeners{Control: lns[0], HTTP: lns[1]}
	if s.opts.TLS != nil {
		ls.HTTPS = lns[2]
	}

	return s.Serve(ctx, ls)
}

type httpEndpoint struct {
	srv *http.Server
	ln  net.Listener
	tls bool
}

// Serve accepts client sessions and public traffic on ls until ctx is canceled.
func (s *Server) Serve(ctx context.Context, ls Listeners) error {
	endpoints, err := s.endpoints(ls)
	if err != nil {
		return err
	}

	control := ls.Control
	if s.opts.TLS != nil {
		control = tls.NewListener(control, s.opts.TLS)
	}

	s.logger.Info("server listening",
		"control_addr", control.Addr().String(),
		"public_url", s.scheme+"://*."+s.domain+s.publicPort,
		"tcp_ports", s.opts.TCPPorts.String(),
		"anonymous", s.opts.AllowAnonymous,
	)

	g, gctx := errgroup.WithContext(ctx)

	for _, e := range endpoints {
		g.Go(e.serve)
	}

	g.Go(func() error {
		return s.acceptSessions(gctx, control)
	})

	g.Go(func() error {
		<-gctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(gctx), shutdownTimeout)
		defer cancel()

		errs := make([]error, 0, 1+len(endpoints))
		errs = append(errs, ignoreClosed(control.Close()))

		for _, e := range endpoints {
			errs = append(errs, shutdownHTTP(shutdownCtx, e.srv))
		}

		return errors.Join(errs...)
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// endpoints serves tunnels over HTTPS when TLS is on, with HTTP redirecting; otherwise over HTTP.
func (s *Server) endpoints(ls Listeners) ([]httpEndpoint, error) {
	if s.opts.TLS == nil {
		s.scheme, s.publicPort = "http", publicPort(ls.HTTP.Addr(), defaultHTTPPort)

		return []httpEndpoint{{srv: s.httpServer(s), ln: ls.HTTP}}, nil
	}

	if ls.HTTPS == nil {
		return nil, errors.New("tls is enabled but no https listener was given")
	}

	s.scheme, s.publicPort = "https", publicPort(ls.HTTPS.Addr(), defaultHTTPSPort)

	https := s.httpServer(s)
	https.TLSConfig = s.opts.TLS

	return []httpEndpoint{
		{srv: https, ln: ls.HTTPS, tls: true},
		{srv: s.httpServer(http.HandlerFunc(s.redirectHTTPS)), ln: ls.HTTP},
	}, nil
}

func (s *Server) httpServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.logger.Handler(), slog.LevelDebug),
	}
}

func (e httpEndpoint) serve() error {
	var err error
	if e.tls {
		err = e.srv.ServeTLS(e.ln, "", "")
	} else {
		err = e.srv.Serve(e.ln)
	}

	if !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve %s: %w", e.ln.Addr(), err)
	}

	return nil
}

func (s *Server) redirectHTTPS(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://"+hostOnly(r.Host)+s.publicPort+r.URL.RequestURI(), http.StatusMovedPermanently)
}

func (s *Server) acceptSessions(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup

	defer func() {
		s.closeSessions()
		wg.Wait()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			return fmt.Errorf("accept session: %w", err)
		}

		wg.Go(func() {
			s.serveSession(ctx, conn)
		})
	}
}

func (s *Server) closeSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closing = true

	for ss := range s.sessions {
		ss.closeMux()
	}
}

// ServeHTTP routes public requests to the tunnel matching the Host header.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t := s.lookupHost(r.Host)
	if t == nil {
		writePage(w, http.StatusNotFound, "No tunnel is running at "+r.Host+".")

		return
	}

	t.ServeHTTP(w, r)
}

func (s *Server) lookupHost(hostport string) *httpTunnel {
	sub, ok := strings.CutSuffix(strings.ToLower(hostOnly(hostport)), "."+s.domain)
	if !ok || strings.Contains(sub, ".") {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.hosts[sub]
}

// registerHost claims requested, or a random subdomain when requested is empty.
func (s *Server) registerHost(requested string, t *httpTunnel) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if requested != "" {
		if !tunnel.ValidSubdomain(requested) {
			return "", fmt.Errorf("invalid subdomain %q", requested)
		}

		if _, taken := s.hosts[requested]; taken {
			return "", fmt.Errorf("subdomain %q is %w", requested, ErrUnavailable)
		}

		s.hosts[requested] = t

		return requested, nil
	}

	for range maxNameAttempts {
		name := randomName()
		if _, taken := s.hosts[name]; !taken {
			s.hosts[name] = t

			return name, nil
		}
	}

	return "", errors.New("could not allocate a subdomain")
}

// listenTCP opens requested, or a random free port in range when requested is zero.
func (s *Server) listenTCP(ctx context.Context, requested int) (net.Listener, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.opts.TCPPorts

	if requested != 0 {
		if !r.contains(requested) {
			return nil, 0, fmt.Errorf("port %d is outside the server's range %s", requested, r)
		}

		if s.ports[requested] {
			return nil, 0, fmt.Errorf("port %d is %w", requested, ErrUnavailable)
		}

		ln, err := listenPort(ctx, requested)
		if err != nil {
			return nil, 0, fmt.Errorf("port %d is %w", requested, ErrUnavailable)
		}

		s.ports[requested] = true

		return ln, requested, nil
	}

	size := r.Last - r.First + 1
	start := rand.IntN(size)

	for i := range size {
		port := r.First + (start+i)%size
		if s.ports[port] {
			continue
		}

		if ln, err := listenPort(ctx, port); err == nil {
			s.ports[port] = true

			return ln, port, nil
		}
	}

	return nil, 0, errors.New("no free tcp ports")
}

func (s *Server) release(subdomains []string, ports []int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sub := range subdomains {
		delete(s.hosts, sub)
	}

	for _, port := range ports {
		delete(s.ports, port)
	}
}

// shutdownHTTP drains requests, then force-closes connections that never sent one (e.g. browser preconnects).
func shutdownHTTP(ctx context.Context, srv *http.Server) error {
	err := srv.Shutdown(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		if err := srv.Close(); err != nil {
			return fmt.Errorf("close http: %w", err)
		}

		return nil
	}

	if err != nil {
		return fmt.Errorf("shutdown http: %w", err)
	}

	return nil
}

func listenPort(ctx context.Context, port int) (net.Listener, error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen tcp %d: %w", port, err)
	}

	return ln, nil
}

// publicPort returns ":port" for URLs, or "" when port is the scheme default.
func publicPort(addr net.Addr, defaultPort string) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil || port == defaultPort {
		return ""
	}

	return ":" + port
}

func hostOnly(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport
	}

	return host
}

func writePage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)

	// The client may already be gone; there is no one left to report a failed write to.
	_, _ = w.Write([]byte("parrot: " + msg + "\n"))
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}
