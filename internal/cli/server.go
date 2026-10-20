package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/aelpxy/parrot/internal/certs"
	"github.com/aelpxy/parrot/internal/server"
	"github.com/aelpxy/parrot/internal/token"
)

const (
	defaultControlAddr = ":" + defaultControlPort
	defaultHTTPAddr    = ":80"
	defaultHTTPSAddr   = ":443"
	defaultTCPPorts    = "30000-40000"
	rootDataDir        = "/var/lib/parrot"
)

type serverOptions struct {
	domain         string
	control        string
	http           string
	https          string
	tcpPorts       string
	tlsCert        string
	tlsKey         string
	dataDir        string
	allowAnonymous bool
	noWarning      bool
}

func (a *App) serverCmd() *cobra.Command {
	var opts serverOptions

	cmd := &cobra.Command{
		Use:     "server",
		Short:   "Run the public forwarding server",
		GroupID: groupHosting,
		Example: `  parrot server --domain example.com --tls-cert fullchain.pem --tls-key privkey.pem
  parrot server --domain localhost --http :8080 --allow-anonymous`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runServer(cmd.Context(), &opts)
		},
	}

	f := cmd.Flags()
	f.StringVar(&opts.domain, "domain", "", "base domain for HTTP tunnels (required)")
	f.StringVar(&opts.control, "listen", defaultControlAddr, "control address clients connect to")
	f.StringVar(&opts.http, "http", defaultHTTPAddr, "public HTTP address (redirects to HTTPS when TLS is on)")
	f.StringVar(&opts.https, "https", defaultHTTPSAddr, "public HTTPS address, used when TLS is on")
	f.StringVar(&opts.tcpPorts, "tcp-ports", defaultTCPPorts, "port range for TCP tunnels")
	f.StringVar(&opts.tlsCert, "tls-cert", "", "certificate for the domain and *.domain (uses the one from 'parrot server cert' if omitted)")
	f.StringVar(&opts.tlsKey, "tls-key", "", "private key for --tls-cert")
	f.BoolVar(&opts.allowAnonymous, "allow-anonymous", false, "accept clients without a token")
	f.BoolVar(&opts.noWarning, "no-warning", false, "don't show browser visitors of anonymous tunnels a warning page first")
	cmd.MarkFlagsRequiredTogether("tls-cert", "tls-key")

	cmd.PersistentFlags().StringVar(&opts.dataDir, "data-dir", defaultDataDir(), "directory for tokens")

	cmd.AddCommand(a.tokenCmd(&opts.dataDir), a.certCmd(&opts.dataDir))

	return cmd
}

func (a *App) runServer(ctx context.Context, opts *serverOptions) error {
	if opts.domain == "" {
		return withHint(errors.New("missing --domain"), "to test locally: '"+localServerCmd+"'")
	}

	ports, err := server.ParsePortRange(opts.tcpPorts)
	if err != nil {
		return fmt.Errorf("--tcp-ports: %w", err)
	}

	logger := a.logger(slog.LevelInfo)

	certFile, keyFile := opts.tlsCert, opts.tlsKey
	if certFile == "" {
		certFile, keyFile = issuedCert(opts.dataDir, opts.domain)
	}

	var tlsConfig *tls.Config

	if certFile != "" {
		tlsConfig, err = server.LoadTLS(certFile, keyFile, logger)
		if err != nil {
			return fmt.Errorf("tls certificate: %w", err)
		}
	}

	store := &token.Store{Dir: opts.dataDir}
	warnAuth(logger, store, opts.allowAnonymous)

	srv, err := server.New(&server.Options{
		Logger:         logger,
		Auth:           store,
		TLS:            tlsConfig,
		Domain:         opts.domain,
		ControlAddr:    opts.control,
		HTTPAddr:       opts.http,
		HTTPSAddr:      opts.https,
		TCPPorts:       ports,
		AllowAnonymous: opts.allowAnonymous,
		NoWarning:      opts.noWarning,
	})
	if err != nil {
		return fmt.Errorf("server: %w", err)
	}

	if err := srv.Run(ctx); err != nil {
		err = fmt.Errorf("server: %w", err)
		if errors.Is(err, unix.EACCES) {
			return withHint(err, "ports below 1024 need root or CAP_NET_BIND_SERVICE; try '--http :8080 --https :8443'")
		}

		return err
	}

	return nil
}

// issuedCert returns the certificate saved by 'parrot server cert' for domain, if there is one.
func issuedCert(dataDir, domain string) (string, string) {
	certFile, keyFile := certs.Paths(dataDir, domain)
	if _, err := os.Stat(certFile); err != nil {
		return "", ""
	}

	return certFile, keyFile
}

// warnAuth points out configurations where clients can connect without a token, or not at all.
func warnAuth(logger *slog.Logger, store *token.Store, allowAnonymous bool) {
	if allowAnonymous {
		logger.Warn("anonymous clients are allowed; anyone who can reach this server can open tunnels")

		return
	}

	tokens, err := store.List()
	if err == nil && len(tokens) == 0 {
		logger.Warn("no tokens yet, so no client can connect; create one with 'parrot server token create'")
	}
}

func (a *App) tokenCmd(dataDir *string) *cobra.Command {
	var name string

	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage client tokens",
	}

	create := &cobra.Command{
		Use:   "create",
		Short: "Create a client token",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.createToken(&token.Store{Dir: *dataDir}, name)
		},
	}
	create.Flags().StringVarP(&name, "name", "n", "", "label to recognize the token by")

	list := &cobra.Command{
		Use:   "ls",
		Short: "List client tokens",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return a.listTokens(&token.Store{Dir: *dataDir})
		},
	}

	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a client token",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := (&token.Store{Dir: *dataDir}).Revoke(args[0]); err != nil {
				return withHint(fmt.Errorf("revoke: %w", err), "run 'parrot server token ls' to see token ids")
			}

			a.ui().Success("revoked token " + args[0])
			a.ui().Note("clients already connected with it stay connected until they reconnect")

			return nil
		},
	}

	cmd.AddCommand(create, list, revoke)

	return cmd
}

func (a *App) createToken(store *token.Store, name string) error {
	t, secret, err := store.Create(name)
	if err != nil {
		return fmt.Errorf("create token: %w", err)
	}

	p := a.ui()
	p.Success("created token " + t.ID)
	p.Highlight(secret,
		"connect with 'parrot login "+secret+" --server <your domain>'",
		"this is the only time the token is shown",
	)

	return nil
}

func (a *App) listTokens(store *token.Store) error {
	tokens, err := store.List()
	if err != nil {
		return fmt.Errorf("list tokens: %w", err)
	}

	if len(tokens) == 0 {
		a.ui().Note("no tokens yet", "create one with 'parrot server token create --name laptop'")

		return nil
	}

	rows := make([][]string, 0, len(tokens))
	for _, t := range tokens {
		rows = append(rows, []string{t.ID, t.Name, t.CreatedAt.Local().Format(time.DateTime)})
	}

	a.ui().Table([]string{"ID", "NAME", "CREATED"}, rows)

	return nil
}

// defaultDataDir prefers systemd's StateDirectory, then /var/lib for root, then the user's state dir.
func defaultDataDir() string {
	if dir := os.Getenv("STATE_DIRECTORY"); dir != "" {
		return dir
	}

	if os.Geteuid() == 0 {
		return rootDataDir
	}

	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
		return filepath.Join(dir, "parrot")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return rootDataDir
	}

	return filepath.Join(home, ".local", "state", "parrot")
}
