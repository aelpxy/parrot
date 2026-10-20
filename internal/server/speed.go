package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/aelpxy/parrot/internal/protocol"
)

const (
	// maxSpeedTests bounds concurrent tests server-wide so they cannot starve tunnel traffic.
	maxSpeedTests     = 2
	speedTestDuration = 5 * time.Second
	speedTestTimeout  = 15 * time.Second
	speedBufferSize   = 32 << 10
)

// speedTest moves data over a new stream in direction and reports what the receiving side measured.
func (ss *session) speedTest(direction protocol.SpeedDirection) *protocol.Message {
	select {
	case ss.srv.speedTests <- struct{}{}:
		defer func() { <-ss.srv.speedTests }()
	default:
		return &protocol.Message{Kind: protocol.KindError, Error: "the server is busy with other speed tests, try again shortly"}
	}

	result, err := ss.runSpeedTest(direction)
	if err != nil {
		ss.logger.Debug("speed test failed", "direction", direction, "error", err)

		return &protocol.Message{Kind: protocol.KindError, Error: "speed test failed: " + err.Error()}
	}

	ss.logger.Info("speed test", "direction", direction, "bytes", result.Bytes, "elapsed", time.Duration(result.ElapsedNS))

	return &protocol.Message{Kind: protocol.KindSpeedResult, Speed: result}
}

func (ss *session) runSpeedTest(direction protocol.SpeedDirection) (*protocol.Speed, error) {
	if direction != protocol.SpeedDownload && direction != protocol.SpeedUpload {
		return nil, errors.New("unknown direction")
	}

	stream, err := ss.client.open("", "")
	if err != nil {
		return nil, err
	}
	defer ss.closeConn(stream)

	if err := stream.SetDeadline(time.Now().Add(speedTestTimeout)); err != nil {
		return nil, fmt.Errorf("set deadline: %w", err)
	}

	start := time.Now()

	n, err := receiveOrSend(stream, direction)
	if err != nil {
		return nil, err
	}

	return &protocol.Speed{Direction: direction, Bytes: n, ElapsedNS: int64(time.Since(start))}, nil
}

// receiveOrSend drains an upload until the client closes its side, or sends a download for speedTestDuration.
func receiveOrSend(stream net.Conn, direction protocol.SpeedDirection) (int64, error) {
	if direction == protocol.SpeedUpload {
		n, err := io.Copy(io.Discard, stream)
		if err != nil {
			return n, fmt.Errorf("receive: %w", err)
		}

		return n, nil
	}

	buf := make([]byte, speedBufferSize)

	var n int64

	for start := time.Now(); time.Since(start) < speedTestDuration; {
		written, err := stream.Write(buf)
		n += int64(written)

		if err != nil {
			return n, fmt.Errorf("send: %w", err)
		}
	}

	// Closing the write side ends the client's read, so it times exactly what arrived.
	if err := stream.Close(); err != nil {
		return n, fmt.Errorf("close: %w", err)
	}

	return n, nil
}
