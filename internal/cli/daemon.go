package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aelpxy/parrot/internal/config"
	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/tunnel"
	"github.com/aelpxy/parrot/internal/ui"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const (
	daemonStartTimeout = 5 * time.Second
	daemonPollInterval = 50 * time.Millisecond
	daemonLogName      = "daemon.log"
	autostartMinWait   = time.Second
	autostartMaxWait   = 30 * time.Second
	autostartBackoff   = 2
	detachedFooter     = "running in the background · 'parrot ls' · 'parrot logs <name>' · 'parrot stop <name>'"
)

func (a *App) daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Short:  "Run the background tunnel daemon",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDaemon(cmd.Context())
		},
	}
}

func (a *App) runDaemon(ctx context.Context) error {
	socket, err := daemon.SocketPath()
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}

	if _, err := daemon.NewClient(socket).List(ctx); err == nil {
		return errors.New("a daemon is already running")
	}

	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket: %w", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", socket, err)
	}

	if err := os.Chmod(socket, 0o600); err != nil {
		return errors.Join(fmt.Errorf("secure socket: %w", err), ln.Close())
	}

	logger := a.logger(slog.LevelInfo)

	var wg sync.WaitGroup

	wg.Go(func() { a.autostart(ctx, daemon.NewClient(socket), logger) })

	err = daemon.New(logger).Serve(ctx, ln)

	wg.Wait()

	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	return nil
}

// autostart makes the daemon run exactly the user config's autostart tunnels, retrying until the
// server is reachable.
func (a *App) autostart(ctx context.Context, c *daemon.Client, logger *slog.Logger) {
	cfg, err := loadUserConfig()
	if err != nil {
		logger.Error("autostart skipped", "error", err)

		return
	}

	specs := slices.DeleteFunc(slices.Clone(cfg.Tunnels), func(s tunnel.Spec) bool { return !s.Autostart })
	if len(specs) == 0 {
		return
	}

	opts, err := a.clientOptions()
	if err != nil {
		logger.Warn("autostart skipped", "error", err)

		return
	}

	source, err := config.UserPath()
	if err != nil {
		logger.Warn("autostart skipped", "error", err)

		return
	}

	req := &daemon.ApplyRequest{Source: source, Server: opts.Server, Token: opts.Token, TLS: opts.TLS != nil, Specs: specs, Prune: true}

	for wait := autostartMinWait; ; wait = min(wait*autostartBackoff, autostartMaxWait) {
		_, err := c.Apply(ctx, req)
		if err == nil {
			logger.Info("autostarted tunnels", "count", len(specs))

			return
		}

		var apiErr *daemon.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			logger.Warn("autostart skipped", "error", err)

			return
		}

		logger.Warn("autostart failed, retrying", "error", err, "retry_in", wait)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// detachTunnels hands specs to the daemon, starting it first if needed, and returns once they are online.
// applyTunnels makes the background tunnels from the config at path match req; see daemon.ApplyRequest.
func (a *App) applyTunnels(ctx context.Context, path string, req *daemon.ApplyRequest) error {
	opts, err := a.clientOptions()
	if err != nil {
		return err
	}

	c, err := a.startedDaemon(ctx)
	if err != nil {
		return err
	}

	req.Server, req.Token, req.TLS = opts.Server, opts.Token, opts.TLS != nil

	res, err := c.Apply(ctx, req)
	if err != nil {
		var apiErr *daemon.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return withHint(err, "rename it in "+path+", or stop the other one with 'parrot stop <name>'")
		}

		return connectError(err)
	}

	rows := make([]ui.TunnelRow, 0, len(res.Tunnels))
	for _, info := range res.Tunnels {
		rows = append(rows, ui.TunnelRow{Name: info.Name, URL: info.URL, Target: info.Target})
	}

	a.ui().Online(rows, applySummary(res)+" · 'parrot down' stops them")

	return nil
}

// applySummary says what an apply changed, such as "started api · stopped db".
func applySummary(res *daemon.ApplyResult) string {
	var parts []string

	for _, change := range []struct {
		verb  string
		names []string
	}{{"started", res.Started}, {"restarted", res.Restarted}, {"stopped", res.Stopped}} {
		if len(change.names) > 0 {
			parts = append(parts, change.verb+" "+strings.Join(change.names, ", "))
		}
	}

	if len(parts) == 0 {
		return "already up to date"
	}

	return strings.Join(parts, " · ")
}

// down stops every background tunnel started from the config at path, even if the file is gone.
func (a *App) down(ctx context.Context, path string) error {
	source, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("config path: %w", err)
	}

	c, err := runningDaemon()
	if err != nil {
		return err
	}

	res, err := c.Apply(ctx, &daemon.ApplyRequest{Source: source, Prune: true})
	if errors.Is(err, daemon.ErrNotRunning) || (err == nil && len(res.Stopped) == 0) {
		a.ui().Note("nothing from " + path + " is running")

		return nil
	}

	if err != nil {
		return daemonError(err)
	}

	a.ui().Success("stopped " + strings.Join(res.Stopped, ", "))

	return nil
}

func (a *App) detachTunnels(ctx context.Context, specs []tunnel.Spec) error {
	opts, err := a.clientOptions()
	if err != nil {
		return err
	}

	c, err := a.startedDaemon(ctx)
	if err != nil {
		return err
	}

	infos, err := c.Start(ctx, &daemon.StartRequest{Server: opts.Server, Token: opts.Token, TLS: opts.TLS != nil, Specs: specs})
	if err != nil {
		var apiErr *daemon.APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return withHint(err, "pick another name with '-n <name>', or stop it with 'parrot stop <name>'")
		}

		return connectError(err)
	}

	rows := make([]ui.TunnelRow, 0, len(infos))
	for _, info := range infos {
		rows = append(rows, ui.TunnelRow{Name: info.Name, URL: info.URL, Target: info.Target})
	}

	a.ui().Online(rows, detachedFooter)

	return nil
}

// startedDaemon returns a client for the daemon, spawning one in the background if none is running.
func (a *App) startedDaemon(ctx context.Context) (*daemon.Client, error) {
	socket, err := daemon.SocketPath()
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	c := daemon.NewClient(socket)

	_, err = c.List(ctx)
	if err == nil {
		return c, nil
	}

	if !errors.Is(err, daemon.ErrNotRunning) {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	args := []string{"daemon"}
	if a.debug {
		args = append(args, "--debug")
	}

	logPath, err := spawnDaemon(ctx, socket, args)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, daemonStartTimeout)
	defer cancel()

	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()

	for {
		if _, err := c.List(ctx); err == nil {
			return c, nil
		}

		select {
		case <-ctx.Done():
			return nil, withHint(errors.New("background daemon did not start"), "check its log at "+logPath)
		case <-ticker.C:
		}
	}
}

// spawnDaemon runs this binary with args in its own session, logging to a file next to the socket.
func spawnDaemon(ctx context.Context, socket string, args []string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate parrot binary: %w", err)
	}

	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create socket dir: %w", err)
	}

	logPath := filepath.Join(dir, daemonLogName)

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open daemon log: %w", err)
	}

	cmd := exec.CommandContext(context.WithoutCancel(ctx), exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

	err = errors.Join(cmd.Start(), logFile.Close())
	if err != nil {
		return "", fmt.Errorf("start daemon: %w", err)
	}

	if err := cmd.Process.Release(); err != nil {
		return "", fmt.Errorf("release daemon: %w", err)
	}

	return logPath, nil
}
