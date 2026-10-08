package server

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/hashicorp/yamux"

	"github.com/aelpxy/parrot/internal/protocol"
)

const maxIdleStreams = 32

// httpTunnel reverse-proxies public requests over streams to one client tunnel.
type httpTunnel struct {
	proxy     *httputil.ReverseProxy
	transport *http.Transport
	user      string
	pass      string
	auth      bool
}

func newHTTPTunnel(mux *yamux.Session, b *protocol.Bind, logger *slog.Logger) *httpTunnel {
	logger = logger.With("tunnel", b.Name)

	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return openStream(mux, protocol.StreamHeader{Tunnel: b.Name})
		},
		MaxIdleConnsPerHost: maxIdleStreams,
		IdleConnTimeout:     idleTimeout,
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: b.Name})
			pr.SetXForwarded()

			pr.Out.Host = pr.In.Host
			if b.HostHeader != "" {
				pr.Out.Host = b.HostHeader
			}

			// The visitor's credentials are for the tunnel, not the local app.
			if b.Auth != "" {
				pr.Out.Header.Del("Authorization")
			}
		},
		Transport: transport,
		ErrorLog:  slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			logger.Debug("proxy request failed", "error", err)
			writePage(w, http.StatusBadGateway, "the tunnel is not reachable right now.")
		},
	}

	user, pass, _ := strings.Cut(b.Auth, ":")

	return &httpTunnel{proxy: proxy, transport: transport, user: user, pass: pass, auth: b.Auth != ""}
}

func (t *httpTunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if t.auth && !t.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Basic realm="parrot", charset="UTF-8"`)
		writePage(w, http.StatusUnauthorized, "authentication required.")

		return
	}

	t.proxy.ServeHTTP(w, r)
}

func (t *httpTunnel) authorized(r *http.Request) bool {
	user, pass, ok := r.BasicAuth()
	if !ok {
		return false
	}

	userOK := subtle.ConstantTimeCompare([]byte(user), []byte(t.user))
	passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(t.pass))

	return userOK&passOK == 1
}

func (t *httpTunnel) close() {
	t.transport.CloseIdleConnections()
}
