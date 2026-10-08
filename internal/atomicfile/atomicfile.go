// Package atomicfile replaces files so readers see the old or the new contents, never a partial write.
package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Write replaces path with what write produces, readable only by the owner,
// creating path's directory (also owner-only) if needed.
func Write(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	err = write(tmp)
	if err == nil {
		// Without a sync, a crash right after the rename can leave an empty file.
		err = tmp.Sync()
	}

	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}

	if err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), os.Remove(tmp.Name()))
	}

	return nil
}
