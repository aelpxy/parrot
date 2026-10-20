package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/relay"
	"github.com/aelpxy/parrot/internal/tunnel"
)

// client is the connection a session's tunnels reach their client over: yamux for parrot clients, SSH for ssh -R.
type client interface {
	// open starts a connection to the client for one visitor of tunnel.
	open(tunnel, remoteAddr string) (net.Conn, error)
	// done is closed once the client has disconnected.
	done() <-chan struct{}
	close() error
}

// muxClient reaches a parrot client over its yamux session.
type muxClient struct {
	mux *yamux.Session
}

func (c muxClient) open(tunnel, remoteAddr string) (net.Conn, error) {
	return openStream(c.mux, protocol.StreamHeader{Tunnel: tunnel, RemoteAddr: remoteAddr})
}

func (c muxClient) done() <-chan struct{} { return c.mux.CloseChan() }

func (c muxClient) close() error {
	if err := c.mux.Close(); err != nil {
		return fmt.Errorf("close session: %w", err)
	}

	return nil
}

// session is one connected client and the tunnels it has bound.
type session struct {
	srv        *Server
	client     client
	logger     *slog.Logger
	names      map[string]bool
	subdomains []string
	ports      []int
	udpPorts   []int
	listeners  []net.Listener
	packets    []net.PacketConn
	tunnels    []*httpTunnel
	wg         sync.WaitGroup
	anonymous  bool
}

func (s *Server) serveSession(ctx context.Context, conn net.Conn) {
	logger := s.logger.With("client", conn.RemoteAddr().String())

	mux, err := yamux.Server(conn, protocol.MuxConfig(logger))
	if err != nil {
		logger.Warn("start session failed", "error", err)

		return
	}

	ss := &session{srv: s, client: muxClient{mux: mux}, logger: logger, names: make(map[string]bool)}

	if !s.track(ss) {
		ss.disconnect()

		return
	}

	err = ss.serve(ctx, mux)

	s.untrack(ss)
	ss.close()
	ss.logEnd(err)
}

// rejectError is a handshake refusal that was already reported to the client.
type rejectError struct {
	reason string
}

func (e *rejectError) Error() string { return e.reason }

// logEnd reports how a session ended, treating ordinary disconnects as routine.
func (ss *session) logEnd(err error) {
	var rejected *rejectError

	switch {
	case errors.As(err, &rejected):
		ss.logger.Warn("client rejected", "reason", rejected.reason)
	case errors.Is(err, io.EOF), errors.Is(err, yamux.ErrSessionShutdown), errors.Is(err, net.ErrClosed):
		ss.logger.Info("client disconnected")
	default:
		ss.logger.Warn("client dropped", "error", err)
	}
}

// track registers ss, refusing it once the server has started shutting down.
func (s *Server) track(ss *session) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing {
		return false
	}

	s.sessions[ss] = struct{}{}

	return true
}

func (s *Server) untrack(ss *session) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.sessions, ss)
}

func (ss *session) serve(ctx context.Context, mux *yamux.Session) error {
	acceptCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	stream, err := mux.AcceptStreamWithContext(acceptCtx)

	cancel()

	if err != nil {
		return fmt.Errorf("accept control stream: %w", err)
	}

	ctrl := protocol.NewControl(stream)

	if err := stream.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return fmt.Errorf("set handshake deadline: %w", err)
	}

	if err := ss.handshake(ctrl); err != nil {
		return err
	}

	if err := stream.SetDeadline(time.Time{}); err != nil {
		return fmt.Errorf("clear handshake deadline: %w", err)
	}

	ss.logger.Info("client connected")

	for {
		m, err := ctrl.Recv()
		if err != nil {
			return fmt.Errorf("control stream: %w", err)
		}

		if err := ctrl.Send(ss.handle(ctx, m)); err != nil {
			return fmt.Errorf("control stream: %w", err)
		}
	}
}

