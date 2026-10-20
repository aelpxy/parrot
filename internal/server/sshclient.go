package server

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshForward is a remote forward as the SSH client registered it; channels must echo it back
// so the client can match them to its local target.
type sshForward struct {
	addr string
	port uint32
}

// sshClient reaches an `ssh -R` client by opening forwarded-tcpip channels.
type sshClient struct {
	conn   *ssh.ServerConn
	closed chan struct{}
	err    error

	mu       sync.Mutex
	forwards map[string]sshForward
}

// newSSHClient watches conn until it shuts down, after which done is closed and err is set.
func newSSHClient(conn *ssh.ServerConn) *sshClient {
	c := &sshClient{conn: conn, closed: make(chan struct{}), forwards: make(map[string]sshForward)}

	go func() {
		c.err = conn.Wait()
		close(c.closed)
	}()

	return c
}

func (c *sshClient) addForward(tunnel string, f sshForward) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.forwards[tunnel] = f
}

// hasForward reports whether a forward for f's address and port is already registered.
func (c *sshClient) hasForward(f sshForward) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, existing := range c.forwards {
		if existing == f {
			return true
		}
	}

	return false
}

func (c *sshClient) forwardCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.forwards)
}

func (c *sshClient) open(tunnel, remoteAddr string) (net.Conn, error) {
	c.mu.Lock()
	f, ok := c.forwards[tunnel]
	c.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("unknown tunnel %q", tunnel)
	}

	// HTTP connections are pooled across visitors, so they originate from the server itself.
	if remoteAddr == "" {
		remoteAddr = c.conn.LocalAddr().String()
	}

	originHost, originPort := splitOrigin(remoteAddr)

	payload := ssh.Marshal(struct {
		Addr       string
		Port       uint32
		OriginAddr string
		OriginPort uint32
	}{f.addr, f.port, originHost, originPort})

	ch, reqs, err := c.conn.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		return nil, fmt.Errorf("open ssh channel: %w", err)
	}

	// Forwarded channels carry no requests we act on; the goroutine ends with the channel.
	go ssh.DiscardRequests(reqs)

	return &channelConn{Channel: ch, local: c.conn.LocalAddr(), remote: c.conn.RemoteAddr()}, nil
}

func (c *sshClient) done() <-chan struct{} { return c.closed }

func (c *sshClient) close() error {
	if err := c.conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("close ssh connection: %w", err)
	}

	return nil
}

// splitOrigin returns the host and port of the address a forwarded connection came from.
func splitOrigin(remoteAddr string) (string, uint32) {
	host, port, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr, 0
	}

	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return host, 0
	}

	return host, uint32(n)
}

// channelConn lets an SSH channel stand in for a net.Conn. SSH channels have no deadlines, so
// those calls succeed without effect; the SSH connection's keepalives catch dead peers instead.
type channelConn struct {
	ssh.Channel

	local  net.Addr
	remote net.Addr
}

func (c *channelConn) LocalAddr() net.Addr              { return c.local }
func (c *channelConn) RemoteAddr() net.Addr             { return c.remote }
func (c *channelConn) SetDeadline(time.Time) error      { return nil }
func (c *channelConn) SetReadDeadline(time.Time) error  { return nil }
func (c *channelConn) SetWriteDeadline(time.Time) error { return nil }
