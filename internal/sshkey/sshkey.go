// Package sshkey keeps an SSH host key on disk so clients see the same server identity across restarts.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"golang.org/x/crypto/ssh"

	"github.com/aelpxy/parrot/internal/atomicfile"
)

// LoadOrCreate returns the ed25519 host key at path, generating and saving one on first use.
func LoadOrCreate(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return create(path)
	}

	if err != nil {
		return nil, fmt.Errorf("read host key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse host key %s: %w", path, err)
	}

	return signer, nil
}

func create(path string) (ssh.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate host key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return nil, fmt.Errorf("encode host key: %w", err)
	}

	err = atomicfile.Write(path, func(w io.Writer) error {
		return pem.Encode(w, block)
	})
	if err != nil {
		return nil, fmt.Errorf("save host key: %w", err)
	}

	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, fmt.Errorf("load host key: %w", err)
	}

	return signer, nil
}
