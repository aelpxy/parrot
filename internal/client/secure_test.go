package client_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aelpxy/parrot/internal/client"
	"github.com/aelpxy/parrot/internal/protocol"
	"github.com/aelpxy/parrot/internal/server"
	"github.com/aelpxy/parrot/internal/tunnel"
)

type fakeAuth map[string]string

func (f fakeAuth) Authenticate(token string) (string, error) {
	if label, ok := f[token]; ok {
		return label, nil
	}

	return "", errors.New("unknown token")
}

func TestAuth(t *testing.T) {
	t.Parallel()

	h := startServerWith(t, &server.Options{Auth: fakeAuth{"prt_good": "laptop"}})
	spec := []tunnel.Spec{{Name: "web", Proto: tunnel.HTTP, Target: "localhost:1"}}

	tests := []struct {
		name    string
		token   string
		wantErr string
	}{
		{name: "valid", token: "prt_good"},
		{name: "invalid", token: "prt_bad", wantErr: "invalid token"},
		{name: "missing", wantErr: "missing token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := &client.Options{Server: h.controlAddr, Token: tt.token}

			err := client.Check(t.Context(), opts)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Check() error: %v", err)
				}

				return
			}

			var remote *protocol.RemoteError
			if !errors.As(err, &remote) || remote.Message != tt.wantErr {
				t.Errorf("Check() error = %v, want %q", err, tt.wantErr)
			}

			if err := client.Run(t.Context(), opts, spec); !errors.As(err, &remote) {
				t.Errorf("Run() error = %v, want rejection", err)
			}
		})
	}
}

func TestTLS(t *testing.T) {
	t.Parallel()

	serverTLS, roots := selfSigned(t)

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "secure "+r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(app.Close)

	h := startServerWith(t, &server.Options{AllowAnonymous: true, TLS: serverTLS})
	h.clientTLS = &tls.Config{ServerName: testDomain, RootCAs: roots, MinVersion: tls.VersionTLS13}

	tunnels := h.startClient(t, tunnel.Spec{Name: "web", Proto: tunnel.HTTP, Target: strings.TrimPrefix(app.URL, "http://"), Subdomain: "myapp"})

	publicURL := tunnels["web"].URL
	if !strings.HasPrefix(publicURL, "https://myapp."+testDomain+":") {
		t.Fatalf("URL = %q, want https", publicURL)
	}

	httpsClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer

				return d.DialContext(ctx, network, h.httpsAddr)
			},
		},
	}

	t.Run("https", func(t *testing.T) {
		t.Parallel()

		status, body := fetch(t, httpsClient, publicURL+"/")
		if status != http.StatusOK || body != "secure https" {
			t.Errorf("got %d %q, want 200 %q", status, body, "secure https")
		}
	})

	t.Run("http redirects", func(t *testing.T) {
		t.Parallel()

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+h.httpAddr+"/path?q=1", nil)
		if err != nil {
			t.Fatal(err)
		}

		req.Host = "myapp." + testDomain

		plain := &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}

		resp, err := plain.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()

		_, port, _ := net.SplitHostPort(h.httpsAddr)
		want := "https://myapp." + testDomain + ":" + port + "/path?q=1"

		if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != want {
			t.Errorf("got %d Location %q, want 301 %q", resp.StatusCode, resp.Header.Get("Location"), want)
		}
	})

	t.Run("plaintext control rejected", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()

		if err := client.Check(ctx, &client.Options{Server: h.controlAddr}); err == nil {
			t.Error("Check() over plaintext succeeded against a TLS server")
		}
	})
}

func fetch(t *testing.T, c *http.Client, url string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}

// selfSigned returns a server config for testDomain and *.testDomain, and a pool trusting it.
func selfSigned(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: testDomain},
		DNSNames:              []string{testDomain, "*." + testDomain},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(cert)

	cfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}

	return cfg, roots
}
