package token_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/token"
)

func TestStore(t *testing.T) {
	t.Parallel()

	s := &token.Store{Dir: filepath.Join(t.TempDir(), "data")}

	if _, err := s.Authenticate("prt_nope"); !errors.Is(err, token.ErrInvalid) {
		t.Fatalf("Authenticate() on empty store error = %v, want ErrInvalid", err)
	}

	laptop, laptopSecret, err := s.Create("laptop")
	if err != nil {
		t.Fatal(err)
	}

	_, ciSecret, err := s.Create("")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(laptopSecret, "prt_") || laptopSecret == ciSecret {
		t.Errorf("unexpected secrets %q, %q", laptopSecret, ciSecret)
	}

	raw, err := os.ReadFile(filepath.Join(s.Dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), laptopSecret) {
		t.Error("tokens file contains a plaintext secret")
	}

	label, err := s.Authenticate(laptopSecret)
	if err != nil || !strings.Contains(label, "laptop") {
		t.Errorf("Authenticate() = %q, %v; want laptop label", label, err)
	}

	if err := s.Revoke(laptop.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Authenticate(laptopSecret); !errors.Is(err, token.ErrInvalid) {
		t.Errorf("Authenticate() after revoke error = %v, want ErrInvalid", err)
	}

	if _, err := s.Authenticate(ciSecret); err != nil {
		t.Errorf("Authenticate() other token error = %v", err)
	}

	if err := s.Revoke(laptop.ID); !errors.Is(err, token.ErrNotFound) {
		t.Errorf("Revoke() twice error = %v, want ErrNotFound", err)
	}

	tokens, err := s.List()
	if err != nil || len(tokens) != 1 {
		t.Errorf("List() = %v, %v; want one token", tokens, err)
	}
}
