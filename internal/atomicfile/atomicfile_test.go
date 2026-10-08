package atomicfile_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/aelpxy/parrot/internal/atomicfile"
)

func write(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, s)

		return err
	}
}

func TestWrite(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "secret.txt")

	if err := atomicfile.Write(path, write("v1")); err != nil {
		t.Fatal(err)
	}

	if err := atomicfile.Write(path, write("v2")); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "v2" {
		t.Fatalf("contents = %q, %v; want v2", data, err)
	}

	for p, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}

		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode = %v, want %v", p, got, want)
		}
	}
}

func TestWriteFailureKeepsOriginal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "tokens.json")

	if err := atomicfile.Write(path, write("good")); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("encode failed")

	err := atomicfile.Write(path, func(w io.Writer) error {
		_, _ = io.WriteString(w, "partial")

		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Write() error = %v, want %v", err, boom)
	}

	if data, _ := os.ReadFile(path); string(data) != "good" {
		t.Errorf("contents after failed write = %q, want original", data)
	}

	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir has %d entries, want only the original (temp file leaked)", len(entries))
	}
}
