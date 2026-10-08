package server

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"

	"github.com/aelpxy/parrot/internal/tunnel"
)

// PortRange is an inclusive range of ports.
type PortRange struct {
	First int
	Last  int
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

func listenPort(ctx context.Context, port int) (net.Listener, error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("", strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("listen tcp %d: %w", port, err)
	}

	return ln, nil
}
