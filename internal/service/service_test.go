package service_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/service"
)

// fakeSystemctl records calls and answers `show` with canned properties.
type fakeSystemctl struct {
	calls [][]string
	show  string
}

func (f *fakeSystemctl) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)

	if slices.Contains(args, "show") {
		return []byte(f.show), nil
	}

	return nil, nil
}

func TestRender(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		unit    service.Unit
		want    []string
		notWant []string
	}{
		{
			name:    "daemon",
			unit:    service.DaemonUnit("/usr/local/bin/parrot"),
			want:    []string{"ExecStart=/usr/local/bin/parrot daemon\n", "WantedBy=default.target", "Restart=on-failure"},
			notWant: []string{"network-online", "ProtectSystem"},
		},
		{
			name: "server",
			unit: service.ServerUnit("/opt/my tools/parrot", []string{"--domain", "example.com", "--tls-cert", "/etc/cert 100%.pem"}),
			want: []string{
				`ExecStart="/opt/my tools/parrot" server --domain example.com --tls-cert "/etc/cert 100%%.pem"`,
				"WantedBy=multi-user.target", "StateDirectory=parrot", "ProtectSystem=strict", "After=network-online.target",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.unit.Render()

			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("Render() missing %q in:\n%s", w, got)
				}
			}

			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("Render() unexpectedly contains %q", w)
				}
			}
		})
	}
}

func TestInstallUninstall(t *testing.T) {
	t.Parallel()

	fake := &fakeSystemctl{}
	m := &service.Manager{Run: fake.run, UnitDir: t.TempDir()}
	u := service.DaemonUnit("/usr/bin/parrot")

	if err := m.Install(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	path, err := m.Path(u)
	if err != nil {
		t.Fatal(err)
	}

	if data, err := os.ReadFile(path); err != nil || string(data) != u.Render() {
		t.Fatalf("unit file = %q, %v; want rendered unit", data, err)
	}

	wantCalls := [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", "parrot.service"},
		{"--user", "restart", "parrot.service"},
	}
	if !slices.EqualFunc(fake.calls, wantCalls, slices.Equal) {
		t.Errorf("install calls = %q, want %q", fake.calls, wantCalls)
	}

	fake.calls = nil

	if err := m.Uninstall(t.Context(), u); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("unit file still present after uninstall: %v", err)
	}

	if len(fake.calls) != 2 || fake.calls[0][1] != "disable" {
		t.Errorf("uninstall calls = %q", fake.calls)
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		show string
		want service.Status
	}{
		{
			name: "running",
			show: "LoadState=loaded\nUnitFileState=enabled\nActiveState=active\nSubState=running\nMainPID=4242\nActiveEnterTimestamp=@1791382000\n",
			want: service.Status{Installed: true, Enabled: true, Active: "active", Sub: "running", PID: 4242},
		},
		{
			name: "not installed",
			show: "LoadState=not-found\nUnitFileState=\nActiveState=inactive\nSubState=dead\nMainPID=0\nActiveEnterTimestamp=\n",
			want: service.Status{Active: "inactive", Sub: "dead"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := &service.Manager{Run: (&fakeSystemctl{show: tt.show}).run, UnitDir: filepath.Join(t.TempDir(), "units")}

			got, err := m.Status(t.Context(), service.DaemonUnit("/usr/bin/parrot"))
			if err != nil {
				t.Fatal(err)
			}

			got.Since = got.Since.UTC()
			want := tt.want

			if tt.want.Installed {
				want.Since = got.Since
				if got.Since.Unix() != 1791382000 {
					t.Errorf("Since = %v, want unix 1791382000", got.Since)
				}
			}

			if got != want {
				t.Errorf("Status() = %+v, want %+v", got, want)
			}
		})
	}
}
