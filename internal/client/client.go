// Package client connects to a parrot server and forwards its streams to local targets.
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/sys/unix"

	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/relay"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 10 * time.Second
	localDialTimeout = 5 * time.Second
	minBackoff       = 500 * time.Millisecond
	maxBackoff       = 5 * time.Second
	backoffFactor    = 2
	jitterFraction   = 5
)

// ErrRejected is returned when the server refuses the handshake, e.g. for a revoked token; retrying cannot fix it.
var ErrRejected = errors.New("rejected by server")

// Options configures Run. The callbacks are optional and called from Run's goroutines.
type Options struct {
	Logger *slog.Logger
	// TLS secures the connection to the server; nil connects in plaintext.
	TLS           *tls.Config
	OnOnline      func([]Tunnel)
	OnOffline     func(err error, retryIn time.Duration)
	OnStreamError func(tunnel string, err error)
	Server        string
	Token         string
}

// LocalError reports that a tunnel's local target could not be reached.
type LocalError struct {
	Err    error
	Target string
}

func (e *LocalError) Error() string {
	if errors.Is(e.Err, unix.ECONNREFUSED) {
		return "nothing is listening on " + e.Target + ", is your app running?"
	}

	return "can't reach " + e.Target + ": " + e.Err.Error()
}

func (e *LocalError) Unwrap() error { return e.Err }

// Tunnel is a spec that the server has accepted.
type Tunnel struct {
	URL  string
	Spec tunnel.Spec
}

type runner struct {
	opts   *Options
	logger *slog.Logger
	specs  []tunnel.Spec
}

// Check connects to the server and verifies the token without binding any tunnels.
func Check(ctx context.Context, opts *Options) error {
	r := &runner{opts: opts, logger: slog.New(slog.DiscardHandler)}

	mux, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer r.closeMux(mux)

	_, _, err = r.handshake(mux)

	return err
}

// Run exposes specs through the server until ctx is canceled, reconnecting after drops.
// It fails fast if the very first connection or any bind is rejected, and stops once the server refuses the handshake.
func Run(ctx context.Context, opts *Options, specs []tunnel.Spec) error {
	if err := checkNames(specs); err != nil {
		return err
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	r := &runner{opts: opts, logger: logger, specs: specs}
	backoff := minBackoff
	everOnline := false

	for {
		online, err := r.session(ctx)

		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if (!everOnline && !online) || errors.Is(err, ErrRejected) {
			return err
		}

		everOnline = true

		if online {
			backoff = minBackoff
		}

		wait := jitter(backoff)
		r.offline(err, wait)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}

		backoff = min(backoff*backoffFactor, maxBackoff)
	}
}

// jitter adds up to a fifth to d so clients don't all reconnect at once after a server restart.
func jitter(d time.Duration) time.Duration {
	return d + rand.N(d/jitterFraction)
}

// session runs one connection and reports whether its tunnels came online.
func (r *runner) session(ctx context.Context) (bool, error) {
	mux, err := r.connect(ctx)
	if err != nil {
		return false, err
	}
	defer r.closeMux(mux)

	stop := context.AfterFunc(ctx, func() { r.closeMux(mux) })
	defer stop()

	tunnels, err := r.setup(mux)
	if err != nil {
		return false, err
	}

	if r.opts.OnOnline != nil {
		r.opts.OnOnline(tunnels)
	}

	return true, r.serve(ctx, mux)
}

func (r *runner) connect(ctx context.Context) (*yamux.Session, error) {
	d := &net.Dialer{Timeout: dialTimeout}

	var (
		conn net.Conn
		err  error
	)

	if r.opts.TLS != nil {
		td := &tls.Dialer{NetDialer: d, Config: r.opts.TLS}
		conn, err = td.DialContext(ctx, "tcp", r.opts.Server)
	} else {
		conn, err = d.DialContext(ctx, "tcp", r.opts.Server)
	}

	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", r.opts.Server, err)
	}

	mux, err := yamux.Client(conn, protocol.MuxConfig(r.logger))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("start session: %w", err), conn.Close())
	}

	return mux, nil
}

// handshake opens the control stream and authenticates, leaving a handshake deadline set on it.
func (r *runner) handshake(mux *yamux.Session) (*protocol.Control, net.Conn, error) {
	stream, err := mux.OpenStream()
	if err != nil {
		return nil, nil, fmt.Errorf("open control stream: %w", err)
	}

	if err := stream.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	ctrl := protocol.NewControl(stream)
	hello := &protocol.Message{Kind: protocol.KindHello, Hello: &protocol.Hello{Version: protocol.Version, Token: r.opts.Token}}

	if _, err := ctrl.Request(hello, protocol.KindWelcome); err != nil {
		if _, ok := errors.AsType[*protocol.RemoteError](err); ok {
			return nil, nil, fmt.Errorf("handshake: %w: %w", ErrRejected, err)
		}

		return nil, nil, fmt.Errorf("handshake: %w", err)
	}

	return ctrl, stream, nil
}

