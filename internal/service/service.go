// Package service installs parrot as a systemd unit: the tunnel daemon as a user service,
// and the forwarding server as a system service.
package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	systemdRunDir = "/run/systemd/system"
	systemUnitDir = "/etc/systemd/system"
	lingerDir     = "/var/lib/systemd/linger"
	notFound      = "not-found"
)

// ErrNoSystemd is returned when systemd is not the running init system.
var ErrNoSystemd = errors.New("systemd is not running on this system")

// Unit describes a systemd service.
type Unit struct {
	Name        string
	Description string
	Exec        []string
	// System units run under the system manager as root; others under the user's manager.
	System bool
}

// Status is a unit's current state as reported by systemd.
type Status struct {
	Since     time.Time
	Active    string
	Sub       string
	PID       int
	Installed bool
	Enabled   bool
}

// Running reports whether the unit's process is up.
func (s Status) Running() bool {
	return s.Active == "active"
}

// Manager drives systemctl. Run is nil for the real systemctl; tests substitute a fake.
type Manager struct {
	Run func(ctx context.Context, args ...string) ([]byte, error)
	// UnitDir overrides where unit files are written; empty means the standard location.
	UnitDir string
}

// DaemonUnit runs the background tunnel daemon for the current user.
func DaemonUnit(exe string) Unit {
	return Unit{Name: "parrot", Description: "parrot tunnel daemon", Exec: []string{exe, "daemon"}}
}

// ServerUnit runs the forwarding server system-wide with args such as --domain.
func ServerUnit(exe string, args []string) Unit {
	return Unit{
		Name:        "parrot-server",
		Description: "parrot tunnel server",
		Exec:        append([]string{exe, "server"}, args...),
		System:      true,
	}
}

// Render returns the unit file contents.
func (u Unit) Render() string {
	var b strings.Builder

	b.WriteString("[Unit]\nDescription=" + u.Description + "\n")

	if u.System {
		b.WriteString("After=network-online.target\nWants=network-online.target\n")
	}

	b.WriteString("\n[Service]\nExecStart=" + execLine(u.Exec) + "\nRestart=on-failure\nRestartSec=2\n")

	if u.System {
		// Runs as root to bind :80/:443 and read TLS keys; the filesystem is read-only except its state dir.
		b.WriteString("StateDirectory=parrot\nNoNewPrivileges=yes\nProtectSystem=strict\n" +
			"ProtectHome=read-only\nPrivateTmp=yes\nProtectKernelTunables=yes\nProtectControlGroups=yes\n")
	}

	b.WriteString("\n[Install]\nWantedBy=" + u.wantedBy() + "\n")

	return b.String()
}

// Path returns where the unit file is written.
func (m *Manager) Path(u Unit) (string, error) {
	dir := m.UnitDir

	switch {
	case dir != "":
	case u.System:
		dir = systemUnitDir
	default:
		configDir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("locate config dir: %w", err)
		}

		dir = filepath.Join(configDir, "systemd", "user")
	}

	return filepath.Join(dir, u.Name+".service"), nil
}

// Install writes the unit file, then enables and (re)starts the unit.
func (m *Manager) Install(ctx context.Context, u Unit) error {
	path, err := m.Path(u)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create unit dir: %w", err)
	}

	if err := os.WriteFile(path, []byte(u.Render()), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	if _, err := m.systemctl(ctx, u, "daemon-reload"); err != nil {
		return err
	}

	if _, err := m.systemctl(ctx, u, "enable", u.unitName()); err != nil {
		return err
	}

	_, err = m.systemctl(ctx, u, "restart", u.unitName())

	return err
}

// Uninstall stops and disables the unit and removes its file.
func (m *Manager) Uninstall(ctx context.Context, u Unit) error {
	path, err := m.Path(u)
	if err != nil {
		return err
	}

	if _, err := m.systemctl(ctx, u, "disable", "--now", u.unitName()); err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unit: %w", err)
	}

	_, err = m.systemctl(ctx, u, "daemon-reload")

	return err
}

// Status reports whether the unit is installed, enabled, and running.
func (m *Manager) Status(ctx context.Context, u Unit) (Status, error) {
	out, err := m.systemctl(ctx, u, "show", u.unitName(),
		"--property=LoadState,UnitFileState,ActiveState,SubState,MainPID,ActiveEnterTimestamp",
		"--timestamp=unix")
	if err != nil {
		return Status{}, err
	}

	props := parseProperties(out)
	s := Status{
		Installed: props["LoadState"] != notFound && props["LoadState"] != "",
		Enabled:   props["UnitFileState"] == "enabled",
		Active:    props["ActiveState"],
		Sub:       props["SubState"],
	}

	s.PID, _ = strconv.Atoi(props["MainPID"])

	if ts, ok := strings.CutPrefix(props["ActiveEnterTimestamp"], "@"); ok {
		if secs, err := strconv.ParseInt(ts, 10, 64); err == nil && secs > 0 {
			s.Since = time.Unix(secs, 0)
		}
	}

	return s, nil
}

// Available returns ErrNoSystemd unless systemd is the running init system.
func Available() error {
	if _, err := os.Stat(systemdRunDir); err != nil {
		return ErrNoSystemd
	}

	return nil
}

// Lingering reports whether the user's services keep running after they log out.
func Lingering() bool {
	u := os.Getenv("USER")
	if u == "" {
		return false
	}

	_, err := os.Stat(lingerDir + "/" + u)

	return err == nil
}

// LogsCommand returns the journalctl invocation that follows the unit's logs.
func LogsCommand(u Unit) string {
	if u.System {
		return "journalctl -u " + u.Name + " -f"
	}

	return "journalctl --user -u " + u.Name + " -f"
}

func (m *Manager) systemctl(ctx context.Context, u Unit, args ...string) ([]byte, error) {
	if !u.System {
		args = append([]string{"--user"}, args...)
	}

	run := m.Run
	if run == nil {
		run = runSystemctl
	}

	out, err := run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("systemctl %s: %w", strings.Join(args, " "), err)
	}

	return out, nil
}

func runSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}

		return nil, fmt.Errorf("run: %w", err)
	}

	return out, nil
}

func (u Unit) unitName() string {
	return u.Name + ".service"
}

func (u Unit) wantedBy() string {
	if u.System {
		return "multi-user.target"
	}

	return "default.target"
}

func parseProperties(out []byte) map[string]string {
	props := make(map[string]string)

	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			props[k] = v
		}
	}

	return props
}

// execLine quotes args for ExecStart, escaping the characters systemd expands.
func execLine(args []string) string {
	quoted := make([]string, 0, len(args))

	for _, a := range args {
		a = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(a)
		if a == "" || strings.ContainsAny(a, " \t'\"\\") {
			a = `"` + a + `"`
		}

		quoted = append(quoted, a)
	}

	return strings.Join(quoted, " ")
}
