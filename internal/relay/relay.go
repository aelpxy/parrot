// Package relay copies bytes between two connections.
package relay

import (
	"errors"
	"io"
	"net"
	"sync"
)

// Join copies between a and b in both directions until either side finishes, then closes both.
func Join(a, b io.ReadWriteCloser) error {
	var (
		wg    sync.WaitGroup
		errAB error
	)

	wg.Go(func() {
		_, errAB = io.Copy(a, b)

		closeBoth(a, b)
	})

	_, errBA := io.Copy(b, a)

	closeBoth(a, b)
	wg.Wait()

	return errors.Join(ignoreClosed(errAB), ignoreClosed(errBA))
}

func closeBoth(a, b io.Closer) {
	// Closing unblocks the opposite copy; a second close or a peer reset is expected here.
	_ = a.Close()
	_ = b.Close()
}

func ignoreClosed(err error) error {
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}

	return err
}