// setup authenticates and binds every spec, remembering what the server assigned.
func (r *runner) setup(mux *yamux.Session) ([]Tunnel, error) {
	ctrl, stream, err := r.handshake(mux)
	if err != nil {
		return nil, err
	}

	tunnels := make([]Tunnel, 0, len(r.specs))

	for i := range r.specs {
		spec := &r.specs[i]

		reply, err := ctrl.Request(&protocol.Message{Kind: protocol.KindBind, Bind: bindFor(spec)}, protocol.KindBound)
		if err != nil {
			return nil, fmt.Errorf("tunnel %s: %w", spec.Name, err)
		}

		if reply.Bound == nil {
			return nil, fmt.Errorf("tunnel %s: %w: empty bound reply", spec.Name, protocol.ErrUnexpectedMessage)
		}

		spec.Subdomain = reply.Bound.Subdomain
		spec.RemotePort = reply.Bound.RemotePort
		tunnels = append(tunnels, Tunnel{URL: reply.Bound.URL, Spec: *spec})
	}

	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear handshake deadline: %w", err)
	}

	return tunnels, nil
}

func (r *runner) serve(ctx context.Context, mux *yamux.Session) error {
	specs := make(map[string]tunnel.Spec, len(r.specs))
	for _, s := range r.specs {
		specs[s.Name] = s
	}

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		stream, err := mux.AcceptStream()
		if err != nil {
			return fmt.Errorf("connection lost: %w", err)
		}

		wg.Go(func() {
			r.handleStream(ctx, stream, specs)
		})
	}
}

func (r *runner) handleStream(ctx context.Context, stream net.Conn, specs map[string]tunnel.Spec) {
	h, err := protocol.ReadStreamHeader(stream)
	if err != nil {
		r.logger.Debug("read stream header failed", "error", err)
		r.closeConn(stream)

		return
	}

	spec, ok := specs[h.Tunnel]
	if !ok {
		r.logger.Debug("stream for unknown tunnel", "tunnel", h.Tunnel)
		r.closeConn(stream)

		return
	}

	d := net.Dialer{Timeout: localDialTimeout}

	local, err := d.DialContext(ctx, "tcp", spec.Target)
	if err != nil {
		if r.opts.OnStreamError != nil {
			r.opts.OnStreamError(spec.Name, &LocalError{Err: err, Target: spec.Target})
		}

		if spec.Proto == tunnel.HTTP {
			writeBadGateway(stream, spec.Target)
		}

		r.closeConn(stream)

		return
	}

	if err := relay.Join(stream, local); err != nil {
		r.logger.Debug("relay ended", "tunnel", spec.Name, "error", err)
	}
}

func (r *runner) offline(err error, retryIn time.Duration) {
	if r.opts.OnOffline != nil {
		r.opts.OnOffline(err, retryIn)
	}
}

func (r *runner) closeMux(mux *yamux.Session) {
	if err := mux.Close(); err != nil {
		r.logger.Debug("close session failed", "error", err)
	}
}

func (r *runner) closeConn(conn net.Conn) {
	if err := conn.Close(); err != nil {
		r.logger.Debug("close stream failed", "error", err)
	}
}

func bindFor(s *tunnel.Spec) *protocol.Bind {
	return &protocol.Bind{
		Name:       s.Name,
		Proto:      s.Proto,
		Subdomain:  s.Subdomain,
		Auth:       s.Auth,
		HostHeader: s.HostHeader,
		RemotePort: s.RemotePort,
	}
}

func checkNames(specs []tunnel.Spec) error {
	if len(specs) == 0 {
		return errors.New("no tunnels to start")
	}

	seen := make(map[string]bool, len(specs))

	for _, s := range specs {
		if s.Name == "" {
			return errors.New("tunnel without a name")
		}

		if seen[s.Name] {
			return fmt.Errorf("duplicate tunnel name %q", s.Name)
		}

		seen[s.Name] = true
	}

	return nil
}

// writeBadGateway answers an HTTP stream whose local target is down, so the browser shows why.
func writeBadGateway(stream net.Conn, target string) {
	body := "parrot: nothing is listening on " + target + ". Start your app and refresh.\n"
	resp := "HTTP/1.1 502 Bad Gateway\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n\r\n" + body

	// The public client may already be gone; the dial error was reported via OnStreamError.
	_, _ = stream.Write([]byte(resp))
}
