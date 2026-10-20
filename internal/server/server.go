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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
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

	// SSHHostKey enables tunnels from plain `ssh -R`; without it the SSH options are ignored.
	SSHHostKey ssh.Signer
	// SSHAddr is a dedicated SSH address for Run, such as ":2222".
	SSHAddr string
	// SSHOnHTTPS also accepts SSH on the HTTPS port, telling the two apart by their first bytes.
	SSHOnHTTPS bool
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
	// SSH is a dedicated listener for `ssh -R` clients; it needs Options.SSHHostKey.
	SSH net.Listener
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
	udpPorts map[int]bool
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

	var (
		wn  *warning
		err error
	)

	if !opts.NoWarning {
		if wn, err = newWarning(); err != nil {
			return nil, err
		}
	}

	return &Server{
		logger:     logger,
		domain:     domain,
		warning:    wn,
		speedTests: make(chan struct{}, maxSpeedTests),
		opts:       *opts,
		hosts:      make(map[string]*httpTunnel),
		ports:      make(map[int]bool),
		udpPorts:   make(map[int]bool),
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

	if s.opts.SSHAddr != "" && s.opts.SSHHostKey != nil {
		ln, err := lc.Listen(ctx, "tcp", s.opts.SSHAddr)
		if err != nil {
			for _, open := range lns {
				err = errors.Join(err, open.Close())
			}

			return fmt.Errorf("listen %s: %w", s.opts.SSHAddr, err)
		}

		ls.SSH = ln
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
	sshLns, split, err := s.sshListeners(&ls)
	if err != nil {
		return err
	}

	s.landing, err = renderLanding(s.domain, s.opts.AllowAnonymous, s.landingSSHCommand(sshLns))
	if err != nil {
		return err
	}

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
		"ssh", len(sshLns) > 0,
	)

	g, gctx := errgroup.WithContext(ctx)

	for _, e := range endpoints {
		g.Go(e.serve)
	}

	s.startSSH(gctx, g, sshLns, split)

	g.Go(func() error {
		return s.acceptSessions(gctx, control)
	})

	g.Go(func() error {
		<-gctx.Done()

		return shutdown(context.WithoutCancel(gctx), control, sshLns, endpoints)
	})

	if err := g.Wait(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	return nil
}

// shutdown stops accepting clients, then gives in-flight public requests shutdownTimeout to finish.
func shutdown(ctx context.Context, control net.Listener, sshLns []net.Listener, endpoints []httpEndpoint) error {
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	errs := make([]error, 0, 1+len(sshLns)+len(endpoints))
	errs = append(errs, ignoreClosed(control.Close()))

	for _, ln := range sshLns {
		errs = append(errs, ignoreClosed(ln.Close()))
	}

	for _, e := range endpoints {
		errs = append(errs, shutdownHTTP(ctx, e.srv))
	}

	return errors.Join(errs...)
}

// landingSSHCommand shows visitors how to tunnel with plain ssh through the port SSH clients
// actually reach, or "" when SSH is off.
func (s *Server) landingSSHCommand(sshLns []net.Listener) string {
	if len(sshLns) == 0 {
		return ""
	}

	user := ""
	if !s.opts.AllowAnonymous {
		user = "<token>"
	}

	// The shared HTTPS port comes last and is the one most networks allow, so prefer it.
	return sshCommand(s.domain, portOf(sshLns[len(sshLns)-1].Addr()), user)
}

// sshListeners collects where SSH clients connect, splitting the HTTPS listener in ls when asked to.
func (s *Server) sshListeners(ls *Listeners) ([]net.Listener, *splitter, error) {
	if s.opts.SSHHostKey == nil {
		if ls.SSH != nil || s.opts.SSHOnHTTPS {
			return nil, nil, errors.New("ssh needs a host key")
		}

		return nil, nil, nil
	}

	var lns []net.Listener
	if ls.SSH != nil {
		lns = append(lns, ls.SSH)
	}

	if !s.opts.SSHOnHTTPS {
		return lns, nil, nil
	}

	if s.opts.TLS == nil || ls.HTTPS == nil {
		return nil, nil, errors.New("ssh on the https port needs tls")
	}

	split := newSplitter(ls.HTTPS, s.logger)

	var sshLn net.Listener

	ls.HTTPS, sshLn = split.listeners()

	return append(lns, sshLn), split, nil
}

// startSSH serves SSH clients on lns, routing the shared HTTPS port first when split is set.
func (s *Server) startSSH(ctx context.Context, g *errgroup.Group, lns []net.Listener, split *splitter) {
	if split != nil {
		g.Go(func() error {
			split.run()

			return nil
		})
	}

	if len(lns) == 0 {
		return
	}

	cfg := s.sshConfig()
	s.logger.Info("ssh tunnels enabled", "host_key", ssh.FingerprintSHA256(s.opts.SSHHostKey.PublicKey()))

	for _, ln := range lns {
		g.Go(func() error {
			return s.acceptSSH(ctx, ln, cfg)
		})
	}
}

// publicPortNumber is the port public URLs use, which SSH clients see as their forward's port.
func (s *Server) publicPortNumber() uint32 {
	port := strings.TrimPrefix(s.publicPort, ":")
	if port == "" {
		port = defaultHTTPPort
		if s.scheme == "https" {
			port = defaultHTTPSPort
		}
	}

	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0
	}

	return uint32(n)
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
		ss.disconnect()
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
