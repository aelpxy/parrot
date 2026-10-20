package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/qrterm"
	"github.com/aelpxy/parrot/internal/token"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const (
	sshServerVersion = "SSH-2.0-parrot"
	sshKeepalive     = 30 * time.Second
	maxSSHForwards   = 8
	noForwardHint    = 2 * time.Second
	defaultSSHPort   = "22"

	// Permissions extensions carrying what the username asked for from auth to the session.
	extLabel     = "parrot-label"
	extAnonymous = "parrot-anonymous"
	extQR        = "parrot-qr"
	extTCP       = "parrot-tcp"
)

// sshUser is what an SSH username asks for: "prt_…+qr@", "tcp@", or anything else, which is ignored
// because ssh sends the local username when none is given.
type sshUser struct {
	token string
	qr    bool
	tcp   bool
}

func parseSSHUser(user string) sshUser {
	var u sshUser

	for part := range strings.SplitSeq(user, "+") {
		switch {
		case part == "qr":
			u.qr = true
		case part == "tcp":
			u.tcp = true
		case strings.HasPrefix(part, token.Prefix):
			u.token = part
		}
	}

	return u
}

func (s *Server) sshConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		ServerVersion: sshServerVersion,
		NoClientAuth:  true,
		NoClientAuthCallback: func(m ssh.ConnMetadata) (*ssh.Permissions, error) {
			u := parseSSHUser(m.User())

			label, anonymous, reject := s.authorize(u.token)
			if reject != "" {
				return nil, errors.New(reject)
			}

			ext := map[string]string{extLabel: label}
			setFlag(ext, extAnonymous, anonymous)
			setFlag(ext, extQR, u.qr)
			setFlag(ext, extTCP, u.tcp)

			return &ssh.Permissions{Extensions: ext}, nil
		},
		// ssh shows the banner before auth fails, which is the only way to tell the user why.
		BannerCallback: func(m ssh.ConnMetadata) string {
			if _, _, reject := s.authorize(parseSSHUser(m.User()).token); reject != "" {
				return "parrot: " + reject + crlf
			}

			return ""
		},
	}

	cfg.AddHostKey(s.opts.SSHHostKey)

	return cfg
}

func setFlag(ext map[string]string, key string, on bool) {
	if on {
		ext[key] = "1"
	}
}

func (s *Server) acceptSSH(ctx context.Context, ln net.Listener, cfg *ssh.ServerConfig) error {
	var wg sync.WaitGroup

	defer func() {
		s.closeSessions()
		wg.Wait()
	}()

	for {
		conn, err := accept(ln, ctx.Done(), s.logger)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}

			return fmt.Errorf("accept ssh: %w", err)
		}

		wg.Go(func() {
			s.serveSSH(ctx, conn, cfg)
		})
	}
}

func (s *Server) serveSSH(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	logger := s.logger.With("client", nc.RemoteAddr().String(), "transport", "ssh")

	conn, chans, reqs, err := sshHandshake(nc, cfg)
	if err != nil {
		logger.Debug("ssh handshake failed", "error", err)
		closeQuietly(nc)

		return
	}

	ext := conn.Permissions.Extensions
	sc := newSSHClient(conn)
	ss := &session{
		srv:       s,
		client:    sc,
		logger:    logger.With("token", ext[extLabel]),
		names:     make(map[string]bool),
		anonymous: ext[extAnonymous] != "",
	}

	if !s.track(ss) {
		ss.disconnect()

		return
	}

	ss.logger.Info("client connected")

	term := &sshTerminal{header: []string{"", "  parrot · " + s.domain, ""}}
	hint := sshCommand(s.domain, portOf(nc.LocalAddr()), "")
	opts := forwardOptions{qr: ext[extQR] != "", tcp: ext[extTCP] != ""}

	var wg sync.WaitGroup

	wg.Go(func() { ss.serveForwards(ctx, reqs, sc, term, opts) })
	wg.Go(func() { ss.serveSSHChannels(chans, sc, term, hint) })
	wg.Go(func() { keepalive(conn, sc.done()) })

	<-sc.done()

	s.untrack(ss)
	ss.close()
	wg.Wait()
	ss.logEnd(endReason(sc.err))
}

// endReason treats the client closing its ssh normally as a plain disconnect. The SSH library's
// disconnect error type is unexported, so it can only be recognized by its message.
func endReason(err error) error {
	if err != nil && strings.HasPrefix(err.Error(), "ssh: disconnect") {
		return io.EOF
	}

	return err
}

// sshCommand is the command that opens an HTTP tunnel through port as user, for hints and the landing page.
func sshCommand(domain, port, user string) string {
	cmd := "ssh "
	if port != "" && port != defaultSSHPort {
		cmd += "-p " + port + " "
	}

	if user != "" {
		domain = user + "@" + domain
	}

	return cmd + "-R0:localhost:3000 " + domain
}

// portOf returns addr's port, or "" when it has none.
func portOf(addr net.Addr) string {
	_, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}

	return port
}

func sshHandshake(nc net.Conn, cfg *ssh.ServerConfig) (*ssh.ServerConn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	if err := nc.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return nil, nil, nil, fmt.Errorf("set handshake deadline: %w", err)
	}

	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ssh handshake: %w", err)
	}

	if err := nc.SetDeadline(time.Time{}); err != nil {
		return nil, nil, nil, errors.Join(fmt.Errorf("clear handshake deadline: %w", err), conn.Close())
	}

	return conn, chans, reqs, nil
}

