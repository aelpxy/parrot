package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/config"
)

func writeServerConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadServerFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want config.Server
	}{
		{
			name: "full",
			in: `
domain = "tryparrot.site"
tcp_ports = "30000-40000"
data_dir = "/var/lib/parrot"
allow_anonymous = true
ssh_on_https = true
`,
			want: config.Server{Domain: "tryparrot.site", TCPPorts: "30000-40000", DataDir: "/var/lib/parrot", AllowAnonymous: true, SSHOnHTTPS: true},
		},
		{name: "empty", in: "", want: config.Server{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := config.LoadServerFile(writeServerConfig(t, tt.in))
			if err != nil {
				t.Fatalf("LoadServerFile() error: %v", err)
			}

			if *got != tt.want {
				t.Errorf("LoadServerFile() = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestLoadServerFileErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr string
	}{
		{name: "unknown key", in: `domian = "x"`, wantErr: `unknown key "domian"`},
		{name: "flag spelling", in: `tcp-ports = "1-2"`, wantErr: `unknown key "tcp-ports"`},
		{name: "wrong type", in: `allow_anonymous = "yes"`, wantErr: "parse"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := config.LoadServerFile(writeServerConfig(t, tt.in)); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("LoadServerFile() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
