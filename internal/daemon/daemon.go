package daemon

import (
	"context"
	"crypto/tls"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const (
	startTimeout      = 20 * time.Second
	shutdownTimeout   = 5 * time.Second
	readHeaderTimeout = 5 * time.Second
	maxRequestBytes   = 1 << 20
)

// Daemon runs background tunnels.
type Daemon struct {
	logger *slog.Logger

	mu      sync.Mutex
	tunnels map[string]*managed
}

// managed is one background tunnel and the goroutine running it.
type managed struct {
	log    *logbook
	cancel context.CancelFunc
	online chan struct{}
	done   chan struct{}

	// spec and conn are what the tunnel was started with, so an apply can tell whether it changed.
	spec tunnel.Spec
	conn connection

	mu   sync.Mutex
	info TunnelInfo
	err  error
	// assigned is the subdomain and port the server gave, which a restart asks for again.
	assigned tunnel.Spec
}

// New returns an empty Daemon.
func New(logger *slog.Logger) *Daemon {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return &Daemon{logger: logger, tunnels: make(map[string]*managed)}
}

// Serve answers API requests on ln until ctx is canceled or a client asks it to shut down,
// then stops every tunnel.
func (d *Daemon) Serve(ctx context.Context, ln net.Listener) error {
	var wg sync.WaitGroup

	ctx, shutdown := context.WithCancel(ctx)
	defer shutdown()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /tunnels", d.handleList)
	mux.HandleFunc("POST /tunnels", func(w http.ResponseWriter, r *http.Request) {
		d.handleStart(ctx, &wg, w, r)
	})
	mux.HandleFunc("POST /apply", func(w http.ResponseWriter, r *http.Request) {
		d.handleApply(ctx, &wg, w, r)
	})
	mux.HandleFunc("DELETE /tunnels", d.handleStopAll)
	mux.HandleFunc("DELETE /tunnels/{name}", d.handleStop)
	mux.HandleFunc("GET /tunnels/{name}/logs", d.handleLogs)
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		d.logger.Info("shutdown requested")
		w.WriteHeader(http.StatusNoContent)
		shutdown()
	})

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
		ErrorLog:          slog.NewLogLogger(d.logger.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	d.logger.Info("daemon listening", "socket", ln.Addr().String())

	errc := make(chan error, 1)

	go func() { errc <- srv.Serve(ln) }()

	var serveErr error

	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	err := srv.Shutdown(shutdownCtx)

	d.stopAll()
	wg.Wait()

	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}

	if err := errors.Join(serveErr, err); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	return nil
}

func (d *Daemon) handleStart(ctx context.Context, wg *sync.WaitGroup, w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxRequestBytes), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())

		return
	}

	started, err := d.start(ctx, wg, &req)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())

		return
	}

	if err := waitOnline(ctx, started); err != nil {
		d.stop(started...)
		writeError(w, http.StatusBadGateway, err.Error())

		return
	}

	infos := make([]TunnelInfo, 0, len(started))
	for _, m := range started {
		infos = append(infos, m.snapshot())
	}

	writeJSON(w, http.StatusCreated, infos)
}