func (ss *session) handshake(ctrl *protocol.Control) error {
	m, err := ctrl.Recv()
	if err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	var reject string

	switch {
	case m.Kind != protocol.KindHello || m.Hello == nil:
		reject = "expected hello"
	case m.Hello.Version != protocol.Version:
		reject = "unsupported protocol version " + strconv.Itoa(m.Hello.Version) + ", please update parrot"
	default:
		reject = ss.authenticate(m.Hello.Token)
	}

	if reject != "" {
		if err := ctrl.Send(&protocol.Message{Kind: protocol.KindError, Error: reject}); err != nil {
			ss.logger.Debug("send rejection failed", "error", err)
		}

		return &rejectError{reason: reject}
	}

	welcome := &protocol.Welcome{Features: []string{protocol.FeatureUDP}}
	if err := ctrl.Send(&protocol.Message{Kind: protocol.KindWelcome, Welcome: welcome}); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	return nil
}

// authenticate checks token and returns a rejection message, or "" when the client may proceed.
func (ss *session) authenticate(token string) string {
	label, anonymous, reject := ss.srv.authorize(token)
	if reject != "" {
		return reject
	}

	ss.logger = ss.logger.With("token", label)
	ss.anonymous = anonymous

	return ""
}

// authorize checks token for any kind of client, returning the label to log it under or a rejection message.
func (s *Server) authorize(token string) (label string, anonymous bool, reject string) {
	auth := s.opts.Auth

	if auth == nil || (token == "" && s.opts.AllowAnonymous) {
		return "anonymous", true, ""
	}

	if token == "" {
		return "", false, "missing token"
	}

	label, err := auth.Authenticate(token)
	if err != nil {
		s.logger.Debug("authentication failed", "error", err)

		return "", false, "invalid token"
	}

	return label, false, ""
}

func (ss *session) handle(ctx context.Context, m *protocol.Message) *protocol.Message {
	switch {
	case m.Kind == protocol.KindBind && m.Bind != nil:
		return ss.handleBind(ctx, m.Bind)
	case m.Kind == protocol.KindSpeedTest && m.Speed != nil:
		return ss.speedTest(m.Speed.Direction)
	}

	return &protocol.Message{Kind: protocol.KindError, Error: protocol.ErrUnexpectedMessage.Error()}
}

func (ss *session) handleBind(ctx context.Context, b *protocol.Bind) *protocol.Message {
	bound, err := ss.bind(ctx, b)
	if err != nil {
		return &protocol.Message{Kind: protocol.KindError, Error: err.Error()}
	}

	ss.logger.Info("tunnel online", "tunnel", bound.Name, "url", bound.URL)

	return &protocol.Message{Kind: protocol.KindBound, Bound: bound}
}

func (ss *session) bind(ctx context.Context, b *protocol.Bind) (*protocol.Bound, error) {
	if b.Name == "" {
		return nil, errors.New("missing tunnel name")
	}

	if ss.names[b.Name] {
		return nil, fmt.Errorf("tunnel name %q is already in use", b.Name)
	}

	var (
		bound *protocol.Bound
		err   error
	)

	switch b.Proto {
	case tunnel.HTTP:
		bound, err = ss.bindHTTP(b)
	case tunnel.TCP:
		bound, err = ss.bindTCP(ctx, b)
	case tunnel.UDP:
		bound, err = ss.bindUDP(ctx, b)
	case tunnel.ProtoInvalid:
		err = errors.New("missing protocol")
	}

	if err != nil {
		return nil, err
	}

	ss.names[b.Name] = true

	return bound, nil
}

