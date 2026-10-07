package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/service"
	"github.com/aelpxy/parrot/internal/ui"
)

const serverInstallExample = "sudo parrot service install --server -- --domain example.com " +
	"--tls-cert /etc/letsencrypt/live/example.com/fullchain.pem --tls-key /etc/letsencrypt/live/example.com/privkey.pem"

func (a *App) serviceCmd() *cobra.Command {
	var server bool

	cmd := &cobra.Command{
		Use:     "service",
		Short:   "Run parrot at login or boot",
		GroupID: groupHosting,
	}

	cmd.PersistentFlags().BoolVar(&server, "server", false, "manage the system-wide server instead of your tunnel daemon")

	unit := func(args []string) (service.Unit, error) {
		if server {
			return a.serverUnit(args)
		}

		return daemonUnit(args)
	}

	install := &cobra.Command{
		Use:     "install [-- server flags]",
		Short:   "Install and start the service",
		Example: "  parrot service install\n  " + serverInstallExample,
		RunE: func(cmd *cobra.Command, args []string) error {
			u, err := unit(args)
			if err != nil {
				return err
			}

			return a.installService(cmd.Context(), u)
		},
	}

	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.uninstallService(cmd.Context(), placeholderUnit(server))
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether the service is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serviceStatus(cmd.Context(), placeholderUnit(server))
		},
	}

	cmd.AddCommand(install, uninstall, status)

	return cmd
}

func daemonUnit(args []string) (service.Unit, error) {
	if len(args) > 0 {
		return service.Unit{}, withHint(errors.New("extra arguments are only used with --server"), "try 'parrot service install'")
	}

	exe, err := installableBinary()
	if err != nil {
		return service.Unit{}, err
	}

	return service.DaemonUnit(exe), nil
}

// serverUnit validates the server flags passed after "--" before they are baked into a unit.
func (a *App) serverUnit(args []string) (service.Unit, error) {
	if os.Geteuid() != 0 {
		return service.Unit{}, withHint(errors.New("installing the server needs root"), "try '"+serverInstallExample+"'")
	}

	probe := a.serverCmd()
	if err := probe.ParseFlags(args); err != nil {
		return service.Unit{}, withHint(fmt.Errorf("server flags: %w", err), "see 'parrot server --help'")
	}

	if domain, _ := probe.Flags().GetString("domain"); domain == "" {
		return service.Unit{}, withHint(errors.New("missing --domain for the server"), "try '"+serverInstallExample+"'")
	}

	exe, err := installableBinary()
	if err != nil {
		return service.Unit{}, err
	}

	return service.ServerUnit(exe, args), nil
}

// placeholderUnit identifies a unit for status and uninstall, where its command line does not matter.
func placeholderUnit(server bool) service.Unit {
	if server {
		return service.ServerUnit("parrot", nil)
	}

	return service.DaemonUnit("parrot")
}

// installableBinary returns this binary's resolved path, refusing temporary locations a unit would outlive.
func installableBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate parrot binary: %w", err)
	}

	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	if strings.HasPrefix(exe, os.TempDir()+string(filepath.Separator)) || strings.Contains(exe, "go-build") {
		return "", withHint(fmt.Errorf("parrot is running from a temporary location (%s)", exe), "install it somewhere permanent first, like '/usr/local/bin/parrot'")
	}

	return exe, nil
}

func (a *App) installService(ctx context.Context, u service.Unit) error {
	if err := service.Available(); err != nil {
		return withHint(err, "on WSL, enable systemd in /etc/wsl.conf; elsewhere, run 'parrot daemon' under your own supervisor")
	}

	if !u.System {
		if err := handOverDaemon(ctx); err != nil {
			return err
		}
	}

	m := &service.Manager{}

	if err := m.Install(ctx, u); err != nil {
		return fmt.Errorf("install service: %w", err)
	}

	path, err := m.Path(u)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}

	p := a.ui()

	if u.System {
		p.Success("installed and started the parrot server as a system service")
		p.Note("unit "+path, "follow its logs with '"+service.LogsCommand(u)+"'", "create a client token with 'sudo parrot server token create --name laptop'")

		return nil
	}

	p.Success("installed and started the parrot daemon as a user service")
	p.Note("unit "+path, "tunnels marked 'autostart = true' in your config start with it", "follow its logs with '"+service.LogsCommand(u)+"'")

	if !service.Lingering() {
		p.Note("it runs while you're logged in", "to keep it running after you log out: 'sudo loginctl enable-linger "+os.Getenv("USER")+"'")
	}

	return nil
}

// handOverDaemon stops an idle daemon started by -d so the service can take over its socket.
func handOverDaemon(ctx context.Context) error {
	c, err := runningDaemon()
	if err != nil {
		return err
	}

	infos, err := c.List(ctx)
	if errors.Is(err, daemon.ErrNotRunning) {
		return nil
	}

	if err != nil {
		return daemonError(err)
	}

	if len(infos) > 0 {
		return withHint(
			fmt.Errorf("%d background tunnel(s) are running outside the service", len(infos)),
			"stop them with 'parrot stop --all', then mark the ones you want with 'autostart = true' in your config",
		)
	}

	if err := c.Shutdown(ctx); err != nil {
		return fmt.Errorf("stop background daemon: %w", err)
	}

	return nil
}

func (a *App) uninstallService(ctx context.Context, u service.Unit) error {
	if err := service.Available(); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	if u.System && os.Geteuid() != 0 {
		return withHint(errors.New("removing the server service needs root"), "try 'sudo parrot service uninstall --server'")
	}

	m := &service.Manager{}

	st, err := m.Status(ctx, u)
	if err != nil {
		return fmt.Errorf("service status: %w", err)
	}

	if !st.Installed {
		a.ui().Note(serviceLabel(u) + " service is not installed")

		return nil
	}

	if err := m.Uninstall(ctx, u); err != nil {
		return fmt.Errorf("uninstall service: %w", err)
	}

	a.ui().Success("removed the " + serviceLabel(u) + " service")

	return nil
}

func (a *App) serviceStatus(ctx context.Context, u service.Unit) error {
	if err := service.Available(); err != nil {
		return fmt.Errorf("service: %w", err)
	}

	m := &service.Manager{}

	st, err := m.Status(ctx, u)
	if err != nil {
		return fmt.Errorf("service status: %w", err)
	}

	install := "install it with 'parrot service install'"
	if u.System {
		install = "install it with '" + serverInstallExample + "'"
	}

	if !st.Installed {
		a.ui().Note(serviceLabel(u)+" service is not installed", install)

		return nil
	}

	path, err := m.Path(u)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}

	a.ui().Service(&ui.ServiceState{
		Name:    serviceLabel(u),
		State:   st.Active + " (" + st.Sub + ")",
		Healthy: st.Running(),
		Details: statusDetails(st),
		Fields: []ui.HelpRow{
			{Name: "unit", Description: path},
			{Name: "logs", Description: "'" + service.LogsCommand(u) + "'"},
		},
	})

	return nil
}

func statusDetails(st service.Status) []string {
	var details []string

	if st.PID > 0 {
		details = append(details, "pid "+strconv.Itoa(st.PID))
	}

	if !st.Since.IsZero() && st.Running() {
		details = append(details, "up "+shortDuration(time.Since(st.Since)))
	}

	if st.Enabled {
		details = append(details, "starts automatically")
	} else {
		details = append(details, "not enabled")
	}

	return details
}

func serviceLabel(u service.Unit) string {
	if u.System {
		return "parrot server"
	}

	return "parrot daemon"
}