// start registers and launches every spec atomically, failing if any name is already running.
func (d *Daemon) start(ctx context.Context, wg *sync.WaitGroup, req *StartRequest) ([]*managed, error) {
	specs := req.Specs
	if len(specs) == 0 {
		return nil, errors.New("no tunnels to start")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for i, s := range specs {
		if _, running := d.tunnels[s.Name]; running {
			return nil, fmt.Errorf("a tunnel named %q is already running", s.Name)
		}

		if slices.ContainsFunc(specs[:i], func(o tunnel.Spec) bool { return o.Name == s.Name }) {
			return nil, fmt.Errorf("duplicate tunnel name %q", s.Name)
		}
	}

	started := make([]*managed, 0, len(specs))

	for _, s := range specs {
		m := &managed{
			log:    newLogbook(),
			online: make(chan struct{}),
			done:   make(chan struct{}),
			spec:   s,
			conn:   connection{server: req.Server, token: req.Token, tls: req.TLS},
			info: TunnelInfo{
				StartedAt: time.Now(),
				Name:      s.Name,
				Source:    req.Source,
				Target:    s.Target,
				Proto:     s.Proto,
				Status:    StatusStarting,
			},
		}
		d.launch(ctx, wg, &s, req, m)
		d.tunnels[s.Name] = m
		started = append(started, m)
	}

	return started, nil
}

// launch starts m's goroutine; callers hold d.mu so the tunnel is never visible half-started.
func (d *Daemon) launch(ctx context.Context, wg *sync.WaitGroup, spec *tunnel.Spec, req *StartRequest, m *managed) {
	logger := d.logger.With("tunnel", spec.Name)

	ctx, m.cancel = context.WithCancel(ctx)

	var once sync.Once

	opts := &client.Options{
		Logger: logger,
		Server: req.Server,
		Token:  req.Token,
		TLS:    tlsFor(req),
		OnOnline: func(ts []client.Tunnel) {
			m.setOnline(&ts[0])
			logger.Info("tunnel online", "url", ts[0].URL)
			once.Do(func() { close(m.online) })
		},
		OnOffline: func(err error, retryIn time.Duration) {
			m.setReconnecting(err, retryIn)
			logger.Warn("tunnel offline", "error", err, "retry_in", retryIn)
		},
		OnStreamError: func(_ string, err error) {
			m.log.add(slog.LevelWarn, err.Error())
		},
	}

	m.log.add(slog.LevelInfo, "starting "+spec.Proto.String()+" tunnel to "+spec.Target)

	wg.Go(func() {
		defer close(m.done)

		err := client.Run(ctx, opts, []tunnel.Spec{*spec})

		m.mu.Lock()
		m.err = err
		m.mu.Unlock()

		if err != nil {
			m.log.add(slog.LevelError, err.Error())
		}
	})
}

// waitOnline waits until every tunnel is online, or returns the first startup failure.
func waitOnline(ctx context.Context, started []*managed) error {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	for _, m := range started {
		select {
		case <-m.online:
		case <-m.done:
			return fmt.Errorf("tunnel %s: %w", m.info.Name, m.runErr())
		case <-ctx.Done():
			return fmt.Errorf("tunnel %s: timed out waiting for the server", m.info.Name)
		}
	}

	return nil
}

func (d *Daemon) handleList(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()

	infos := make([]TunnelInfo, 0, len(d.tunnels))
	for _, m := range d.tunnels {
		infos = append(infos, m.snapshot())
	}

	d.mu.Unlock()

	slices.SortFunc(infos, func(a, b TunnelInfo) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, infos)
}

func (d *Daemon) handleStop(w http.ResponseWriter, r *http.Request) {
	m := d.lookup(r.PathValue("name"))
	if m == nil {
		writeError(w, http.StatusNotFound, "no background tunnel named "+r.PathValue("name"))

		return
	}

	d.stop(m)
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) handleStopAll(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.stopAll())
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	m := d.lookup(r.PathValue("name"))
	if m == nil {
		writeError(w, http.StatusNotFound, "no background tunnel named "+r.PathValue("name"))

		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")

	enc := jsontext.NewEncoder(w)
	rc := http.NewResponseController(w)

	if r.URL.Query().Get("follow") != "true" {
		writeLines(enc, m.log.snapshot())

		return
	}

	backlog, lines, stop := m.log.follow()
	defer stop()

	writeLines(enc, backlog)

	for {
		if err := rc.Flush(); err != nil {
			return
		}

		select {
		case line := <-lines:
			writeLines(enc, []LogLine{line})
		case <-m.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (d *Daemon) lookup(name string) *managed {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.tunnels[name]
}

// stop cancels the given tunnels, waits for them to exit, and forgets them.
func (d *Daemon) stop(ms ...*managed) {
	d.mu.Lock()

	for _, m := range ms {
		if d.tunnels[m.info.Name] == m {
			delete(d.tunnels, m.info.Name)
		}
	}

	d.mu.Unlock()

	for _, m := range ms {
		m.cancel()
		<-m.done

		if m.snapshot().Status == StatusStarting {
			d.logger.Debug("discarded tunnel that never came online", "tunnel", m.info.Name)

			continue
		}

		d.logger.Info("tunnel stopped", "tunnel", m.info.Name)
	}
}

// stopAll stops every tunnel and returns their names.
func (d *Daemon) stopAll() []string {
	d.mu.Lock()

	ms := make([]*managed, 0, len(d.tunnels))
	names := make([]string, 0, len(d.tunnels))

	for name, m := range d.tunnels {
		ms = append(ms, m)
		names = append(names, name)
	}

	d.mu.Unlock()

	d.stop(ms...)
	slices.Sort(names)

	return names
}

func (m *managed) snapshot() TunnelInfo {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.info
}

func (m *managed) runErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err == nil {
		return errors.New("stopped before coming online")
	}

	return m.err
}

func (m *managed) setOnline(t *client.Tunnel) {
	m.mu.Lock()
	m.info.Status, m.info.URL, m.info.LastError = StatusOnline, t.URL, ""
	m.assigned = t.Spec
	m.mu.Unlock()

	m.log.add(slog.LevelInfo, "online at "+t.URL)
}

// keepAddress fills in the subdomain or port the server gave m, so a restarted tunnel keeps its URL.
// Anything spec asks for itself, or a change of protocol, wins.
func (m *managed) keepAddress(spec *tunnel.Spec) tunnel.Spec {
	m.mu.Lock()
	defer m.mu.Unlock()

	kept := *spec
	if kept.Proto != m.assigned.Proto {
		return kept
	}

	if kept.Subdomain == "" {
		kept.Subdomain = m.assigned.Subdomain
	}

	if kept.RemotePort == 0 {
		kept.RemotePort = m.assigned.RemotePort
	}

	return kept
}

func (m *managed) setReconnecting(err error, retryIn time.Duration) {
	msg := "connection lost, retrying in " + retryIn.String()
	if err != nil {
		msg += ": " + err.Error()
	}

	m.mu.Lock()
	m.info.Status, m.info.LastError = StatusReconnecting, msg
	m.mu.Unlock()

	m.log.add(slog.LevelWarn, msg)
}

func tlsFor(req *StartRequest) *tls.Config {
	if !req.TLS {
		return nil
	}

	host, _, err := net.SplitHostPort(req.Server)
	if err != nil {
		host = req.Server
	}

	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS13}
}

func writeLines(enc *jsontext.Encoder, lines []LogLine) {
	for _, l := range lines {
		if err := json.MarshalEncode(enc, l); err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// The CLI on the other end may have gone away; nothing useful can be done about it here.
	_ = json.MarshalWrite(w, v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}
