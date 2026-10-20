package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/cli"
)

func TestRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	badConfig := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(badConfig, []byte("[tunnels.db]\nproto = \"tcp\"\ntarget = 1\nsubdomain = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "help", args: []string{"--help"}, wantStdout: "Expose local services"},
		{name: "help groups commands", args: []string{"--help"}, wantStdout: "TUNNELS\n  down"},
		{name: "subcommand help", args: []string{"tcp", "--help"}, wantStdout: "-p, --remote-port int"},
		{name: "help command", args: []string{"help", "server"}, wantStdout: "--tls-cert string"},
		{name: "version", args: []string{"version"}, wantStdout: "parrot "},
		{name: "typo suggests command", args: []string{"htp", "3000"}, wantCode: 1, wantStderr: "did you mean 'parrot http'?"},
		{name: "unknown command", args: []string{"zzz"}, wantCode: 1, wantStderr: `unknown command "zzz"`},
		{name: "invalid target", args: []string{"http", "abc"}, wantCode: 1, wantStderr: "use a port like 3000"},
		{name: "missing target", args: []string{"tcp"}, wantCode: 1, wantStderr: "try 'parrot tcp 5432'"},
		{name: "http-only flag on tcp", args: []string{"tcp", "22", "-s", "x"}, wantCode: 1, wantStderr: "parrot tcp --help"},
		{name: "missing config", args: []string{"up", "-c", filepath.Join(dir, "none.toml")}, wantCode: 1, wantStderr: "run 'parrot http <port>' directly"},
		{name: "invalid config", args: []string{"up", "-c", badConfig}, wantCode: 1, wantStderr: "only supported for http"},
		{name: "stop needs name", args: []string{"stop"}, wantCode: 1, wantStderr: "missing tunnel name"},
		{name: "stop name and all", args: []string{"stop", "web", "--all"}, wantCode: 1, wantStderr: "not both"},
		{name: "udp help", args: []string{"udp", "--help"}, wantStdout: "Expose a local UDP service"},
		{name: "server config unknown key", args: []string{"server", "--config", serverConfig(t, `domian = "x"`)}, wantCode: 1, wantStderr: `unknown key "domian"`},
		{name: "server config without domain", args: []string{"server", "--config", serverConfig(t, `allow_anonymous = true`)}, wantCode: 1, wantStderr: "set domain in --config"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer

			code := cli.New(strings.NewReader(""), &stdout, &stderr).Run(t.Context(), tt.args)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tt.wantCode, stderr.String())
			}

			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want containing %q", stdout.String(), tt.wantStdout)
			}

			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want containing %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

func serverConfig(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "server.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestServerConfigDataDir(t *testing.T) {
	t.Parallel()

	dataDir := filepath.Join(t.TempDir(), "state")
	path := serverConfig(t, "data_dir = "+strconv.Quote(dataDir))

	var stdout, stderr bytes.Buffer

	code := cli.New(strings.NewReader(""), &stdout, &stderr).Run(t.Context(), []string{"server", "token", "create", "--name", "ci", "--config", path})
	if code != 0 {
		t.Fatalf("token create exit code = %d\nstderr: %s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(dataDir, "tokens.json")); err != nil {
		t.Errorf("token not stored in the config's data_dir: %v", err)
	}
}

func TestVersionFlagMatchesCommand(t *testing.T) {
	t.Parallel()

	var flagOut, cmdOut, discard bytes.Buffer

	cli.New(strings.NewReader(""), &flagOut, &discard).Run(t.Context(), []string{"--version"})
	cli.New(strings.NewReader(""), &cmdOut, &discard).Run(t.Context(), []string{"version"})

	if flagOut.String() != cmdOut.String() || !strings.HasPrefix(cmdOut.String(), "parrot ") {
		t.Errorf("--version = %q, version = %q; want identical", flagOut.String(), cmdOut.String())
	}
}
