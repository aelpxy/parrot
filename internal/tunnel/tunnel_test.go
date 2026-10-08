package tunnel_test

import (
	"errors"
	"testing"

	"github.com/aelpxy/parrot/internal/tunnel"
)

func TestParseTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "3000", want: "localhost:3000"},
		{in: ":3000", want: "localhost:3000"},
		{in: "192.168.1.5:22", want: "192.168.1.5:22"},
		{in: "[::1]:8080", want: "[::1]:8080"},
		{in: "", wantErr: true},
		{in: "0", wantErr: true},
		{in: "65536", wantErr: true},
		{in: "localhost", wantErr: true},
		{in: "host:http", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := tunnel.ParseTarget(tt.in)
			if tt.wantErr {
				if !errors.Is(err, tunnel.ErrInvalidTarget) {
					t.Fatalf("ParseTarget(%q) error = %v, want ErrInvalidTarget", tt.in, err)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseTarget(%q) unexpected error: %v", tt.in, err)
			}

			if got != tt.want {
				t.Errorf("ParseTarget(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSpecNormalize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		spec    tunnel.Spec
		wantErr bool
	}{
		{name: "http", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "localhost:3000", Subdomain: "my-app", Auth: "a:b"}},
		{name: "tcp", spec: tunnel.Spec{Proto: tunnel.TCP, Target: "5432", RemotePort: 35432}},
		{name: "missing proto", spec: tunnel.Spec{Target: "localhost:3000"}, wantErr: true},
		{name: "bad target", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "nope"}, wantErr: true},
		{name: "bad subdomain", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "localhost:1", Subdomain: "My_App"}, wantErr: true},
		{name: "bad auth", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "localhost:1", Auth: "admin"}, wantErr: true},
		{name: "auth without user", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "localhost:1", Auth: ":secret"}, wantErr: true},
		{name: "auth without pass", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "localhost:1", Auth: "admin:"}, wantErr: true},
		{name: "http remote port", spec: tunnel.Spec{Proto: tunnel.HTTP, Target: "localhost:1", RemotePort: 80}, wantErr: true},
		{name: "tcp subdomain", spec: tunnel.Spec{Proto: tunnel.TCP, Target: "localhost:1", Subdomain: "x"}, wantErr: true},
		{name: "tcp port range", spec: tunnel.Spec{Proto: tunnel.TCP, Target: "localhost:1", RemotePort: 70000}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.spec.Normalize()
			if (err != nil) != tt.wantErr {
				t.Errorf("Normalize() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
