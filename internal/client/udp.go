package client

import (
	"context"
	"errors"
	"net"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/tunnel"
)

// handleUDP relays one remote sender's packets between its stream and a local socket of its own,
// so the target's replies come back for that sender only. The server ends quiet senders' streams.
func (r *runner) handleUDP(ctx context.Context, stream net.Conn, spec *tunnel.Spec) {
	d := net.Dialer{Timeout: localDialTimeout}

	local, err := d.DialContext(ctx, "udp", udpTarget(spec.Target))
	if err != nil {
		r.localError(spec, err)
		r.closeConn(stream)

		return
	}

	var wg sync.WaitGroup

	wg.Go(func() {
		r.udpReplies(local, stream, spec)
		r.closeConn(stream)
	})

	r.udpRequests(stream, local)
	r.closeConn(local)
	wg.Wait()
}

// udpRequests passes the sender's packets to the local target until the stream ends.
func (r *runner) udpRequests(stream, local net.Conn) {
	buf := make([]byte, protocol.MaxDatagram)

	for {
		n, err := protocol.ReadDatagram(stream, buf)
		if err != nil {
			return
		}

		// A send fails when an earlier packet found nothing listening; the reply side reports that.
		if _, err := local.Write(buf[:n]); err != nil {
			r.logger.Debug("udp send to target failed", "error", err)
		}
	}
}

// udpReplies passes the target's answers back until the local socket is closed. Nothing listening
// on the target shows up here, once per sender, and the relay keeps going in case the app starts.
func (r *runner) udpReplies(local, stream net.Conn, spec *tunnel.Spec) {
	buf := make([]byte, protocol.MaxDatagram)
	reported := false

	for {
		n, err := local.Read(buf)
		if errors.Is(err, unix.ECONNREFUSED) {
			if !reported {
				r.localError(spec, err)
			}

			reported = true

			continue
		}

		if err != nil {
			return
		}

		if err := protocol.WriteDatagram(stream, buf[:n]); err != nil {
			return
		}
	}
}

func (r *runner) localError(spec *tunnel.Spec, err error) {
	if r.opts.OnStreamError != nil {
		r.opts.OnStreamError(spec.Name, &LocalError{Err: err, Target: spec.Target})
	}
}

// udpTarget sends "localhost" to 127.0.0.1. TCP falls back from ::1 when nothing listens there,
// but a UDP socket can't tell, and most UDP apps listen on IPv4 only.
func udpTarget(target string) string {
	host, port, err := net.SplitHostPort(target)
	if err != nil || host != "localhost" {
		return target
	}

	return net.JoinHostPort("127.0.0.1", port)
}
