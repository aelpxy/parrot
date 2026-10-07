package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/config"
	"github.com/aelpxy/parrot/internal/tunnel"
)

func TestLoad(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    []tunnel.Spec
		wantErr string
	}{
		{
			name: "shorthand",
			in: `
[tunnels]
web = 3000
db = "tcp:5432"
lan = "192.168.1.10:8080"
`,
			want: []tunnel.Spec{
				{Name: "db", Proto: tunnel.TCP, Target: "localhost:5432"},
				{Name: "lan", Proto: tunnel.HTTP, Target: "192.168.1.10:8080"},
				{Name: "web", Proto: tunnel.HTTP, Target: "localhost:3000"},
			},
		},
		{
			name: "tables",
			in: `
[tunnels.api]
target = 8080
subdomain = "acme-api"
auth = "admin:hunter2"
host = "api.local"

[tunnels.db]
proto = "tcp"
target = ":5432"
remote_port = 35432
autostart = true
`,
			want: []tunnel.Spec{
				{Name: "api", Proto: tunnel.HTTP, Target: "localhost:8080", Subdomain: "acme-api", Auth: "admin:hunter2", HostHeader: "api.local"},
				{Name: "db", Proto: tunnel.TCP, Target: "localhost:5432", RemotePort: 35432, Autostart: true},
			},
		},
		{name: "unknown top-level key", in: `tokn = "x"`, wantErr: `unknown key "tokn"`},
		{name: "unknown tunnel key", in: "[tunnels.web]\ntarget = 1\nport = 2", wantErr: `unknown key "tunnels.web.port"`},
		{name: "bad proto", in: "[tunnels.web]\nproto = \"udp\"\ntarget = 1", wantErr: "unknown protocol"},
		{name: "bad target", in: "[tunnels]\nweb = 70000", wantErr: "invalid target"},
		{name: "bad target type", in: "[tunnels]\nweb = true", wantErr: "expected a port"},
		{name: "tcp with subdomain", in: "[tunnels.db]\nproto = \"tcp\"\ntarget = 1\nsubdomain = \"x\"", wantErr: "only supported for http"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(strings.NewReader(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}

			if !reflect.DeepEqual(cfg.Tunnels, tt.want) {
				t.Errorf("Load() tunnels =\n%+v\nwant\n%+v", cfg.Tunnels, tt.want)
			}
		})
	}
}

func TestLoadUser(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(strings.NewReader("token = \"prt_123\"\nserver = \"parrot.sh\"\n"))
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Token != "prt_123" || cfg.Server != "parrot.sh" {
		t.Errorf("Load() = %+v, want token and server set", cfg)
	}
}

func TestCredentials(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "parrot", "credentials.toml")
	want := config.Credentials{Server: "tunnel.example.com", Token: "prt_abc"}

	if err := config.SaveCredentials(path, want); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials mode = %v, want 0600", perm)
	}

	got, err := config.LoadCredentials(path)
	if err != nil || got != want {
		t.Errorf("LoadCredentials() = %+v, %v; want %+v", got, err, want)
	}
}
