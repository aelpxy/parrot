package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/protocol"
)

const (
	speedTestDuration = 5 * time.Second
	speedBufferSize   = 32 << 10
	latencySamples    = 5
	bitsPerByte       = 8
)

// ErrSpeedTestUnsupported is returned when the server runs a parrot too old to answer speed tests.
var ErrSpeedTestUnsupported = errors.New("server does not support speed tests")

// SpeedTest measures the connection to a server over the same path tunnel traffic takes.
type SpeedTest struct {
	mux  *yamux.Session
	ctrl *protocol.Control
	stop func() bool
}

// DialSpeedTest connects and authenticates; canceling ctx aborts any test in progress.
func DialSpeedTest(ctx context.Context, opts *Options) (*SpeedTest, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	r := &runner{opts: opts, logger: logger}

	mux, err := r.connect(ctx)
	if err != nil {
		return nil, err
	}

	ctrl, stream, err := r.handshake(mux)
	if err == nil {
		err = stream.SetDeadline(time.Time{})
	}

	if err != nil {
		return nil, errors.Join(err, mux.Close())
	}

	stop := context.AfterFunc(ctx, func() { r.closeMux(mux) })

	return &SpeedTest{mux: mux, ctrl: ctrl, stop: stop}, nil
}

// Close ends the connection.
func (s *SpeedTest) Close() error {
	s.stop()

	if err := s.mux.Close(); err != nil {
		return fmt.Errorf("close session: %w", err)
	}

	return nil
}

// Latency returns the median round trip time to the server.
func (s *SpeedTest) Latency() (time.Duration, error) {
	samples := make([]time.Duration, 0, latencySamples)

	for range latencySamples {
		rtt, err := s.mux.Ping()
		if err != nil {
			return 0, fmt.Errorf("ping: %w", err)
		}

		samples = append(samples, rtt)
	}

	slices.Sort(samples)

	return samples[len(samples)/2], nil
}

// Download returns how fast the server can send to this machine, in bits per second.
func (s *SpeedTest) Download() (float64, error) {
	done := make(chan speedSample, 1)

	go func() { done <- receive(s.mux) }()

	if _, err := s.request(protocol.SpeedDownload); err != nil {
		return 0, s.abort(err, done)
	}

	got := <-done
	if got.err != nil {
		return 0, got.err
	}

	return bitsPerSecond(got.bytes, got.elapsed), nil
}

// Upload returns how fast this machine can send to the server, in bits per second.
func (s *SpeedTest) Upload() (float64, error) {
	done := make(chan speedSample, 1)

	go func() { done <- send(s.mux) }()

	result, err := s.request(protocol.SpeedUpload)
	if err != nil {
		return 0, s.abort(err, done)
	}

	if got := <-done; got.err != nil {
		return 0, got.err
	}

	return bitsPerSecond(result.Bytes, time.Duration(result.ElapsedNS)), nil
}

func (s *SpeedTest) request(direction protocol.SpeedDirection) (*protocol.Speed, error) {
	reply, err := s.ctrl.Request(&protocol.Message{Kind: protocol.KindSpeedTest, Speed: &protocol.Speed{Direction: direction}}, protocol.KindSpeedResult)

	// Servers from before speed tests reject the message kind they don't know.
	if remote, ok := errors.AsType[*protocol.RemoteError](err); ok && remote.Message == protocol.ErrUnexpectedMessage.Error() {
		return nil, ErrSpeedTestUnsupported
	}

	if err != nil {
		return nil, fmt.Errorf("%s test: %w", direction, err)
	}

	if reply.Speed == nil {
		return nil, fmt.Errorf("%s test: %w: empty result", direction, protocol.ErrUnexpectedMessage)
	}

	return reply.Speed, nil
}

// abort closes the session so the data goroutine stops, then waits for it.
func (s *SpeedTest) abort(err error, done <-chan speedSample) error {
	closeErr := s.Close()

	<-done

	if closeErr != nil {
		return errors.Join(err, closeErr)
	}

	return err
}

type speedSample struct {
	err     error
	bytes   int64
	elapsed time.Duration
}

// receive reads the server's download stream to the end, timing from the first byte.
func receive(mux *yamux.Session) speedSample {
	stream, err := acceptSpeedStream(mux)
	if err != nil {
		return speedSample{err: err}
	}
	defer func() { _ = stream.Close() }()

	buf := make([]byte, speedBufferSize)

	var (
		n     int64
		start time.Time
	)

	for {
		read, err := stream.Read(buf)
		if read > 0 && start.IsZero() {
			start = time.Now()
		}

		n += int64(read)

		if errors.Is(err, io.EOF) {
			return speedSample{bytes: n, elapsed: time.Since(start)}
		}

		if err != nil {
			return speedSample{err: fmt.Errorf("download: %w", err)}
		}
	}
}

// send writes to the server's upload stream for speedTestDuration, then closes its write side.
func send(mux *yamux.Session) speedSample {
	stream, err := acceptSpeedStream(mux)
	if err != nil {
		return speedSample{err: err}
	}
	defer func() { _ = stream.Close() }()

	buf := make([]byte, speedBufferSize)

	for start := time.Now(); time.Since(start) < speedTestDuration; {
		if _, err := stream.Write(buf); err != nil {
			return speedSample{err: fmt.Errorf("upload: %w", err)}
		}
	}

	// yamux's Close only ends our side, so the server still gets to send its result.
	if err := stream.Close(); err != nil {
		return speedSample{err: fmt.Errorf("upload: %w", err)}
	}

	return speedSample{}
}

func acceptSpeedStream(mux *yamux.Session) (net.Conn, error) {
	stream, err := mux.AcceptStream()
	if err != nil {
		return nil, fmt.Errorf("accept speed test stream: %w", err)
	}

	if _, err := protocol.ReadStreamHeader(stream); err != nil {
		return nil, errors.Join(err, stream.Close())
	}

	return stream, nil
}

func bitsPerSecond(bytes int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}

	return float64(bytes*bitsPerByte) / elapsed.Seconds()
}
