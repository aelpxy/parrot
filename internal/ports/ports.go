// Package ports finds local TCP ports that are listening and the processes behind them.
//
// It reads Linux's /proc; on other systems Listening returns ErrUnsupported.
package ports

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/aelpxy/parrot/internal/tunnel"
)

const (
	stateListen   = "0A"
	maxDetailLen  = 32
	maxDetailArgs = 2
	inodeField    = 9
	minTCPFields  = 10
	socketPrefix  = "socket:["
	ownBinaryName = "parrot"
)

// ErrUnsupported is returned where /proc is not available.
var ErrUnsupported = errors.New("listing ports is only supported on linux")

// Listener is a listening TCP socket.
type Listener struct {
	Process string
	Addr    netip.AddrPort
	PID     int
}

// Scanner reads socket and process information from a /proc file system.
type Scanner struct {
	// Proc is the /proc file system; nil means the real one.
	Proc fs.FS
	// SelfPID is excluded from results; zero means the current process.
	SelfPID int
}

// Target returns the address to dial: localhost for wildcard and loopback binds, the bound IP otherwise.
func (l Listener) Target() string {
	ip := l.Addr.Addr()
	if ip.IsUnspecified() || ip.IsLoopback() {
		return "localhost:" + strconv.Itoa(int(l.Addr.Port()))
	}

	return l.Addr.String()
}

// Proto guesses whether the port speaks HTTP or a raw TCP protocol.
func (l Listener) Proto() tunnel.Proto {
	tcpPorts := []uint16{22, 1433, 3306, 5432, 5672, 6379, 9092, 11211, 27017}
	tcpProcesses := []string{"sshd", "postgres", "mysqld", "mariadbd", "redis-server", "mongod", "memcached"}

	name, _, _ := strings.Cut(l.Process, " ")
	if slices.Contains(tcpPorts, l.Addr.Port()) || slices.Contains(tcpProcesses, name) {
		return tunnel.TCP
	}

	return tunnel.HTTP
}

// Listening returns listening TCP sockets sorted by port, one per port, excluding parrot itself.
func (s *Scanner) Listening() ([]Listener, error) {
	proc, self := s.Proc, s.SelfPID
	if proc == nil {
		proc = os.DirFS("/proc")
	}

	if self == 0 {
		self = os.Getpid()
	}

	byInode, err := listeningSockets(proc)
	if err != nil {
		return nil, err
	}

	owners := socketOwners(proc, byInode)
	listeners := make([]Listener, 0, len(byInode))

	for inode, addr := range byInode {
		l := Listener{Addr: addr, PID: owners[inode]}
		if l.PID != 0 {
			l.Process = processLabel(proc, l.PID)
		}

		if l.PID == self || strings.HasPrefix(l.Process, ownBinaryName) || systemService(l) {
			continue
		}

		listeners = append(listeners, l)
	}

	return dedupe(listeners), nil
}

// listeningSockets maps socket inodes to their addresses from net/tcp and net/tcp6.
func listeningSockets(proc fs.FS) (map[string]netip.AddrPort, error) {
	sockets := make(map[string]netip.AddrPort)

	for _, name := range []string{"net/tcp", "net/tcp6"} {
		f, err := proc.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}

		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		err = errors.Join(parseTCP(f, sockets), f.Close())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
	}

	if len(sockets) == 0 {
		if _, err := fs.Stat(proc, "net/tcp"); errors.Is(err, fs.ErrNotExist) {
			return nil, ErrUnsupported
		}
	}

	return sockets, nil
}

func parseTCP(f fs.File, sockets map[string]netip.AddrPort) error {
	sc := bufio.NewScanner(f)
	sc.Scan()

	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < minTCPFields || fields[3] != stateListen {
			continue
		}

		addr, err := parseAddr(fields[1])
		if err != nil {
			return err
		}

		sockets[fields[inodeField]] = addr
	}

	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	return nil
}

// parseAddr decodes /proc's "0100007F:0BB8" form: hex IP in host byte order per 32-bit word, hex port.
func parseAddr(s string) (netip.AddrPort, error) {
	hexIP, hexPort, ok := strings.Cut(s, ":")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("malformed address %q", s)
	}

	port, err := strconv.ParseUint(hexPort, 16, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("malformed port in %q: %w", s, err)
	}

	raw, err := hex.DecodeString(hexIP)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, fmt.Errorf("malformed ip in %q", s)
	}

	for word := range len(raw) / 4 {
		slices.Reverse(raw[word*4 : word*4+4])
	}

	ip, _ := netip.AddrFromSlice(raw)

	return netip.AddrPortFrom(ip.Unmap(), uint16(port)), nil
}

// socketOwners finds which pid holds each wanted socket inode, skipping processes we cannot inspect.
func socketOwners(proc fs.FS, wanted map[string]netip.AddrPort) map[string]int {
	owners := make(map[string]int, len(wanted))

	entries, err := fs.ReadDir(proc, ".")
	if err != nil {
		return owners
	}

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}

		fds, err := fs.ReadDir(proc, path.Join(e.Name(), "fd"))
		if err != nil {
			continue
		}

		for _, fd := range fds {
			link, err := fs.ReadLink(proc, path.Join(e.Name(), "fd", fd.Name()))
			if err != nil || !strings.HasPrefix(link, socketPrefix) {
				continue
			}

			inode := strings.TrimSuffix(strings.TrimPrefix(link, socketPrefix), "]")
			if _, ok := wanted[inode]; ok {
				owners[inode] = pid
			}
		}
	}

	return owners
}

// processLabel describes a process as "name" or "name (detail)", e.g. "node (next dev)".
func processLabel(proc fs.FS, pid int) string {
	dir := strconv.Itoa(pid)

	comm, err := fs.ReadFile(proc, path.Join(dir, "comm"))
	if err != nil {
		return ""
	}

	name := strings.TrimSpace(string(comm))

	cmdline, err := fs.ReadFile(proc, path.Join(dir, "cmdline"))
	if err != nil {
		return name
	}

	var parts []string

	for _, arg := range strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}

		if parts = append(parts, path.Base(arg)); len(parts) == maxDetailArgs {
			break
		}
	}

	detail := strings.Join(parts, " ")
	if detail == "" || detail == name {
		return name
	}

	if len(detail) > maxDetailLen {
		detail = detail[:maxDetailLen] + "…"
	}

	return name + " (" + detail + ")"
}

// systemService reports OS resolvers and similar daemons whose owner we cannot see; they are never tunnel targets.
func systemService(l Listener) bool {
	systemPorts := []uint16{25, 53, 111, 631, 5353, 5355}

	return l.PID == 0 && slices.Contains(systemPorts, l.Addr.Port())
}

// dedupe keeps one listener per port, preferring ones reachable through localhost, sorted by port.
func dedupe(ls []Listener) []Listener {
	slices.SortFunc(ls, func(a, b Listener) int {
		if c := int(a.Addr.Port()) - int(b.Addr.Port()); c != 0 {
			return c
		}

		return rank(a) - rank(b)
	})

	return slices.CompactFunc(ls, func(a, b Listener) bool { return a.Addr.Port() == b.Addr.Port() })
}

// Preference order when one port is bound several times.
const (
	rankLoopback = iota
	rankWildcard
	rankOther
)

func rank(l Listener) int {
	switch ip := l.Addr.Addr(); {
	case ip.IsLoopback():
		return rankLoopback
	case ip.IsUnspecified():
		return rankWildcard
	}

	return rankOther
}