// serveSSHChannels shows output on the client's session channel and refuses everything else.
func (ss *session) serveSSHChannels(chans <-chan ssh.NewChannel, sc *sshClient, term *sshTerminal, hint string) {
	var wg sync.WaitGroup
	defer wg.Wait()

	for nc := range chans {
		if nc.ChannelType() != "session" || term.attached() {
			_ = nc.Reject(ssh.Prohibited, "parrot only accepts remote forwards (-R) and one session")

			continue
		}

		ch, reqs, err := nc.Accept()
		if err != nil {
			continue
		}

		wg.Go(func() { answerSessionRequests(reqs) })
		wg.Go(func() { watchInput(ch, ss.disconnect) })

		term.attach(ch)
		hintIfNoForwards(sc, term, hint)
	}
}

// hintIfNoForwards explains how to ask for a tunnel when ssh connected without -R.
func hintIfNoForwards(sc *sshClient, term *sshTerminal, command string) {
	timer := time.NewTimer(noForwardHint)
	defer timer.Stop()

	select {
	case <-sc.done():
	case <-timer.C:
		if sc.forwardCount() == 0 {
			term.println("  no tunnels requested. try: "+command, "")
		}
	}
}

func keepalive(conn ssh.Conn, done <-chan struct{}) {
	t := time.NewTicker(sshKeepalive)
	defer t.Stop()

	for {
		select {
		case <-done:
			return
		case <-t.C:
		}

		if _, _, err := conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
			closeQuietly(conn)

			return
		}
	}
}

type forwardOptions struct {
	qr  bool
	tcp bool
}

// serveForwards turns each `ssh -R` into a tunnel and refuses other global requests.
func (ss *session) serveForwards(ctx context.Context, reqs <-chan *ssh.Request, sc *sshClient, term *sshTerminal, opts forwardOptions) {
	for req := range reqs {
		if req.Type != "tcpip-forward" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}

			continue
		}

		url, reply, err := ss.forward(ctx, req.Payload, sc, opts)
		if err != nil {
			ss.logger.Info("ssh forward refused", "error", err)
			term.println("  ✗ "+err.Error(), "")
		}

		// A failed reply means the connection is gone, which ends the loop anyway.
		if req.WantReply {
			_ = req.Reply(err == nil, reply)
		}

		if err == nil {
			ss.showTunnel(term, url, opts.qr)
		}
	}
}

// forward binds the tunnel one remote forward asks for, returning its URL and the reply payload.
func (ss *session) forward(ctx context.Context, payload []byte, sc *sshClient, opts forwardOptions) (string, []byte, error) {
	var req struct {
		Addr string
		Port uint32
	}

	if err := ssh.Unmarshal(payload, &req); err != nil {
		return "", nil, fmt.Errorf("bad forward request: %w", err)
	}

	if sc.forwardCount() >= maxSSHForwards {
		return "", nil, fmt.Errorf("at most %d tunnels per connection", maxSSHForwards)
	}

	b := ss.bindFor(req.Addr, req.Port, opts.tcp)

	// For HTTP the port only identifies the forward to the client, so echo a stable one back.
	f := sshForward{addr: req.Addr, port: req.Port}
	if b.Proto == tunnel.HTTP && f.port == 0 {
		f.port = ss.srv.publicPortNumber()
	}

	if b.Proto == tunnel.HTTP && sc.hasForward(f) {
		return "", nil, fmt.Errorf("already forwarding %s; name each tunnel, e.g. -R myapp:80:localhost:3000", net.JoinHostPort(f.addr, strconv.Itoa(int(f.port))))
	}

	bound, err := ss.bind(ctx, b)
	if err != nil {
		return "", nil, err
	}

	if b.Proto == tunnel.TCP {
		f.port = uint32(bound.RemotePort)
	}

	sc.addForward(b.Name, f)
	ss.logger.Info("tunnel online", "tunnel", b.Name, "url", bound.URL)

	// Only a request for port 0 expects to hear which port it got.
	if req.Port != 0 {
		return bound.URL, nil, nil
	}

	return bound.URL, ssh.Marshal(struct{ Port uint32 }{f.port}), nil
}

// bindFor reads a remote forward like an HTTP tunnel unless the user asked for TCP or a specific
// non-web port. A bind address such as "myapp" in -R myapp:80:localhost:3000 requests that subdomain.
func (ss *session) bindFor(addr string, port uint32, tcp bool) *protocol.Bind {
	b := &protocol.Bind{Name: "ssh-" + strconv.Itoa(len(ss.names)+1)}

	if tcp || (port != 0 && port != 80 && port != 443) {
		b.Proto = tunnel.TCP
		b.RemotePort = int(port)

		return b
	}

	b.Proto = tunnel.HTTP

	if tunnel.ValidSubdomain(addr) && addr != "localhost" {
		b.Subdomain = addr
	}

	return b
}

// showTunnel prints a tunnel's URL, with a QR code when the user asked for one.
func (ss *session) showTunnel(term *sshTerminal, url string, qr bool) {
	lines := []string{"  " + url}

	if qr {
		code, err := qrterm.Lines(url)
		if err != nil {
			ss.logger.Debug("render qr code failed", "error", err)
		}

		for _, l := range code {
			lines = append(lines, "  "+l)
		}
	}

	term.tunnel(lines...)
}
