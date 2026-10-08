package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/ui"
)

const (
	noTunnels    = "no background tunnels"
	startOneHint = "start one with 'parrot http 3000 -d'"
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
