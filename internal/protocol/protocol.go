// Package protocol defines the wire format between parrot clients and servers.
//
// A client opens one multiplexed session per server. The first stream it opens is the
// control stream, carrying JSON messages. Every other stream is opened by the server for
// a single public connection, or for a speed test the client asked for, and starts with a
// binary StreamHeader, followed by raw bytes. A UDP tunnel's stream carries one remote sender's
// packets, each framed by WriteDatagram.
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
	"slices"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/tunnel"
)

// Version is the protocol version spoken by this build.
const Version = 1

// maxStreamWindow is how much a stream may have in flight. A stream moves at most one window per
// round trip, so yamux's 256 KiB default capped tunnels near 5 Mbit/s at 400 ms; 4 MiB measured
// 42 Mbit/s down and 77 Mbit/s up there. A receiver that reads slowly buffers up to this per stream.
const maxStreamWindow = 4 << 20

// maxMessageSize bounds one control message so a peer cannot make us buffer without limit.
const maxMessageSize = 64 << 10

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
	KindSpeedTest
	KindSpeedResult
)

// SpeedDirection is which way a speed test moves data, seen from the client.
type SpeedDirection int

// Speed test directions.
const (
	SpeedInvalid SpeedDirection = iota
	SpeedDownload
	SpeedUpload
)

func (d SpeedDirection) String() string {
	switch d {
	case SpeedDownload:
		return "download"
	case SpeedUpload:
		return "upload"
	case SpeedInvalid:
	}

	return "invalid"
}

var (
	// ErrUnexpectedMessage is returned when the peer sends a message out of order.
	ErrUnexpectedMessage = errors.New("unexpected message")

	// ErrMessageTooLarge is returned when a control message exceeds the size limit.
	ErrMessageTooLarge = errors.New("control message too large")
)

// Message is a single control message; exactly one payload matching Kind is set.
type Message struct {
	Hello   *Hello   `json:"hello,omitzero"`
	Bind    *Bind    `json:"bind,omitzero"`
	Bound   *Bound   `json:"bound,omitzero"`
	Speed   *Speed   `json:"speed,omitzero"`
	Welcome *Welcome `json:"welcome,omitzero"`
	Error   string   `json:"error,omitzero"`
	Kind    Kind     `json:"kind"`
}

// Hello is the first message a client sends.
type Hello struct {
	Token   string `json:"token,omitzero"`
	Version int    `json:"version"`
}

// FeatureUDP is advertised by servers that accept UDP tunnels.
const FeatureUDP = "udp"

// Welcome accepts a client and lists optional features the server supports. Servers from before
// features existed send no Welcome payload at all.
type Welcome struct {
	Features []string `json:"features,omitzero"`
}

// Supports reports whether w lists feature; a nil Welcome supports nothing optional.
func (w *Welcome) Supports(feature string) bool {
	return w != nil && slices.Contains(w.Features, feature)
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

// Speed asks for a speed test and carries its result: bytes the receiver got over ElapsedNS nanoseconds.
type Speed struct {
	Direction SpeedDirection `json:"direction"`
	Bytes     int64          `json:"bytes,omitzero"`
	ElapsedNS int64          `json:"elapsed_ns,omitzero"`
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
	lim *io.LimitedReader
}

// NewControl returns a Control reading from and writing to rw.
func NewControl(rw io.ReadWriter) *Control {
	lim := &io.LimitedReader{R: rw}

	return &Control{enc: jsontext.NewEncoder(rw), dec: jsontext.NewDecoder(lim), lim: lim}
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
	c.lim.N = maxMessageSize

	var m Message
	if err := json.UnmarshalDecode(c.dec, &m); err != nil {
		if c.lim.N <= 0 {
			return nil, ErrMessageTooLarge
		}

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
	case KindInvalid, KindHello, KindWelcome, KindBind, KindBound, KindSpeedTest, KindSpeedResult:
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
	cfg.MaxStreamWindowSize = maxStreamWindow
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

// MaxDatagram is the largest UDP payload a tunnel carries, the most a 2-byte length can describe.
const MaxDatagram = math.MaxUint16

// datagramHeader is the size of each datagram's big-endian length prefix.
const datagramHeader = 2

// WriteDatagram writes p as one length-prefixed frame, in a single write so frames never interleave.
func WriteDatagram(w io.Writer, p []byte) error {
	if len(p) > MaxDatagram {
		return fmt.Errorf("datagram of %d bytes is over the %d byte limit", len(p), MaxDatagram)
	}

	frame := make([]byte, datagramHeader, datagramHeader+len(p))
	binary.BigEndian.PutUint16(frame, uint16(len(p)))

	if _, err := w.Write(append(frame, p...)); err != nil {
		return fmt.Errorf("write datagram: %w", err)
	}

	return nil
}

// ReadDatagram reads the next frame into buf, which should hold MaxDatagram bytes, and returns its length.
func ReadDatagram(r io.Reader, buf []byte) (int, error) {
	var size [datagramHeader]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return 0, fmt.Errorf("read datagram: %w", err)
	}

	n := int(binary.BigEndian.Uint16(size[:]))
	if n > len(buf) {
		return 0, fmt.Errorf("datagram of %d bytes does not fit a %d byte buffer", n, len(buf))
	}

	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, fmt.Errorf("read datagram: %w", err)
	}

	return n, nil
}
