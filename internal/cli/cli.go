// Package cli defines the parrot command tree.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/tunnel"
	"github.com/aelpxy/parrot/internal/ui"
)

const (
	groupTunnels = "tunnels"
	groupAccount = "account"
	groupHosting = "hosting"

	suggestDistance = 2

	// exitAborted matches the shell convention for a command interrupted with ctrl+c.
	exitAborted = 130
)

// App is the parrot command line.
type App struct {
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	root    *cobra.Command
	printer *ui.Printer
	noColor bool
	debug   bool
}

// hintedError attaches a suggested next step to an error.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string { return e.err.Error() }
func (e *hintedError) Unwrap() error { return e.err }
func (e *hintedError) Hint() string  { return e.hint }

func withHint(err error, hint string) error {
	return &hintedError{err: err, hint: hint}
}

// New builds the command tree, reading prompts from stdin and writing output to stdout and stderr.
func New(stdin io.Reader, stdout, stderr io.Writer) *App {
	a := &App{stdin: stdin, stdout: stdout, stderr: stderr}

	a.root = a.rootCmd()
	a.root.SetOut(stdout)
	a.root.SetErr(stderr)

	return a
}

// Run executes args and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	a.root.SetArgs(args)

	err := a.root.ExecuteContext(ctx)
	if errors.Is(err, ui.ErrAborted) {
		return exitAborted
	}

	if err != nil {
		a.ui().Error(err)

		return 1
	}

	return 0
}

// ui returns the shared printer, created on first use so it sees the parsed --no-color flag.
func (a *App) ui() *ui.Printer {
	if a.printer == nil {
		a.printer = ui.New(a.stdout, a.stderr, ui.Options{NoColor: a.noColor})
	}

	return a.printer
}

func (a *App) rootCmd() *cobra.Command {
	var opts tunnelOptions

	cmd := &cobra.Command{
		Use:   "parrot [port]",
		Short: "Expose local services to the internet",
		Example: `  parrot              pick a local port to expose
  parrot 3000         expose localhost:3000 over HTTP
  parrot tcp 5432     expose a TCP service
  parrot up -d        start every tunnel in parrot.toml in the background`,
		Version:                    version(),
		Args:                       rootArgs,
		SuggestionsMinimumDistance: suggestDistance,
		SilenceErrors:              true,
		SilenceUsage:               true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return a.pick(cmd.Context(), &opts)
			}

			return a.startTunnel(cmd.Context(), tunnel.HTTP, args[0], &opts)
		},
	}

	cmd.SetHelpFunc(a.help)
	cmd.SetVersionTemplate(versionLine() + "\n")

	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return withHint(err, fmt.Sprintf("run '%s --help' for usage", cmd.CommandPath()))
	})

	cmd.PersistentFlags().BoolVar(&a.noColor, "no-color", false, "disable colored output")
	cmd.PersistentFlags().BoolVar(&a.debug, "debug", false, "log debug details to stderr")
	cmd.Flags().BoolP("version", "v", false, "print version information")
	addCommonFlags(cmd, &opts)
	addHTTPFlags(cmd, &opts)

	cmd.AddGroup(
		&cobra.Group{ID: groupTunnels, Title: "tunnels"},
		&cobra.Group{ID: groupAccount, Title: "account"},
		&cobra.Group{ID: groupHosting, Title: "hosting"},
	)

	cmd.AddCommand(
		a.httpCmd(), a.tcpCmd(), a.upCmd(), a.lsCmd(), a.logsCmd(), a.stopCmd(),
		a.loginCmd(), a.logoutCmd(),
		a.serverCmd(), a.serviceCmd(), a.daemonCmd(),
		versionCmd(),
	)

	return cmd
}

// rootArgs accepts a single target and otherwise reports an unknown command with suggestions.
func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}

	if _, err := tunnel.ParseTarget(args[0]); err != nil {
		return unknownCommand(cmd, args[0])
	}

	if len(args) > 1 {
		return withHint(errors.New("too many arguments"), "try 'parrot 3000'")
	}

	return nil
}

func unknownCommand(cmd *cobra.Command, name string) error {
	err := fmt.Errorf("unknown command %q", name)

	if s := cmd.SuggestionsFor(name); len(s) > 0 {
		return withHint(err, fmt.Sprintf("did you mean 'parrot %s'?", s[0]))
	}

	return withHint(err, "run 'parrot --help' to see all commands")
}
