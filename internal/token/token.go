// Package token creates, stores, and verifies client auth tokens.
//
// Only SHA-256 hashes are stored; the token itself is shown once at creation.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aelpxy/parrot/internal/atomicfile"
)

const (
	prefix   = "prt_"
	idLength = 8
	fileName = "tokens.json"
)

// Sentinel errors for token lookups.
var (
	ErrNotFound = errors.New("token not found")
	ErrInvalid  = errors.New("invalid token")
)

// Token is a stored token record.
type Token struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	Name      string    `json:"name,omitzero"`
	Hash      string    `json:"hash"`
}

// Store keeps tokens in a JSON file inside Dir. It re-reads the file on every call,
// so tokens created or revoked by another process take effect immediately.
type Store struct {
	Dir string
}

// Create generates a new token and returns its record and the secret to hand to the user.
func (s *Store) Create(name string) (Token, string, error) {
	tokens, err := s.List()
	if err != nil {
		return Token{}, "", err
	}

	secret := prefix + strings.ToLower(rand.Text())
	t := Token{
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		ID:        strings.ToLower(rand.Text()[:idLength]),
		Name:      name,
		Hash:      hash(secret),
	}

	if err := s.save(append(tokens, t)); err != nil {
		return Token{}, "", err
	}

	return t, secret, nil
}

// List returns all tokens, oldest first.
func (s *Store) List() ([]Token, error) {
	f, err := os.Open(s.path())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("open tokens: %w", err)
	}

	var tokens []Token

	err = errors.Join(json.UnmarshalRead(f, &tokens), f.Close())
	if err != nil {
		return nil, fmt.Errorf("read tokens %s: %w", s.path(), err)
	}

	return tokens, nil
}

// Revoke deletes the token with id.
func (s *Store) Revoke(id string) error {
	tokens, err := s.List()
	if err != nil {
		return err
	}

	i := slices.IndexFunc(tokens, func(t Token) bool { return t.ID == id })
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	return s.save(slices.Delete(tokens, i, i+1))
}

// Authenticate verifies secret and returns a label for the matching token.
func (s *Store) Authenticate(secret string) (string, error) {
	tokens, err := s.List()
	if err != nil {
		return "", err
	}

	sum := []byte(hash(secret))

	for _, t := range tokens {
		if subtle.ConstantTimeCompare([]byte(t.Hash), sum) == 1 {
			return t.label(), nil
		}
	}

	return "", ErrInvalid
}

func (t Token) label() string {
	if t.Name == "" {
		return t.ID
	}

	return t.ID + " (" + t.Name + ")"
}

func (s *Store) path() string {
	return filepath.Join(s.Dir, fileName)
}

// save writes tokens atomically so a concurrently running server never reads a partial file.
func (s *Store) save(tokens []Token) error {
	err := atomicfile.Write(s.path(), func(w io.Writer) error {
		return json.MarshalWrite(w, tokens, jsontext.WithIndent("  "))
	})
	if err != nil {
		return fmt.Errorf("save tokens: %w", err)
	}

	return nil
}

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(sum[:])
}
