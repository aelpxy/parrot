package client_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/aelpxy/parrot/internal/server"
	"github.com/aelpxy/parrot/internal/tunnel"
)

const (
	continuePath = "/.well-known/parrot/continue"
	browserHTML  = "text/html,application/xhtml+xml,*/*;q=0.8"
	warningText  = "run by someone else"
)

// result is what came back from a visit, with the body already read and closed.
type result struct {
	header http.Header
	body   string
	status int
}

type visit struct {
	method  string
	path    string
	headers map[string]string
	form    url.Values
}

// do sends v to the tunnel at publicURL without following redirects.
func (h *harness) do(t *testing.T, publicURL string, v visit) result {
	t.Helper()

	u, err := url.Parse(publicURL)
	if err != nil {
		t.Fatal(err)
	}

	var body io.Reader
	if v.form != nil {
		body = strings.NewReader(v.form.Encode())
	}

	req, err := http.NewRequestWithContext(t.Context(), cmpOr(v.method, http.MethodGet), "http://"+h.httpAddr+cmpOr(v.path, "/"), body)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = u.Host

	if v.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	for k, val := range v.headers {
		req.Header.Set(k, val)
	}

	c := &http.Client{Timeout: h.http.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return result{header: resp.Header, body: string(b), status: resp.StatusCode}
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}

	return s
}

// echoApp reports the cookies and skip header it receives, so tests can check parrot strips its own.
func echoApp(t *testing.T) string {
	t.Helper()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "app cookie="+strconv.Quote(r.Header.Get("Cookie"))+" skip="+strconv.Quote(r.Header.Get("Parrot-Skip-Warning")))
	}))
	t.Cleanup(app.Close)

	return strings.TrimPrefix(app.URL, "http://")
}

func TestWarningForAnonymousTunnels(t *testing.T) {
	t.Parallel()

	h := startServer(t)
	publicURL := h.startClient(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: echoApp(t)})["web"].URL
	origin := "http://" + strings.TrimPrefix(publicURL, "http://")

	tests := []struct {
		name       string
		visit      visit
		wantStatus int
		wantBody   string
	}{
		{name: "browser page load", visit: visit{headers: map[string]string{"Accept": browserHTML}}, wantStatus: http.StatusOK, wantBody: warningText},
		{name: "api request", visit: visit{headers: map[string]string{"Accept": "application/json"}}, wantStatus: http.StatusOK, wantBody: "app"},
		{name: "webhook post", visit: visit{method: http.MethodPost, headers: map[string]string{"Accept": browserHTML}}, wantStatus: http.StatusOK, wantBody: "app"},
		{name: "skip header", visit: visit{headers: map[string]string{"Accept": browserHTML, "Parrot-Skip-Warning": "1"}}, wantStatus: http.StatusOK, wantBody: `skip=""`},
		{name: "continue by link", visit: visit{path: continuePath}, wantStatus: http.StatusForbidden},
		{name: "continue without origin", visit: visit{method: http.MethodPost, path: continuePath, form: url.Values{"next": {"/"}}}, wantStatus: http.StatusForbidden},
		{name: "continue from another site", visit: visit{method: http.MethodPost, path: continuePath, form: url.Values{"next": {"/"}}, headers: map[string]string{"Origin": "https://evil.example"}}, wantStatus: http.StatusForbidden},
		{name: "cookie lets browser through", visit: visit{headers: map[string]string{"Accept": browserHTML, "Cookie": "a=1; parrot_warning_seen=1; b=2"}}, wantStatus: http.StatusOK, wantBody: `cookie="a=1; b=2"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := h.do(t, publicURL, tt.visit)
			if got.status != tt.wantStatus || !strings.Contains(got.body, tt.wantBody) {
				t.Errorf("got %d %q, want %d containing %q", got.status, got.body, tt.wantStatus, tt.wantBody)
			}
		})
	}

	t.Run("continue sets cookie and stays on host", func(t *testing.T) {
		t.Parallel()

		for next, want := range map[string]string{"/docs?x=1": "/docs?x=1", "https://evil.example/": "/", "//evil.example": "/"} {
			got := h.do(t, publicURL, visit{method: http.MethodPost, path: continuePath, form: url.Values{"next": {next}}, headers: map[string]string{"Origin": origin}})
			if got.status != http.StatusSeeOther || got.header.Get("Location") != want {
				t.Errorf("next %q: got %d to %q, want 303 to %q", next, got.status, got.header.Get("Location"), want)
			}

			if !strings.Contains(got.header.Get("Set-Cookie"), "parrot_warning_seen=1") {
				t.Errorf("next %q: Set-Cookie = %q, want the warning cookie", next, got.header.Get("Set-Cookie"))
			}
		}
	})
}

func TestNoWarningForTokenTunnels(t *testing.T) {
	t.Parallel()

	h := startServerWith(t, &server.Options{Auth: fakeAuth{"prt_good": "laptop"}, AllowAnonymous: true})
	h.token = "prt_good"

	publicURL := h.startClient(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: echoApp(t)})["web"].URL

	got := h.do(t, publicURL, visit{headers: map[string]string{"Accept": browserHTML}})
	if got.status != http.StatusOK || !strings.HasPrefix(got.body, "app") {
		t.Errorf("got %d %q, want the app without a warning", got.status, got.body)
	}
}

func TestNoWarningWhenDisabled(t *testing.T) {
	t.Parallel()

	h := startServerWith(t, &server.Options{AllowAnonymous: true, NoWarning: true})
	publicURL := h.startClient(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: echoApp(t)})["web"].URL

	got := h.do(t, publicURL, visit{headers: map[string]string{"Accept": browserHTML}})
	if got.status != http.StatusOK || !strings.HasPrefix(got.body, "app") {
		t.Errorf("got %d %q, want the app without a warning", got.status, got.body)
	}
}
