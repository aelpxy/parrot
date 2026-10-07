package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/tunnel"
	"github.com/aelpxy/parrot/internal/ui"
)

const (
	daemonStartTimeout = 5 * time.Second
	daemonPollInterval = 50 * time.Millisecond
	daemonLogName      = "daemon.log"
	autostartMinWait   = time.Second
	autostartMaxWait   = 30 * time.Second
	autostartBackoff   = 2
	noTunnels          = "no background tunnels"
	startOneHint       = "start one with 'parrot http 3000 -d'"
	detachedFooter     = "running in the background · 'parrot ls' · 'parrot logs <name>' · 'parrot stop <name>'"
)

func (a *App) lsCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List background tunnels",
		GroupID: groupTunnels,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			infos, err := a.listTunnels(cmd.Context())
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSON(a.stdout, infos)
			}

			if len(infos) == 0 {
				a.ui().Note(noTunnels, startOneHint)

				return nil
			}

			a.ui().TunnelList(statusRows(infos))

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	return cmd
}

func (a *App) logsCmd() *cobra.Command {
	var follow bool

	cmd := &cobra.Command{
		Use:     "logs <name>",
		Short:   "Show a background tunnel's logs",
		GroupID: groupTunnels,
		Example: "  parrot logs web\n  parrot logs web -f",
		Args:    requireName("parrot logs web -f"),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := runningDaemon()
			if err != nil {
				return err
			}

			p := a.ui()
			show := func(l daemon.LogLine) { p.LogLine(l.Time, l.Level, l.Message) }

			if follow {
				err = c.FollowLogs(cmd.Context(), args[0], show)
			} else {
				err = c.Logs(cmd.Context(), args[0], show)
			}

			return daemonError(err)
		},
		ValidArgsFunction: completeRunning,
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep streaming new lines")

	return cmd
}

func (a *App) stopCmd() *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:     "stop <name>",
		Short:   "Stop a background tunnel",
		GroupID: groupTunnels,
		Example: "  parrot stop web\n  parrot stop --all",
		Args: func(_ *cobra.Command, args []string) error {
			switch {
			case all && len(args) > 0:
				return errors.New("pass a tunnel name or --all, not both")
			case !all && len(args) != 1:
				return withHint(errors.New("missing tunnel name"), "run 'parrot ls' to see running tunnels, or pass '--all'")
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				return a.stopAll(cmd.Context())
			}

			c, err := runningDaemon()
			if err != nil {
				return err
			}

			if err := c.Stop(cmd.Context(), args[0]); err != nil {
				return daemonError(err)
			}

			a.ui().Success("stopped " + args[0])

			return nil
		},
		ValidArgsFunction: completeRunning,
	}

	cmd.Flags().BoolVar(&all, "all", false, "stop every background tunnel")

	return cmd
}

func (a *App) stopAll(ctx context.Context) error {
	c, err := runningDaemon()
	if err != nil {
		return err
	}

	names, err := c.StopAll(ctx)
	if err != nil && !errors.Is(err, daemon.ErrNotRunning) {
		return daemonError(err)
	}

	if len(names) == 0 {
		a.ui().Note(noTunnels)

		return nil
	}

	a.ui().Success("stopped " + strings.Join(names, ", "))

	return nil
}

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

// autostart starts the user config's autostart tunnels, retrying until the server is reachable.
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

	req := &daemon.StartRequest{Server: opts.Server, Token: opts.Token, TLS: opts.TLS != nil, Specs: specs}

	for wait := autostartMinWait; ; wait = min(wait*autostartBackoff, autostartMaxWait) {
		_, err := c.Start(ctx, req)
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

func (a *App) listTunnels(ctx context.Context) ([]daemon.TunnelInfo, error) {
	c, err := runningDaemon()
	if err != nil {
		return nil, err
	}

	infos, err := c.List(ctx)
	if errors.Is(err, daemon.ErrNotRunning) {
		return []daemon.TunnelInfo{}, nil
	}

	return infos, daemonError(err)
}

// runningDaemon returns a client for the daemon socket without starting a daemon.
func runningDaemon() (*daemon.Client, error) {
	socket, err := daemon.SocketPath()
	if err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	return daemon.NewClient(socket), nil
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

	logPath, err := spawnDaemon(ctx, socket, a.debug)
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

// spawnDaemon starts `parrot daemon` in its own session, logging to a file next to the socket.
func spawnDaemon(ctx context.Context, socket string, debug bool) (string, error) {
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

	args := []string{"daemon"}
	if debug {
		args = append(args, "--debug")
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

func daemonError(err error) error {
	var apiErr *daemon.APIError

	switch {
	case err == nil:
		return nil
	case errors.Is(err, daemon.ErrNotRunning):
		return withHint(errors.New(noTunnels), startOneHint)
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return withHint(err, "run 'parrot ls' to see running tunnels")
	}

	return err
}

func completeRunning(cmd *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
	c, err := runningDaemon()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	infos, err := c.List(cmd.Context())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	names := make([]cobra.Completion, 0, len(infos))
	for _, info := range infos {
		names = append(names, cobra.CompletionWithDesc(info.Name, info.Proto.String()+" "+info.URL))
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}

func statusRows(infos []daemon.TunnelInfo) []ui.TunnelStatus {
	rows := make([]ui.TunnelStatus, 0, len(infos))
	for _, info := range infos {
		rows = append(rows, ui.TunnelStatus{
			Name:    info.Name,
			Proto:   info.Proto.String(),
			URL:     info.URL,
			Target:  info.Target,
			Status:  info.Status.String(),
			Uptime:  shortDuration(time.Since(info.StartedAt)),
			Healthy: info.Status == daemon.StatusOnline,
		})
	}

	return rows
}

// shortDuration formats d like "45s", "12m", or "3h12m".
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	}

	return strconv.Itoa(int(d.Hours())) + "h" + strconv.Itoa(int((d % time.Hour).Minutes())) + "m"
}

func writeJSON(w io.Writer, v any) error {
	if err := json.MarshalWrite(w, v, jsontext.WithIndent("  ")); err != nil {
		return fmt.Errorf("write json: %w", err)
	}

	if _, err := io.WriteString(w, "\n"); err != nil {
		return fmt.Errorf("write json: %w", err)
	}

	return nil
}

func requireName(example string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return withHint(errors.New("expected one tunnel name"), "try '"+example+"'")
		}

		return nil
	}
}
