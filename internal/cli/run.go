package cli

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/config"
	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/tunnel"
	"github.com/aelpxy/parrot/internal/ui"
)

const (
	defaultControlPort = "7000"
	envServer          = "PARROT_SERVER"
	envToken           = "PARROT_TOKEN"
	localServerCmd     = "parrot server --domain localhost --http :8080 --allow-anonymous"
)

func (a *App) startTunnel(ctx context.Context, proto tunnel.Proto, target string, opts *tunnelOptions) error {
	spec, err := opts.spec(proto, target)
	if err != nil {
		return err
	}

	if opts.detach {
		return a.detachTunnels(ctx, []tunnel.Spec{spec})
	}

	return a.runTunnels(ctx, []tunnel.Spec{spec})
}

// runTunnels runs specs in the foreground until ctx is canceled.
func (a *App) runTunnels(ctx context.Context, specs []tunnel.Spec) error {
	opts, err := a.clientOptions()
	if err != nil {
		return err
	}

	p := a.ui()
	opts.OnOnline = func(ts []client.Tunnel) { p.Online(tunnelRows(ts), "ctrl+c to stop") }
	opts.OnOffline = p.Offline
	opts.OnStreamError = func(name string, err error) {
		p.Warn(name + ": " + err.Error())
	}

	if err := client.Run(ctx, opts, specs); err != nil {
		return connectError(err)
	}

	return nil
}

// clientOptions resolves the server and token from the environment, then credentials, then the user config.
func (a *App) clientOptions() (*client.Options, error) {
	server, token, err := resolveServer()
	if err != nil {
		return nil, err
	}

	if server == "" {
		return nil, withHint(errors.New("not logged in"), "run 'parrot login <token> --server <host>', or set "+envServer)
	}

	opts := dialOptions(server, token)
	opts.Logger = a.logger(slog.LevelWarn)

	return opts, nil
}

func resolveServer() (string, string, error) {
	server, token := os.Getenv(envServer), os.Getenv(envToken)
	if server != "" && token != "" {
		return server, token, nil
	}

	creds, err := loadCredentials()
	if err != nil {
		return "", "", err
	}

	server, token = cmp.Or(server, creds.Server), cmp.Or(token, creds.Token)
	if server != "" && token != "" {
		return server, token, nil
	}

	cfg, err := loadUserConfig()
	if err != nil {
		return "", "", err
	}

	return cmp.Or(server, cfg.Server), cmp.Or(token, cfg.Token), nil
}

// dialOptions adds the default control port and enables TLS for any server not on loopback.
func dialOptions(server, token string) *client.Options {
	host, port, err := net.SplitHostPort(server)
	if err != nil {
		host, port = server, defaultControlPort
	}

	opts := &client.Options{Server: net.JoinHostPort(host, port), Token: token}
	if !isLoopback(host) {
		opts.TLS = &tls.Config{ServerName: host, MinVersion: tls.VersionTLS13}
	}

	return opts
}

func isLoopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

func loadCredentials() (config.Credentials, error) {
	path, err := config.CredentialsPath()
	if err != nil {
		return config.Credentials{}, fmt.Errorf("credentials: %w", err)
	}

	creds, err := config.LoadCredentials(path)
	if errors.Is(err, fs.ErrNotExist) {
		return config.Credentials{}, nil
	}

	if err != nil {
		return config.Credentials{}, fmt.Errorf("credentials: %w", err)
	}

	return creds, nil
}

func loadUserConfig() (*config.Config, error) {
	path, err := config.UserPath()
	if err != nil {
		return nil, fmt.Errorf("user config: %w", err)
	}

	cfg, err := config.LoadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &config.Config{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("user config: %w", err)
	}

	return cfg, nil
}

// logger logs styled lines to stderr at level, or at debug with --debug.
func (a *App) logger(level slog.Level) *slog.Logger {
	if a.debug {
		level = slog.LevelDebug
	}

	return a.ui().Logger(level)
}

// connectError adds a next step to errors from reaching or authenticating with the server.
func connectError(err error) error {
	var remote *protocol.RemoteError

	switch {
	case errors.As(err, &remote) && strings.Contains(remote.Message, "token"):
		return withHint(fmt.Errorf("server refused: %w", err), "log in again with a token from 'parrot server token create': 'parrot login <token>'")
	case errors.As(err, &remote):
		return fmt.Errorf("server refused: %w", err)
	}

	return withHint(err, "is the server running? to test locally: '"+localServerCmd+"'")
}

func tunnelRows(ts []client.Tunnel) []ui.TunnelRow {
	rows := make([]ui.TunnelRow, 0, len(ts))
	for _, t := range ts {
		rows = append(rows, ui.TunnelRow{Name: t.Spec.Name, URL: t.URL, Target: t.Spec.Target})
	}

	return rows
}

// defaultName names an unnamed tunnel after its protocol and local port, e.g. "http-3000".
func defaultName(s *tunnel.Spec) string {
	_, port, err := net.SplitHostPort(s.Target)
	if err != nil {
		return s.Proto.String()
	}

	return s.Proto.String() + "-" + port
}
