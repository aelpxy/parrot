package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/aelpxy/parrot/internal/atomicfile"
)

// Credentials are written by `parrot login`, kept apart from the hand-edited config file.
type Credentials struct {
	Server string `toml:"server"`
	Token  string `toml:"token"`
}

// CredentialsPath returns the path of the per-user credentials file.
func CredentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate config dir: %w", err)
	}

	return filepath.Join(dir, "parrot", "credentials.toml"), nil
}

// LoadCredentials reads the credentials file at path.
func LoadCredentials(path string) (Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("open credentials: %w", err)
	}

	var c Credentials

	md, err := toml.NewDecoder(f).Decode(&c)

	err = errors.Join(err, f.Close())
	if err != nil {
		return Credentials{}, fmt.Errorf("%s: %w", path, err)
	}

	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return Credentials{}, fmt.Errorf("%s: unknown key %q", path, undecoded[0].String())
	}

	return c, nil
}

// SaveCredentials atomically writes c to path, readable only by the current user.
func SaveCredentials(path string, c Credentials) error {
	err := atomicfile.Write(path, func(w io.Writer) error {
		return toml.NewEncoder(w).Encode(c)
	})
	if err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	return nil
}
