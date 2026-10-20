package server

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	sshPrefix    = "SSH-"
	sniffTimeout = 5 * time.Second
)

// splitter shares one listener between SSH and TLS: SSH clients speak first with "SSH-", and
// anything else is handed on as TLS.
type splitter struct {
	base   net.Listener
	logger *slog.Logger
	tls    chan net.Conn
	ssh    chan net.Conn
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

func newSplitter(base net.Listener, logger *slog.Logger) *splitter {
	return &splitter{
		base:   base,
		logger: logger,
		tls:    make(chan net.Conn),
		ssh:    make(chan net.Conn),
		done:   make(chan struct{}),
	}
}

// listeners returns the TLS and SSH halves. Each closes on its own, so the HTTPS server can still
// drain after SSH stops; closing the TLS half also closes the shared listener.
func (sp *splitter) listeners() (net.Listener, net.Listener) {
	tlsHalf := &splitListener{sp: sp, conns: sp.tls, closed: make(chan struct{}), ownsBase: true}
	sshHalf := &splitListener{sp: sp, conns: sp.ssh, closed: make(chan struct{})}

	return tlsHalf, sshHalf
}

// run accepts and routes connections until the listener is closed.
func (sp *splitter) run() {
	defer sp.wg.Wait()

	for {
		conn, err := accept(sp.base, sp.done, sp.logger)
		if err != nil {
			sp.close()

			return
		}

		sp.wg.Go(func() {
			sp.route(conn)
		})
	}
}

func (sp *splitter) route(conn net.Conn) {
	r := bufio.NewReader(conn)

	prefix, err := peek(conn, r, len(sshPrefix))
	if err != nil {
		sp.logger.Debug("sniff connection failed", "client", conn.RemoteAddr().String(), "error", err)
		closeQuietly(conn)

		return
	}

	half := sp.tls
	if string(prefix) == sshPrefix {
		half = sp.ssh
	}

	select {
	case half <- &peekedConn{Conn: conn, r: r}:
	case <-sp.done:
		closeQuietly(conn)
	case <-time.After(sniffTimeout):
		// Nobody is accepting this kind of connection any more, such as SSH during shutdown.
		closeQuietly(conn)
	}
}

// peek reads the first n bytes without consuming them, giving up on clients that stay silent.
func peek(conn net.Conn, r *bufio.Reader, n int) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(sniffTimeout)); err != nil {
		return nil, fmt.Errorf("set sniff deadline: %w", err)
	}

	prefix, err := r.Peek(n)
	if err != nil {
		return nil, fmt.Errorf("read first bytes: %w", err)
	}

	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear sniff deadline: %w", err)
	}

	return prefix, nil
}

func (sp *splitter) close() {
	sp.once.Do(func() {
		close(sp.done)
		closeQuietly(sp.base)
	})
}

type splitListener struct {
	sp       *splitter
	conns    chan net.Conn
	closed   chan struct{}
	once     sync.Once
	ownsBase bool
}

func (l *splitListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-l.sp.done:
		return nil, net.ErrClosed
	}
}

func (l *splitListener) Close() error {
	l.once.Do(func() { close(l.closed) })

	if l.ownsBase {
		l.sp.close()
	}

	return nil
}

func (l *splitListener) Addr() net.Addr { return l.sp.base.Addr() }

// peekedConn replays the sniffed bytes before reading on from the connection.
type peekedConn struct {
	net.Conn

	r *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) {
	return c.r.Read(p) //nolint:wrapcheck // io.Reader callers compare against io.EOF, so it must pass through unwrapped.
}

// closeQuietly closes c where nothing useful can be done with a failure, such as rejecting a connection.
func closeQuietly(c interface{ Close() error }) {
	_ = c.Close()
}
