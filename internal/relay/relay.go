// Package relay copies bytes between two connections.
package relay

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
)

// Join copies between a and b in both directions, passing on half-closes, until both sides finish, then closes both.
func Join(a, b io.ReadWriteCloser) error {
	var (
		wg    sync.WaitGroup
		errAB error
	)

	wg.Go(func() {
		errAB = pipe(a, b)
	})

	errBA := pipe(b, a)

	wg.Wait()
	closeBoth(a, b)

	return errors.Join(ignoreClosed(errAB), ignoreClosed(errBA))
}

// pipe copies src to dst, then shuts dst's write side so its peer sees EOF but can still reply.
func pipe(dst, src io.ReadWriteCloser) error {
	_, err := io.Copy(dst, src)
	if err == nil {
		err = closeWrite(dst)
	}

	if err != nil {
		// Unblocks the opposite copy, which would otherwise wait on a dead connection.
		closeBoth(dst, src)
	}

	return err
}

// closeWrite half-closes c, falling back to a full close for types that cannot; yamux streams half-close on Close.
func closeWrite(c io.Closer) error {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			return fmt.Errorf("close write: %w", err)
		}

		return nil
	}

	if err := c.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	return nil
}

func closeBoth(a, b io.Closer) {
	// A second close or a peer reset is expected here.
	_ = a.Close()
	_ = b.Close()
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}

	return err
}
