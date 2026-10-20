package server

import (
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aelpxy/parrot/internal/protocol"
)

const (
	udpIdleTimeout   = 2 * time.Minute
	udpSweepInterval = 30 * time.Second
	// maxUDPFlows bounds the senders one UDP tunnel tracks at once, each costing a stream.
	maxUDPFlows = 256
)

// udpRelay carries one UDP tunnel. Each remote sender gets its own stream to the client, so the
// client can answer from a socket of its own and replies reach the right sender.
type udpRelay struct {
	pc     net.PacketConn
	open   func(remoteAddr string) (net.Conn, error)
	logger *slog.Logger
	done   chan struct{}
	wg     sync.WaitGroup

	mu    sync.Mutex
	flows map[string]*udpFlow
}

type udpFlow struct {
	stream net.Conn
	// last is when the flow last carried a packet either way, in Unix nanoseconds.
	last atomic.Int64
}

func (f *udpFlow) touch() { f.last.Store(time.Now().UnixNano()) }

func (f *udpFlow) idle() time.Duration { return time.Since(time.Unix(0, f.last.Load())) }

func newUDPRelay(pc net.PacketConn, open func(string) (net.Conn, error), logger *slog.Logger) *udpRelay {
	return &udpRelay{pc: pc, open: open, logger: logger, done: make(chan struct{}), flows: make(map[string]*udpFlow)}
}

// run forwards packets until pc is closed, then closes every flow and waits for them to end.
func (r *udpRelay) run() {
	defer r.shutdown()

	r.wg.Go(r.sweep)

	buf := make([]byte, protocol.MaxDatagram)

	for {
		n, addr, err := r.pc.ReadFrom(buf)
		if err != nil {
			return
		}

		f := r.flow(addr)
		if f == nil {
			continue
		}

		f.touch()

		if err := protocol.WriteDatagram(f.stream, buf[:n]); err != nil {
			r.drop(addr.String(), f)
		}
	}
}

// flow returns addr's flow, opening a stream for a new sender; nil means the packet is dropped.
func (r *udpRelay) flow(addr net.Addr) *udpFlow {
	key := addr.String()

	r.mu.Lock()
	f, ok := r.flows[key]
	full := len(r.flows) >= maxUDPFlows
	r.mu.Unlock()

	if ok {
		return f
	}

	if full {
		r.logger.Debug("udp sender dropped, too many senders", "sender", key)

		return nil
	}

	stream, err := r.open(key)
	if err != nil {
		r.logger.Debug("open udp stream failed", "sender", key, "error", err)

		return nil
	}

	f = &udpFlow{stream: stream}

	r.mu.Lock()
	r.flows[key] = f
	r.mu.Unlock()

	r.wg.Go(func() { r.reply(key, addr, f) })

	return f
}

// reply sends the client's answers for one sender back to it until the stream ends.
func (r *udpRelay) reply(key string, addr net.Addr, f *udpFlow) {
	defer r.drop(key, f)

	buf := make([]byte, protocol.MaxDatagram)

	for {
		n, err := protocol.ReadDatagram(f.stream, buf)
		if err != nil {
			return
		}

		f.touch()

		if _, err := r.pc.WriteTo(buf[:n], addr); err != nil {
			r.logger.Debug("udp reply failed", "sender", key, "error", err)
		}
	}
}

// sweep closes flows that have been quiet for udpIdleTimeout, since UDP has no end of its own.
func (r *udpRelay) sweep() {
	t := time.NewTicker(udpSweepInterval)
	defer t.Stop()

	for {
		select {
		case <-r.done:
			return
		case <-t.C:
		}

		r.mu.Lock()

		for key, f := range r.flows {
			if f.idle() > udpIdleTimeout {
				delete(r.flows, key)
				closeQuietly(f.stream)
			}
		}

		r.mu.Unlock()
	}
}

func (r *udpRelay) drop(key string, f *udpFlow) {
	r.mu.Lock()
	if r.flows[key] == f {
		delete(r.flows, key)
	}
	r.mu.Unlock()

	closeQuietly(f.stream)
}

func (r *udpRelay) shutdown() {
	close(r.done)

	r.mu.Lock()

	for key, f := range r.flows {
		delete(r.flows, key)
		closeQuietly(f.stream)
	}

	r.mu.Unlock()

	r.wg.Wait()
}
