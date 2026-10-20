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
	warning   *warning
	user      string
	pass      string
	auth      bool
}

// newHTTPTunnel proxies to b's client tunnel; a non-nil wn shows browser visitors a warning first.
func newHTTPTunnel(mux *yamux.Session, b *protocol.Bind, logger *slog.Logger, wn *warning) *httpTunnel {
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

			if wn != nil {
				stripWarning(pr.Out.Header)
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

	return &httpTunnel{proxy: proxy, transport: transport, warning: wn, user: user, pass: pass, auth: b.Auth != ""}
}

func (t *httpTunnel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The warning comes before any login prompt so visitors see it before typing credentials.
	if t.warning != nil && t.warning.intercept(w, r) {
		return
	}

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

func (s *Server) redirectHTTPS(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://"+hostOnly(r.Host)+s.publicPort+r.URL.RequestURI(), http.StatusMovedPermanently)
}

// ServeHTTP routes public requests to the tunnel matching the Host header.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.ToLower(hostOnly(r.Host)) == s.domain && r.URL.Path == "/" {
		s.serveLanding(w)

		return
	}

	t := s.lookupHost(r.Host)
	if t == nil {
		writePage(w, http.StatusNotFound, "No tunnel is running at "+r.Host+".")

		return
	}

	t.ServeHTTP(w, r)
}

func (s *Server) serveLanding(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	// The client may already be gone; there is no one left to report a failed write to.
	_, _ = w.Write(s.landing)
}

func writePage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)

	// The client may already be gone; there is no one left to report a failed write to.
	_, _ = w.Write([]byte("parrot: " + msg + "\n"))
}
