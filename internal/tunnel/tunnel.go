// Package tunnel describes tunnels independently of how they are configured or run.
package tunnel

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Proto is the protocol a tunnel forwards.
type Proto int

// Supported protocols.
const (
	ProtoInvalid Proto = iota
	HTTP
	TCP
)

const (
	defaultHost   = "localhost"
	maxPort       = 65535
	maxLabelBytes = 63
)

// ErrInvalidTarget is returned when a local target cannot be parsed.
var ErrInvalidTarget = errors.New("invalid target")

// Spec describes a single tunnel.
type Spec struct {
	Name       string `json:"name"`
	Target     string `json:"target"`
	Subdomain  string `json:"subdomain,omitzero"`
	Auth       string `json:"auth,omitzero"`
	HostHeader string `json:"host_header,omitzero"`
	Proto      Proto  `json:"proto"`
	RemotePort int    `json:"remote_port,omitzero"`
	Autostart  bool   `json:"autostart,omitzero"`
}

// ParseProto parses "http" or "tcp".
func ParseProto(s string) (Proto, error) {
	switch s {
	case "http":
		return HTTP, nil
	case "tcp":
		return TCP, nil
	}

	return ProtoInvalid, fmt.Errorf("unknown protocol %q, expected http or tcp", s)
}

// MarshalText implements encoding.TextMarshaler.
func (p Proto) MarshalText() ([]byte, error) {
	if p == ProtoInvalid {
		return nil, errors.New("marshal invalid protocol")
	}

	return []byte(p.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (p *Proto) UnmarshalText(b []byte) error {
	parsed, err := ParseProto(string(b))
	if err != nil {
		return err
	}

	*p = parsed

	return nil
}

func (p Proto) String() string {
	switch p {
	case HTTP:
		return "http"
	case TCP:
		return "tcp"
	case ProtoInvalid:
	}

	return "invalid"
}

// ParseTarget normalizes "3000", ":3000", or "host:3000" into "host:port".
func ParseTarget(s string) (string, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = "", s
	}

	if host == "" {
		host = defaultHost
	}

	if err := checkPort(port); err != nil {
		return "", fmt.Errorf("%w %q: %w", ErrInvalidTarget, s, err)
	}

	return net.JoinHostPort(host, port), nil
}

// Normalize resolves the target to host:port and reports every problem with the spec at once.
func (s *Spec) Normalize() error {
	var errs []error

	target, err := ParseTarget(s.Target)
	if err != nil {
		errs = append(errs, err)
	} else {
		s.Target = target
	}

	switch s.Proto {
	case HTTP:
		errs = append(errs, s.validateHTTP()...)
	case TCP:
		errs = append(errs, s.validateTCP()...)
	case ProtoInvalid:
		errs = append(errs, errors.New("missing protocol"))
	}

	return errors.Join(errs...)
}

func (s *Spec) validateHTTP() []error {
	var errs []error

	if s.RemotePort != 0 {
		errs = append(errs, errors.New("remote port is only supported for tcp tunnels"))
	}

	if s.Subdomain != "" && !ValidSubdomain(s.Subdomain) {
		errs = append(errs, fmt.Errorf("invalid subdomain %q: use lowercase letters, digits, and hyphens", s.Subdomain))
	}

	if user, pass, ok := strings.Cut(s.Auth, ":"); s.Auth != "" && (!ok || user == "" || pass == "") {
		errs = append(errs, errors.New("auth must be in user:pass form"))
	}

	return errs
}

func (s *Spec) validateTCP() []error {
	var errs []error

	if s.Subdomain != "" {
		errs = append(errs, errors.New("subdomain is only supported for http tunnels"))
	}

	if s.Auth != "" {
		errs = append(errs, errors.New("auth is only supported for http tunnels"))
	}

	if s.HostHeader != "" {
		errs = append(errs, errors.New("host header is only supported for http tunnels"))
	}

	if s.RemotePort != 0 {
		if err := checkPort(strconv.Itoa(s.RemotePort)); err != nil {
			errs = append(errs, fmt.Errorf("remote port: %w", err))
		}
	}

	return errs
}

func checkPort(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxPort {
		return errors.New("port must be a number from 1 to 65535")
	}

	return nil
}

// ValidSubdomain reports whether s is a single lowercase DNS label.
func ValidSubdomain(s string) bool {
	if s == "" || len(s) > maxLabelBytes || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}

	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}

	return true
}
