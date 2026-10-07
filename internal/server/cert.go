package server

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

const (
	certCheckInterval = time.Minute
	expiryWarnWindow  = 21 * 24 * time.Hour
	expiryWarnEvery   = 24 * time.Hour
)

// certReloader serves a key pair from disk and picks up renewals without a restart.
type certReloader struct {
	logger   *slog.Logger
	certFile string
	keyFile  string

	mu       sync.Mutex
	cert     *tls.Certificate
	modTime  time.Time
	checked  time.Time
	warnedAt time.Time
}

// LoadTLS returns a TLS config serving certFile and keyFile, reloaded when certFile changes.
func LoadTLS(certFile, keyFile string, logger *slog.Logger) (*tls.Config, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	r := &certReloader{logger: logger, certFile: certFile, keyFile: keyFile}

	info, err := os.Stat(certFile)
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}

	if err := r.load(info.ModTime()); err != nil {
		return nil, err
	}

	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.certificate}, nil
}

func (r *certReloader) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if time.Since(r.checked) < certCheckInterval {
		return r.cert, nil
	}

	r.checked = time.Now()
	r.warnExpiry()

	info, err := os.Stat(r.certFile)
	if err != nil || !info.ModTime().After(r.modTime) {
		return r.cert, nil
	}

	if err := r.load(info.ModTime()); err != nil {
		r.logger.Error("reload tls certificate failed, keeping the current one", "error", err)
	}

	return r.cert, nil
}

func (r *certReloader) load(modTime time.Time) error {
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return fmt.Errorf("load tls key pair: %w", err)
	}

	r.cert, r.modTime, r.checked, r.warnedAt = &cert, modTime, time.Now(), time.Time{}
	r.logger.Info("tls certificate loaded", "cert_file", r.certFile, "expires", cert.Leaf.NotAfter.Format(time.DateOnly))
	r.warnExpiry()

	return nil
}

// warnExpiry logs at most daily once the certificate is close to expiring.
func (r *certReloader) warnExpiry() {
	if r.cert.Leaf == nil || time.Until(r.cert.Leaf.NotAfter) > expiryWarnWindow || time.Since(r.warnedAt) < expiryWarnEvery {
		return
	}

	r.warnedAt = time.Now()
	r.logger.Warn("tls certificate expires soon; renew it with 'parrot server cert'",
		"expires", r.cert.Leaf.NotAfter.Format(time.DateOnly), "cert_file", r.certFile)
}
