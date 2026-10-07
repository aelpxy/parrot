package ports_test

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/aelpxy/parrot/internal/ports"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const tcpHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func symlink(target string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink}
}

func fakeProc() fstest.MapFS {
	return fstest.MapFS{
		"net/tcp": {Data: []byte(tcpHeader +
			// 127.0.0.1:3000 listening, owned by pid 100 (node).
			"   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 111 1\n" +
			// 0.0.0.0:5432 listening, owned by pid 200 (postgres).
			"   1: 00000000:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 222 1\n" +
			// An established connection, not a listener.
			"   2: 0100007F:0BB8 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 333 1\n" +
			// 192.168.1.5:8080, owner unknown.
			"   3: 0501A8C0:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 444 1\n" +
			// 0.0.0.0:7000 owned by parrot itself.
			"   4: 00000000:1B58 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 555 1\n" +
			// 127.0.0.53:53, the system resolver with an owner we cannot see.
			"   5: 3500007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000   101        0 777 1\n" +
			// 0.0.0.0:8000 owned by pid 400 (python3 -m http.server 8000).
			"   6: 00000000:1F40 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 888 1\n")},
		"net/tcp6": {Data: []byte(tcpHeader +
			// [::1]:3000 duplicate of the IPv4 listener.
			"   0: 00000000000000000000000001000000:0BB8 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 666 1\n")},
		"100/comm":    {Data: []byte("node\n")},
		"100/cmdline": {Data: []byte("node\x00/app/node_modules/.bin/next\x00dev\x00")},
		"100/fd/3":    symlink("pipe:[9]"),
		"100/fd/21":   symlink("socket:[111]"),
		"100/fd/22":   symlink("socket:[666]"),
		"200/comm":    {Data: []byte("postgres\n")},
		"200/cmdline": {Data: []byte("postgres\x00-D\x00/var/lib/postgres\x00")},
		"200/fd/5":    symlink("socket:[222]"),
		"300/comm":    {Data: []byte("parrot\n")},
		"400/comm":    {Data: []byte("python3\n")},
		"400/cmdline": {Data: []byte("python3\x00-m\x00http.server\x008000\x00")},
		"400/fd/3":    symlink("socket:[888]"),
		"300/fd/7":    symlink("socket:[555]"),
	}
}

func TestListening(t *testing.T) {
	t.Parallel()

	s := &ports.Scanner{Proc: fakeProc(), SelfPID: 999}

	got, err := s.Listening()
	if err != nil {
		t.Fatal(err)
	}

	want := []struct {
		target  string
		process string
		proto   tunnel.Proto
	}{
		{target: "localhost:3000", process: "node (next dev)", proto: tunnel.HTTP},
		{target: "localhost:5432", process: "postgres", proto: tunnel.TCP},
		{target: "localhost:8000", process: "python3 (http.server 8000)", proto: tunnel.HTTP},
		{target: "192.168.1.5:8080", process: "", proto: tunnel.HTTP},
	}

	if len(got) != len(want) {
		t.Fatalf("Listening() = %+v, want %d listeners", got, len(want))
	}

	for i, w := range want {
		if got[i].Target() != w.target || got[i].Process != w.process || got[i].Proto() != w.proto {
			t.Errorf("listener %d = %s %q %s, want %s %q %s",
				i, got[i].Target(), got[i].Process, got[i].Proto(), w.target, w.process, w.proto)
		}
	}
}

func TestListeningUnsupported(t *testing.T) {
	t.Parallel()

	s := &ports.Scanner{Proc: fstest.MapFS{}}
	if _, err := s.Listening(); !errors.Is(err, ports.ErrUnsupported) {
		t.Errorf("Listening() error = %v, want ErrUnsupported", err)
	}
}
