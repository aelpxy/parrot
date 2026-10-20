package sshkey_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/aelpxy/parrot/internal/sshkey"
)

func TestLoadOrCreateKeepsKey(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state", "ssh_host_ed25519_key")

	first, err := sshkey.LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate() first call: %v", err)
	}

	second, err := sshkey.LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate() second call: %v", err)
	}

	if a, b := ssh.FingerprintSHA256(first.PublicKey()), ssh.FingerprintSHA256(second.PublicKey()); a != b {
		t.Errorf("fingerprint changed across loads: %s then %s", a, b)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("host key mode = %o, want 600", mode)
	}
}
