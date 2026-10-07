// Package protocol defines the wire format between parrot clients and servers.
//
// A client opens one multiplexed session per server. The first stream it opens is the
// control stream, carrying JSON messages. Every other stream is opened by the server for
// a single public connection and starts with a binary StreamHeader, followed by raw bytes.
package protocol

import (
	"encoding/binary"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/tunnel"
)

// Version is the protocol version spoken by this build.
const Version = 1

// Kind identifies a control message.
type Kind int

// Control message kinds.
const (
	KindInvalid Kind = iota
	KindHello
	KindWelcome
	KindBind
	KindBound
	KindError
)

// ErrUnexpectedMessage is returned when the peer sends a message out of order.
var ErrUnexpectedMessage = errors.New("unexpected message")

// Message is a single control message; exactly one payload matching Kind is set.
type Message struct {
	Hello *Hello `json:"hello,omitzero"`
	Bind  *Bind  `json:"bind,omitzero"`
	Bound *Bound `json:"bound,omitzero"`
	Error string `json:"error,omitzero"`
	Kind  Kind   `json:"kind"`
}

// Hello is the first message a client sends.
type Hello struct {
	Token   string `json:"token,omitzero"`
	Version int    `json:"version"`
}

// Bind asks the server to expose a tunnel.
type Bind struct {
	Name       string       `json:"name"`
	Subdomain  string       `json:"subdomain,omitzero"`
	Auth       string       `json:"auth,omitzero"`
	HostHeader string       `json:"host_header,omitzero"`
	Proto      tunnel.Proto `json:"proto"`
	RemotePort int          `json:"remote_port,omitzero"`
}

// Bound confirms a tunnel; clients re-request Subdomain and RemotePort to keep URLs across reconnects.
type Bound struct {
	Name       string `json:"name"`
	URL        string `json:"url"`
	Subdomain  string `json:"subdomain,omitzero"`
	RemotePort int    `json:"remote_port,omitzero"`
}

// RemoteError is an error reported by the peer.
type RemoteError struct {
	Message string
}

func (e *RemoteError) Error() string { return e.Message }

// Control sends and receives messages on the control stream.
type Control struct {
	enc *jsontext.Encoder
	dec *jsontext.Decoder
}

// NewControl returns a Control reading from and writing to rw.
func NewControl(rw io.ReadWriter) *Control {
	return &Control{enc: jsontext.NewEncoder(rw), dec: jsontext.NewDecoder(rw)}
}

// Send writes m.
func (c *Control) Send(m *Message) error {
	if err := json.MarshalEncode(c.enc, m); err != nil {
		return fmt.Errorf("send message: %w", err)
	}

	return nil
}

// Recv reads the next message.
func (c *Control) Recv() (*Message, error) {
	var m Message
	if err := json.UnmarshalDecode(c.dec, &m); err != nil {
		return nil, fmt.Errorf("receive message: %w", err)
	}

	return &m, nil
}

// Request sends m and waits for a reply of kind want, turning error replies into RemoteError.
func (c *Control) Request(m *Message, want Kind) (*Message, error) {
	if err := c.Send(m); err != nil {
		return nil, err
	}

	reply, err := c.Recv()
	if err != nil {
		return nil, err
	}

	switch reply.Kind {
	case want:
		return reply, nil
	case KindError:
		return nil, &RemoteError{Message: reply.Error}
	case KindInvalid, KindHello, KindWelcome, KindBind, KindBound:
	}

	return nil, fmt.Errorf("%w: kind %d", ErrUnexpectedMessage, reply.Kind)
}

// StreamHeader opens every data stream and names the tunnel it belongs to.
type StreamHeader struct {
	Tunnel     string
	RemoteAddr string
}

// WriteStreamHeader writes h as a version byte followed by length-prefixed strings.
func WriteStreamHeader(w io.Writer, h StreamHeader) error {
	if len(h.Tunnel) > math.MaxUint16 || len(h.RemoteAddr) > math.MaxUint16 {
		return errors.New("stream header field too long")
	}

	buf := make([]byte, 0, 1+2+len(h.Tunnel)+2+len(h.RemoteAddr))
	buf = append(buf, Version)
	buf = appendString(buf, h.Tunnel)
	buf = appendString(buf, h.RemoteAddr)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("write stream header: %w", err)
	}

	return nil
}

// ReadStreamHeader reads a header written by WriteStreamHeader.
func ReadStreamHeader(r io.Reader) (StreamHeader, error) {
	var version [1]byte
	if _, err := io.ReadFull(r, version[:]); err != nil {
		return StreamHeader{}, fmt.Errorf("read stream header: %w", err)
	}

	if version[0] != Version {
		return StreamHeader{}, fmt.Errorf("unsupported stream header version %d", version[0])
	}

	name, err := readString(r)
	if err != nil {
		return StreamHeader{}, err
	}

	addr, err := readString(r)
	if err != nil {
		return StreamHeader{}, err
	}

	return StreamHeader{Tunnel: name, RemoteAddr: addr}, nil
}

// MuxConfig returns the multiplexer settings both sides must use.
func MuxConfig(logger *slog.Logger) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = nil
	cfg.Logger = slog.NewLogLogger(logger.Handler(), slog.LevelDebug)

	return cfg
}

func appendString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))

	return append(b, s...)
}

func readString(r io.Reader) (string, error) {
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return "", fmt.Errorf("read stream header: %w", err)
	}

	buf := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read stream header: %w", err)
	}

	return string(buf), nil
}
