package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/config"
)

func (a *App) loginCmd() *cobra.Command {
	var server string

	cmd := &cobra.Command{
		Use:     "login <token>",
		Short:   "Verify and save your auth token",
		GroupID: groupAccount,
		Example: "  parrot login prt_... --server tunnel.example.com",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return withHint(errors.New("missing token"), "try 'parrot login prt_... --server tunnel.example.com'")
			}

			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.login(cmd.Context(), args[0], server)
		},
	}

	cmd.Flags().StringVar(&server, "server", "", "server host[:port] (defaults to the last server you logged in to)")

	return cmd
}

func (a *App) login(ctx context.Context, token, server string) error {
	path, err := config.CredentialsPath()
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}

	if server == "" {
		creds, err := loadCredentials()
		if err != nil {
			return err
		}

		server = creds.Server
	}

	if server == "" {
		return withHint(errors.New("missing --server"), "try 'parrot login "+token+" --server tunnel.example.com'")
	}

	if err := client.Check(ctx, dialOptions(server, token)); err != nil {
		return connectError(fmt.Errorf("verify token with %s: %w", server, err))
	}

	if err := config.SaveCredentials(path, config.Credentials{Server: server, Token: token}); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}

	a.ui().Success("logged in to " + server)

	return nil
}

func (a *App) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "logout",
		Short:   "Remove saved credentials",
		GroupID: groupAccount,
		Args:    cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			path, err := config.CredentialsPath()
			if err != nil {
				return fmt.Errorf("credentials: %w", err)
			}

			err = os.Remove(path)
			if errors.Is(err, fs.ErrNotExist) {
				a.ui().Note("already logged out")

				return nil
			}

			if err != nil {
				return fmt.Errorf("remove credentials: %w", err)
			}

			a.ui().Success("logged out")

			return nil
		},
	}
}
