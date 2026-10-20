// Package server implements the public side of parrot: it accepts client sessions and
// forwards public HTTP requests and TCP connections to them.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	handshakeTimeout  = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 90 * time.Second
	shutdownTimeout   = 5 * time.Second
	minAcceptDelay    = 5 * time.Millisecond
	maxAcceptDelay    = time.Second
	acceptDelayFactor = 2
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
	// NoWarning skips the page that tells browser visitors an anonymous tunnel is run by someone else.
	NoWarning bool
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

// Server forwards public traffic to connected clients.
type Server struct {
	logger     *slog.Logger
	domain     string
	opts       Options
	scheme     string
	publicPort string
	landing    []byte
	warning    *warning
	speedTests chan struct{}

	mu       sync.Mutex
	hosts    map[string]*httpTunnel
	ports    map[int]bool
	sessions map[*session]struct{}
	closing  bool
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

	landing, err := renderLanding(domain, opts.AllowAnonymous)
	if err != nil {
		return nil, err
	}

	var wn *warning
	if !opts.NoWarning {
		if wn, err = newWarning(); err != nil {
			return nil, err
		}
	}

	return &Server{
		logger:     logger,
		domain:     domain,
		landing:    landing,
		warning:    wn,
		speedTests: make(chan struct{}, maxSpeedTests),
		opts:       *opts,
		hosts:      make(map[string]*httpTunnel),
		ports:      make(map[int]bool),
		sessions:   make(map[*session]struct{}),
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

func (s *Server) acceptSessions(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup

	defer func() {
		s.closeSessions()
		wg.Wait()
	}()

	for {
		conn, err := accept(ln, ctx.Done(), s.logger)
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

// accept retries temporary failures such as fd exhaustion until ln is closed or stop fires.
func accept(ln net.Listener, stop <-chan struct{}, logger *slog.Logger) (net.Conn, error) {
	delay := minAcceptDelay

	for {
		conn, err := ln.Accept()
		if err == nil {
			return conn, nil
		}

		if errors.Is(err, net.ErrClosed) {
			return nil, fmt.Errorf("accept: %w", err)
		}

		logger.Warn("accept failed, retrying", "error", err, "delay", delay)

		select {
		case <-stop:
			return nil, fmt.Errorf("accept: %w", err)
		case <-time.After(delay):
		}

		delay = min(delay*acceptDelayFactor, maxAcceptDelay)
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

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}

	return err
}
