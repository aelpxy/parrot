package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	warningCookie     = "parrot_warning_seen"
	warningCookieAge  = 7 * 24 * time.Hour
	warningSkipHeader = "Parrot-Skip-Warning"

	// warningContinuePath lives under .well-known so it is unlikely to collide with a tunneled app's routes.
	warningContinuePath = "/.well-known/parrot/continue"
)

//go:embed warning.html
var warningHTML string

// warning shows anonymous tunnels' browser visitors who is behind the page before letting them through.
type warning struct {
	page *template.Template
}

func newWarning() (*warning, error) {
	page, err := template.New("warning").Parse(warningHTML)
	if err != nil {
		return nil, fmt.Errorf("parse warning page: %w", err)
	}

	return &warning{page: page}, nil
}

// intercept answers r itself when the visitor still has to see the warning, reporting whether it did.
func (wn *warning) intercept(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == warningContinuePath {
		wn.accept(w, r)

		return true
	}

	if !needsWarning(r) {
		return false
	}

	wn.show(w, r)

	return true
}

// needsWarning is true for browser page loads that have not been through the warning yet.
// Fetches, webhooks, and CLI clients don't ask for HTML, so they pass straight through.
func needsWarning(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	if r.Header.Get(warningSkipHeader) != "" || !strings.Contains(r.Header.Get("Accept"), "text/html") {
		return false
	}

	_, err := r.Cookie(warningCookie)

	return err != nil
}

func (wn *warning) show(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer

	err := wn.page.Execute(&buf, struct {
		Host   string
		Action string
		Next   string
	}{hostOnly(r.Host), warningContinuePath, r.URL.RequestURI()})
	if err != nil {
		writePage(w, http.StatusInternalServerError, "could not show this page.")

		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	// Framing would let another site trick visitors into clicking through.
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	w.WriteHeader(http.StatusOK)

	// The client may already be gone; there is no one left to report a failed write to.
	_, _ = w.Write(buf.Bytes())
}

// accept records that the visitor chose to continue, then sends them where they were going.
func (wn *warning) accept(w http.ResponseWriter, r *http.Request) {
	// Only the warning page itself may set the cookie; a link or another site's form must not skip it.
	if r.Method != http.MethodPost || !sameOrigin(r) {
		writePage(w, http.StatusForbidden, "open the page from its link to continue.")

		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     warningCookie,
		Value:    "1",
		Path:     "/",
		MaxAge:   int(warningCookieAge / time.Second),
		Secure:   r.TLS != nil,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	http.Redirect(w, r, safeNext(r.PostFormValue("next")), http.StatusSeeOther)
}

// sameOrigin reports whether the browser says the request came from a page on the same host.
func sameOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	if err != nil || origin.Host == "" {
		return false
	}

	return strings.EqualFold(origin.Host, r.Host)
}

// safeNext keeps the redirect on this host, so the form cannot be used as an open redirect.
func safeNext(next string) string {
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, `/\`) {
		return "/"
	}

	return next
}

// stripWarning removes what only parrot uses, so the tunneled app never sees it.
func stripWarning(h http.Header) {
	h.Del(warningSkipHeader)

	cookies := h.Values("Cookie")
	if len(cookies) == 0 {
		return
	}

	kept := make([]string, 0, len(cookies))

	for _, line := range cookies {
		for part := range strings.SplitSeq(line, ";") {
			part = strings.TrimSpace(part)
			if part != "" && !strings.HasPrefix(part, warningCookie+"=") {
				kept = append(kept, part)
			}
		}
	}

	h.Del("Cookie")

	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}
