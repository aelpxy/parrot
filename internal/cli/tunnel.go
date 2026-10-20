package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/config"
	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/tunnel"
)

type tunnelOptions struct {
	name       string
	subdomain  string
	auth       string
	hostHeader string
	remotePort int
	detach     bool
}

func (o *tunnelOptions) spec(proto tunnel.Proto, target string) (tunnel.Spec, error) {
	spec := tunnel.Spec{
		Name:       o.name,
		Proto:      proto,
		Target:     target,
		Subdomain:  o.subdomain,
		Auth:       o.auth,
		HostHeader: o.hostHeader,
		RemotePort: o.remotePort,
	}

	if err := spec.Normalize(); err != nil {
		err = fmt.Errorf("%s tunnel: %w", proto, err)
		if errors.Is(err, tunnel.ErrInvalidTarget) {
			return tunnel.Spec{}, withHint(err, "use a port like 3000 or an address like 192.168.1.5:8080")
		}

		return tunnel.Spec{}, err
	}

	if spec.Name == "" {
		spec.Name = defaultName(&spec)
	}

	return spec, nil
}

func addCommonFlags(cmd *cobra.Command, opts *tunnelOptions) {
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "run in the background")
	cmd.Flags().StringVarP(&opts.name, "name", "n", "", "tunnel name (generated if omitted)")
}

func addHTTPFlags(cmd *cobra.Command, opts *tunnelOptions) {
	cmd.Flags().StringVarP(&opts.subdomain, "subdomain", "s", "", "request a subdomain")
	cmd.Flags().StringVar(&opts.auth, "auth", "", "require basic auth (user:pass)")
	cmd.Flags().StringVar(&opts.hostHeader, "host-header", "", "rewrite the Host header sent to your app")
}

func requireTarget(example string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		switch len(args) {
		case 0:
			return withHint(errors.New("missing target"), "try '"+example+"'")
		case 1:
			return nil
		}

		return withHint(errors.New("too many arguments"), "try '"+example+"'")
	}
}

func (a *App) httpCmd() *cobra.Command {
	var opts tunnelOptions

	cmd := &cobra.Command{
		Use:     "http <target>",
		Short:   "Expose a local HTTP service",
		GroupID: groupTunnels,
		Example: "  parrot http 3000\n  parrot http 192.168.1.5:8080 -s myapp",
		Args:    requireTarget("parrot http 3000"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.startTunnel(cmd.Context(), tunnel.HTTP, args[0], &opts)
		},
	}

	addCommonFlags(cmd, &opts)
	addHTTPFlags(cmd, &opts)

	return cmd
}

func (a *App) tcpCmd() *cobra.Command {
	var opts tunnelOptions

	cmd := &cobra.Command{
		Use:     "tcp <target>",
		Short:   "Expose a local TCP service",
		GroupID: groupTunnels,
		Example: "  parrot tcp 5432\n  parrot tcp 22 -p 35022 -d",
		Args:    requireTarget("parrot tcp 5432"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.startTunnel(cmd.Context(), tunnel.TCP, args[0], &opts)
		},
	}

	addCommonFlags(cmd, &opts)
	cmd.Flags().IntVarP(&opts.remotePort, "remote-port", "p", 0, "request a public port")

	return cmd
}

func (a *App) downCmd() *cobra.Command {
	var path string

	cmd := &cobra.Command{
		Use:     "down",
		Short:   "Stop background tunnels started from " + config.ProjectFile,
		GroupID: groupTunnels,
		Example: "  parrot down\n  parrot down -c staging.toml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.down(cmd.Context(), path)
		},
	}

	cmd.Flags().StringVarP(&path, "config", "c", config.ProjectFile, "config file the tunnels were started from")

	return cmd
}

func (a *App) udpCmd() *cobra.Command {
	var opts tunnelOptions

	cmd := &cobra.Command{
		Use:     "udp <target>",
		Short:   "Expose a local UDP service",
		GroupID: groupTunnels,
		Example: "  parrot udp 51820\n  parrot udp 27015 -p 37015 -d",
		Args:    requireTarget("parrot udp 51820"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.startTunnel(cmd.Context(), tunnel.UDP, args[0], &opts)
		},
	}

	addCommonFlags(cmd, &opts)
	cmd.Flags().IntVarP(&opts.remotePort, "remote-port", "p", 0, "request a public port")

	return cmd
}

func (a *App) upCmd() *cobra.Command {
	var (
		detach bool
		path   string
	)

	cmd := &cobra.Command{
		Use:   "up [name...]",
		Short: "Start tunnels from " + config.ProjectFile,
		Long: "Start tunnels from " + config.ProjectFile + ".\n\n" +
			"With -d, the background tunnels from the file are made to match it: new ones start, changed ones\n" +
			"restart, and ones removed from the file stop. Naming tunnels updates just those.",
		GroupID: groupTunnels,
		Example: "  parrot up\n  parrot up -d\n  parrot up api db -d",
		RunE: func(cmd *cobra.Command, args []string) error {
			specs, err := projectTunnels(path, args)
			if err != nil {
				return err
			}

			if !detach {
				return a.runTunnels(cmd.Context(), specs)
			}

			source, err := filepath.Abs(path)
			if err != nil {
				return fmt.Errorf("config path: %w", err)
			}

			return a.applyTunnels(cmd.Context(), path, &daemon.ApplyRequest{Source: source, Specs: specs, Prune: len(args) == 0})
		},
		ValidArgsFunction: func(_ *cobra.Command, _ []string, _ string) ([]cobra.Completion, cobra.ShellCompDirective) {
			return completeTunnelNames(path)
		},
	}

	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "run in the background")
	cmd.Flags().StringVarP(&path, "config", "c", config.ProjectFile, "config file")

	return cmd
}

// projectTunnels loads the config at path and returns the named tunnels, or all of them.
func projectTunnels(path string, names []string) ([]tunnel.Spec, error) {
	cfg, err := config.LoadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, withHint(fmt.Errorf("no %s found", path), "create one, or run 'parrot http <port>' directly")
	}

	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	if len(cfg.Tunnels) == 0 {
		return nil, withHint(fmt.Errorf("no tunnels defined in %s", path), "add one under [tunnels], such as 'web = 3000'")
	}

	if len(names) == 0 {
		return cfg.Tunnels, nil
	}

	specs := make([]tunnel.Spec, 0, len(names))

	for _, name := range names {
		i := slices.IndexFunc(cfg.Tunnels, func(s tunnel.Spec) bool { return s.Name == name })
		if i < 0 {
			return nil, fmt.Errorf("no tunnel named %q in %s", name, path)
		}

		specs = append(specs, cfg.Tunnels[i])
	}

	return specs, nil
}

func completeTunnelNames(path string) ([]cobra.Completion, cobra.ShellCompDirective) {
	cfg, err := config.LoadFile(path)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	names := make([]cobra.Completion, 0, len(cfg.Tunnels))
	for _, s := range cfg.Tunnels {
		names = append(names, cobra.CompletionWithDesc(s.Name, s.Proto.String()+" "+s.Target))
	}

	return names, cobra.ShellCompDirectiveNoFileComp
}
