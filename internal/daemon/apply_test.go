package daemon_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/daemon"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const projectFile = "/work/app/parrot.toml"

func (e *env) apply(t *testing.T, prune bool, specs ...tunnel.Spec) (*daemon.ApplyResult, error) {
	t.Helper()

	return e.client.Apply(t.Context(), &daemon.ApplyRequest{Source: projectFile, Server: e.server, Specs: specs, Prune: prune})
}

func mustApply(t *testing.T, e *env, prune bool, specs ...tunnel.Spec) *daemon.ApplyResult {
	t.Helper()

	res, err := e.apply(t, prune, specs...)
	if err != nil {
		t.Fatalf("Apply() error: %v", err)
	}

	return res
}

func checkResult(t *testing.T, step string, got []string, want ...string) {
	t.Helper()

	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", step, got, want)
	}
}

func TestApplyReconciles(t *testing.T) {
	t.Parallel()

	e := setup(t)
	web := tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: e.app, Subdomain: "app"}
	db := tunnel.Spec{Name: "db", Proto: tunnel.TCP, Target: e.app}

	res := mustApply(t, e, true, web, db)
	checkResult(t, "first apply started", res.Started, "web", "db")

	res = mustApply(t, e, true, web, db)
	checkResult(t, "same apply unchanged", res.Unchanged, "web", "db")
	checkResult(t, "same apply started", res.Started)

	// Changing web restarts it, and it must win back its subdomain from the old copy.
	changed := web
	changed.HostHeader = "app.local"

	res = mustApply(t, e, true, changed)
	checkResult(t, "edit restarted", res.Restarted, "web")
	checkResult(t, "removal stopped", res.Stopped, "db")

	if len(res.Tunnels) != 1 || !strings.HasPrefix(res.Tunnels[0].URL, "http://app.parrot.test") {
		t.Errorf("restarted tunnels = %+v, want web back on app.parrot.test", res.Tunnels)
	}

	res = mustApply(t, e, true)
	checkResult(t, "empty apply stopped", res.Stopped, "web")
}

func TestApplyLeavesOtherSourcesAlone(t *testing.T) {
	t.Parallel()

	e := setup(t)

	// A tunnel started directly from the command line, outside any config file.
	if _, err := e.start(t, tunnel.Spec{Name: "adhoc", Proto: tunnel.HTTP, Target: e.app}); err != nil {
		t.Fatalf("Start() error: %v", err)
	}

	if _, err := e.apply(t, true, tunnel.Spec{Name: "adhoc", Proto: tunnel.HTTP, Target: e.app}); !isStatus(err, http.StatusConflict) {
		t.Errorf("Apply() over another source's tunnel error = %v, want a conflict", err)
	}

	res := mustApply(t, e, true)
	checkResult(t, "pruning an empty project stopped", res.Stopped)

	infos, err := e.client.List(t.Context())
	if err != nil || len(infos) != 1 || infos[0].Name != "adhoc" {
		t.Errorf("List() = %+v, %v; want the ad hoc tunnel still running", infos, err)
	}
}

func TestApplyWithoutPruneKeepsOthers(t *testing.T) {
	t.Parallel()

	e := setup(t)
	web := tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: e.app}
	db := tunnel.Spec{Name: "db", Proto: tunnel.TCP, Target: e.app}

	mustApply(t, e, true, web, db)

	// Like 'parrot up -d web': only the named tunnel is reconciled.
	res := mustApply(t, e, false, web)
	checkResult(t, "partial apply stopped", res.Stopped)

	infos, err := e.client.List(t.Context())
	if err != nil || len(infos) != 2 {
		t.Errorf("List() = %+v, %v; want both tunnels still running", infos, err)
	}
}

func TestApplyRestartKeepsURL(t *testing.T) {
	t.Parallel()

	e := setup(t)
	web := tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: e.app}

	first := mustApply(t, e, true, web)

	// An edit that doesn't touch the address must not hand out a new random URL.
	web.HostHeader = "app.local"

	second := mustApply(t, e, true, web)
	checkResult(t, "edit restarted", second.Restarted, "web")

	if first.Tunnels[0].URL != second.Tunnels[0].URL {
		t.Errorf("URL changed on restart: %s -> %s", first.Tunnels[0].URL, second.Tunnels[0].URL)
	}

	// The carried-over name must not make the next identical apply look like a change.
	third := mustApply(t, e, true, web)
	checkResult(t, "repeat unchanged", third.Unchanged, "web")
}
