package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"sync"

	"github.com/aelpxy/parrot/internal/tunnel"
)

// connection is the server a tunnel runs against; changing it restarts the tunnel.
type connection struct {
	server string
	token  string
	tls    bool
}

// applyPlan is what an apply will do, worked out before anything changes. launch is what each new
// tunnel runs with, and requested is how its config file wrote it, which later applies compare against.
type applyPlan struct {
	result    ApplyResult
	launch    []tunnel.Spec
	requested []tunnel.Spec
	retire    []*managed
}

func (d *Daemon) handleApply(ctx context.Context, wg *sync.WaitGroup, w http.ResponseWriter, r *http.Request) {
	var req ApplyRequest
	if err := json.UnmarshalRead(http.MaxBytesReader(w, r.Body, maxRequestBytes), &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())

		return
	}

	result, started, err := d.apply(ctx, wg, &req)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())

		return
	}

	if err := waitOnline(ctx, started); err != nil {
		d.stop(started...)
		writeError(w, http.StatusBadGateway, err.Error())

		return
	}

	result.Tunnels = d.infos(req.Specs)
	writeJSON(w, http.StatusOK, result)
}

// apply stops what is retired or replaced before launching replacements, so a restarted tunnel can
// reclaim its subdomain from the server.
func (d *Daemon) apply(ctx context.Context, wg *sync.WaitGroup, req *ApplyRequest) (*ApplyResult, []*managed, error) {
	plan, err := d.plan(req)
	if err != nil {
		return nil, nil, err
	}

	d.stop(plan.retire...)

	if len(plan.launch) == 0 {
		return &plan.result, nil, nil
	}

	started, err := d.start(ctx, wg, &StartRequest{Source: req.Source, Server: req.Server, Token: req.Token, Specs: plan.launch, TLS: req.TLS})
	if err != nil {
		return nil, nil, err
	}

	d.mu.Lock()

	for i, m := range started {
		m.spec = plan.requested[i]
	}

	d.mu.Unlock()

	return &plan.result, started, nil
}

// plan compares req with what runs and takes every tunnel it will replace or stop out of the table.
// It changes nothing when a listed name belongs to another source.
func (d *Daemon) plan(req *ApplyRequest) (*applyPlan, error) {
	for i, s := range req.Specs {
		if slices.ContainsFunc(req.Specs[:i], func(o tunnel.Spec) bool { return o.Name == s.Name }) {
			return nil, fmt.Errorf("duplicate tunnel name %q", s.Name)
		}
	}

	conn := connection{server: req.Server, token: req.Token, tls: req.TLS}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, s := range req.Specs {
		if m, ok := d.tunnels[s.Name]; ok && m.info.Source != req.Source {
			return nil, fmt.Errorf("a tunnel named %q is already running from somewhere else", s.Name)
		}
	}

	p := &applyPlan{}

	for _, s := range req.Specs {
		m, running := d.tunnels[s.Name]

		switch {
		case running && m.spec == s && m.conn == conn:
			p.result.Unchanged = append(p.result.Unchanged, s.Name)

			continue
		case running:
			p.result.Restarted = append(p.result.Restarted, s.Name)
			p.retire = append(p.retire, m)
			p.launch = append(p.launch, m.keepAddress(&s))
			p.requested = append(p.requested, s)

			delete(d.tunnels, s.Name)

			continue
		default:
			p.result.Started = append(p.result.Started, s.Name)
		}

		p.launch = append(p.launch, s)
		p.requested = append(p.requested, s)
	}

	if req.Prune {
		d.prune(req, p)
	}

	return p, nil
}

// prune retires tunnels from req's source that req no longer lists. The caller holds d.mu.
func (d *Daemon) prune(req *ApplyRequest, p *applyPlan) {
	for name, m := range d.tunnels {
		listed := slices.ContainsFunc(req.Specs, func(s tunnel.Spec) bool { return s.Name == name })
		if m.info.Source != req.Source || listed {
			continue
		}

		p.result.Stopped = append(p.result.Stopped, name)
		p.retire = append(p.retire, m)

		delete(d.tunnels, name)
	}

	slices.Sort(p.result.Stopped)
}

// infos returns the current state of the named tunnels that are running.
func (d *Daemon) infos(specs []tunnel.Spec) []TunnelInfo {
	infos := make([]TunnelInfo, 0, len(specs))

	for _, s := range specs {
		if m := d.lookup(s.Name); m != nil {
			infos = append(infos, m.snapshot())
		}
	}

	return infos
}
