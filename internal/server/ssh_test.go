package server_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/aelpxy/parrot/internal/server"
)

const waitTimeout = 5 * time.Second

var urlPattern = regexp.MustCompile(`(http|https|tcp)://[a-z0-9.-]+(:\d+)?`)

type fakeAuth map[string]string

func (f fakeAuth) Authenticate(token string) (string, error) {
	if label, ok := f[token]; ok {
		return label, nil
	}

	return "", errors.New("unknown token")
}

func hostKey(t *testing.T) ssh.Signer {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return signer
}

// syncBuffer collects a session's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

// sshUser connects as user and opens an interactive session whose output it returns.
func sshUser(t *testing.T, addr, user string, key ssh.PublicKey) (*ssh.Client, *syncBuffer) {
	t.Helper()

	client, err := ssh.Dial("tcp", addr, &ssh.ClientConfig{User: user, HostKeyCallback: ssh.FixedHostKey(key), Timeout: waitTimeout})
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}

	out := &syncBuffer{}
	sess.Stdout = out

	if err := sess.Shell(); err != nil {
		t.Fatal(err)
	}

	return client, out
}

// waitForURL returns the first tunnel URL the session prints.
func waitForURL(t *testing.T, out *syncBuffer) string {
	t.Helper()

	deadline := time.Now().Add(waitTimeout)

	for time.Now().Before(deadline) {
		if u := urlPattern.FindString(out.String()); u != "" {
			return u
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("no tunnel URL in session output: %q", out.String())

	return ""
}

func TestSSHHTTPTunnel(t *testing.T) {
	t.Parallel()

	key := hostKey(t)
	sshLn, public := listen(t), listen(t)
	serve(t, &server.Options{AllowAnonymous: true, SSHHostKey: key}, server.Listeners{Control: listen(t), HTTP: public, SSH: sshLn})

	// ssh sends the local username when none is given; unknown words like it are ignored.
	client, out := sshUser(t, sshLn.Addr().String(), "someone+qr", key.PublicKey())

	app, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("remote forward: %v", err)
	}

	go func() {
		_ = http.Serve(app, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "hello from ssh")
		}))
	}()

	publicURL := waitForURL(t, out)

	if !strings.Contains(out.String(), "█") {
		t.Errorf("session output has no QR code: %q", out.String())
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+public.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = strings.TrimPrefix(publicURL, "http://")

	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK || string(body) != "hello from ssh" {
		t.Errorf("got %d %q through %s, want the app", resp.StatusCode, body, publicURL)
	}
}

func TestSSHTCPTunnel(t *testing.T) {
	t.Parallel()

	key := hostKey(t)
	sshLn := listen(t)
	serve(t, &server.Options{AllowAnonymous: true, SSHHostKey: key}, server.Listeners{Control: listen(t), HTTP: listen(t), SSH: sshLn})

	client, out := sshUser(t, sshLn.Addr().String(), "tcp", key.PublicKey())

	echo, err := client.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("remote forward: %v", err)
	}

	go serveEcho(echo)

	_, port, err := net.SplitHostPort(strings.TrimPrefix(waitForURL(t, out), "tcp://"))
	if err != nil {
		t.Fatal(err)
	}

	d := net.Dialer{Timeout: waitTimeout}

	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(waitTimeout)); err != nil {
		t.Fatal(err)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, len("ping"))
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo = %q, %v; want %q", buf, err, "ping")
	}
}

func serveEcho(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		go func() {
			defer func() { _ = conn.Close() }()

			_, _ = io.Copy(conn, conn)
		}()
	}
}

func TestSSHRejectsBadToken(t *testing.T) {
	t.Parallel()

	key := hostKey(t)
	sshLn := listen(t)
	serve(t, &server.Options{Auth: fakeAuth{"prt_good": "laptop"}, SSHHostKey: key}, server.Listeners{Control: listen(t), HTTP: listen(t), SSH: sshLn})

	var banner string

	_, err := ssh.Dial("tcp", sshLn.Addr().String(), &ssh.ClientConfig{
		User:            "prt_nope",
		HostKeyCallback: ssh.FixedHostKey(key.PublicKey()),
		BannerCallback:  func(msg string) error { banner = msg; return nil },
		Timeout:         waitTimeout,
	})
	if err == nil {
		t.Fatal("ssh dial with a bad token succeeded")
	}

	if !strings.Contains(banner, "invalid token") {
		t.Errorf("banner = %q, want it to explain the rejection", banner)
	}
}

func TestSSHSharesHTTPSPort(t *testing.T) {
	t.Parallel()

	key := hostKey(t)
	tlsConfig, roots := testTLS(t)
	https := listen(t)
	serve(t, &server.Options{AllowAnonymous: true, TLS: tlsConfig, SSHHostKey: key, SSHOnHTTPS: true}, server.Listeners{Control: listen(t), HTTP: listen(t), HTTPS: https})

	client, out := sshUser(t, https.Addr().String(), "someone", key.PublicKey())

	if _, err := client.Listen("tcp", "127.0.0.1:0"); err != nil {
		t.Fatalf("remote forward over the https port: %v", err)
	}

	waitForURL(t, out)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+https.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = testDomain

	c := &http.Client{Timeout: requestTimeout, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: testDomain, MinVersion: tls.VersionTLS13}}}

	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("https on the shared port: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "-R0:localhost:3000 "+testDomain) {
		t.Errorf("landing page = %d %q, want it to show the ssh command", resp.StatusCode, body)
	}
}

// testTLS returns a server config with a self-signed certificate for the test domain, and a pool trusting it.
func testTLS(t *testing.T) (*tls.Config, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: testDomain},
		DNSNames:     []string{testDomain, "*." + testDomain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
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

	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS13}, roots
}