func (ss *session) bindHTTP(b *protocol.Bind) (*protocol.Bound, error) {
	var wn *warning
	if ss.anonymous {
		wn = ss.srv.warning
	}

	t := newHTTPTunnel(func() (net.Conn, error) { return ss.client.open(b.Name, "") }, b, ss.logger, wn)

	sub, err := ss.srv.registerHost(b.Subdomain, t)
	if err != nil {
		return nil, err
	}

	ss.subdomains = append(ss.subdomains, sub)
	ss.tunnels = append(ss.tunnels, t)

	return &protocol.Bound{
		Name:      b.Name,
		URL:       ss.srv.scheme + "://" + sub + "." + ss.srv.domain + ss.srv.publicPort,
		Subdomain: sub,
	}, nil
}

func (ss *session) bindTCP(ctx context.Context, b *protocol.Bind) (*protocol.Bound, error) {
	ln, port, err := ss.srv.listenTCP(ctx, b.RemotePort)
	if err != nil {
		return nil, err
	}

	ss.ports = append(ss.ports, port)
	ss.listeners = append(ss.listeners, ln)

	ss.wg.Go(func() {
		ss.acceptTCP(ln, b.Name)
	})

	return &protocol.Bound{
		Name:       b.Name,
		URL:        "tcp://" + net.JoinHostPort(ss.srv.domain, strconv.Itoa(port)),
		RemotePort: port,
	}, nil
}

func (ss *session) bindUDP(ctx context.Context, b *protocol.Bind) (*protocol.Bound, error) {
	pc, port, err := ss.srv.listenUDP(ctx, b.RemotePort)
	if err != nil {
		return nil, err
	}

	ss.udpPorts = append(ss.udpPorts, port)
	ss.packets = append(ss.packets, pc)

	open := func(sender string) (net.Conn, error) { return ss.client.open(b.Name, sender) }
	ss.wg.Go(newUDPRelay(pc, open, ss.logger.With("tunnel", b.Name)).run)

	return &protocol.Bound{
		Name:       b.Name,
		URL:        "udp://" + net.JoinHostPort(ss.srv.domain, strconv.Itoa(port)),
		RemotePort: port,
	}, nil
}

func (ss *session) acceptTCP(ln net.Listener, name string) {
	for {
		conn, err := accept(ln, ss.client.done(), ss.logger)
		if err != nil {
			return
		}

		ss.wg.Go(func() {
			ss.forwardTCP(conn, name)
		})
	}
}

func (ss *session) forwardTCP(conn net.Conn, name string) {
	stream, err := ss.client.open(name, conn.RemoteAddr().String())
	if err != nil {
		ss.logger.Debug("open tcp stream failed", "tunnel", name, "error", err)
		ss.closeConn(conn)

		return
	}

	if err := relay.Join(conn, stream); err != nil {
		ss.logger.Debug("tcp relay ended", "tunnel", name, "error", err)
	}
}

// close releases everything the session holds and waits for its connections to finish.
func (ss *session) close() {
	ss.srv.release(ss.subdomains, ss.ports, ss.udpPorts)
	ss.disconnect()

	for _, ln := range ss.listeners {
		if err := ln.Close(); err != nil {
			ss.logger.Debug("close tcp listener failed", "error", err)
		}
	}

	for _, pc := range ss.packets {
		if err := pc.Close(); err != nil {
			ss.logger.Debug("close udp socket failed", "error", err)
		}
	}

	for _, t := range ss.tunnels {
		t.close()
	}

	ss.wg.Wait()
}

func (ss *session) disconnect() {
	if err := ss.client.close(); err != nil {
		ss.logger.Debug("disconnect failed", "error", err)
	}
}

func (ss *session) closeConn(conn net.Conn) {
	if err := conn.Close(); err != nil {
		ss.logger.Debug("close connection failed", "error", err)
	}
}

func openStream(mux *yamux.Session, h protocol.StreamHeader) (net.Conn, error) {
	stream, err := mux.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("open stream: %w", err)
	}

	if err := protocol.WriteStreamHeader(stream, h); err != nil {
		return nil, errors.Join(err, stream.Close())
	}

	return stream, nil
}
