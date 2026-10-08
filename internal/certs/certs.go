// Package certs obtains a wildcard certificate from Let's Encrypt with DNS-01 challenges
// that the user satisfies by creating TXT records by hand at their DNS provider.
package certs

import (
	"cmp"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/acme"
)

// StagingURL is Let's Encrypt's test CA: untrusted certificates, but no production rate limits.
const StagingURL = "https://acme-staging-v02.api.letsencrypt.org/directory"

const (
	accountKeyName  = "acme-account.key"
	certDirName     = "certs"
	challengePrefix = "_acme-challenge."
	dnsPollInterval = 5 * time.Second
	dnsWaitTimeout  = 30 * time.Minute
	acmeHTTPTimeout = 30 * time.Second
	dnsPort         = "53"
)

// Record is a TXT record the user must create.
type Record struct {
	Name  string
	Value string
}

// Options configures Issue.
type Options struct {
	// Records is called once with the TXT records to create.
	Records func([]Record)
	// Waiting is called on each check that still finds records missing.
	Waiting func(missing int)
	// LookupTXT resolves TXT records; nil queries the domain's own nameservers, bypassing caches.
	LookupTXT func(ctx context.Context, name string) ([]string, error)
	Domain    string
	// Dir holds the ACME account key and issued certificates.
	Dir string
	// DirectoryURL is the ACME directory; empty means Let's Encrypt production.
	DirectoryURL string
}

type challenge struct {
	chal     *acme.Challenge
	authzURL string
	record   Record
}

// Paths returns where the certificate chain and key for domain are stored under dir.
func Paths(dir, domain string) (string, string) {
	base := filepath.Join(dir, certDirName, domain)

	return base + ".crt", base + ".key"
}

// Issue obtains a certificate for domain and *.domain, saves it under opts.Dir, and returns the leaf.
func Issue(ctx context.Context, opts *Options) (*x509.Certificate, error) {
	client, err := newClient(ctx, opts)
	if err != nil {
		return nil, err
	}

	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(opts.Domain, "*."+opts.Domain))
	if err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	pending, err := pendingChallenges(ctx, client, order, opts.Domain)
	if err != nil {
		return nil, err
	}

	if err := solve(ctx, client, opts, pending); err != nil {
		return nil, err
	}

	order, err = client.WaitOrder(ctx, order.URI)
	if err != nil {
		return nil, fmt.Errorf("wait for order: %w", err)
	}

	return finalize(ctx, client, order, opts)
}

func newClient(ctx context.Context, opts *Options) (*acme.Client, error) {
	key, err := accountKey(opts.Dir)
	if err != nil {
		return nil, err
	}

	client := &acme.Client{
		Key:          key,
		DirectoryURL: cmp.Or(opts.DirectoryURL, acme.LetsEncryptURL),
		HTTPClient:   &http.Client{Timeout: acmeHTTPTimeout},
	}

	_, err = client.Register(ctx, &acme.Account{}, acme.AcceptTOS)
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("register acme account: %w", err)
	}

	return client, nil
}

// pendingChallenges returns the dns-01 challenge for every authorization not already valid.
func pendingChallenges(ctx context.Context, client *acme.Client, order *acme.Order, domain string) ([]challenge, error) {
	var pending []challenge

	for _, u := range order.AuthzURLs {
		authz, err := client.GetAuthorization(ctx, u)
		if err != nil {
			return nil, fmt.Errorf("get authorization: %w", err)
		}

		if authz.Status == acme.StatusValid {
			continue
		}

		i := slices.IndexFunc(authz.Challenges, func(c *acme.Challenge) bool { return c.Type == "dns-01" })
		if i < 0 {
			return nil, fmt.Errorf("the CA offered no dns-01 challenge for %s", authz.Identifier.Value)
		}

		value, err := client.DNS01ChallengeRecord(authz.Challenges[i].Token)
		if err != nil {
			return nil, fmt.Errorf("compute challenge record: %w", err)
		}

		pending = append(pending, challenge{
			chal:     authz.Challenges[i],
			authzURL: u,
			record:   Record{Name: challengePrefix + domain, Value: value},
		})
	}

	return pending, nil
}

// solve shows the records, waits for them to appear in DNS, and asks the CA to verify them.
func solve(ctx context.Context, client *acme.Client, opts *Options, pending []challenge) error {
	if len(pending) == 0 {
		return nil
	}

	records := make([]Record, 0, len(pending))
	for _, p := range pending {
		records = append(records, p.record)
	}

	if opts.Records != nil {
		opts.Records(records)
	}

	lookup := opts.LookupTXT
	if lookup == nil {
		var err error

		lookup, err = authoritativeLookup(ctx, opts.Domain)
		if err != nil {
			return err
		}
	}

	if err := waitForRecords(ctx, lookup, records, opts.Waiting); err != nil {
		return err
	}

	for _, p := range pending {
		if _, err := client.Accept(ctx, p.chal); err != nil {
			return fmt.Errorf("accept challenge: %w", err)
		}

		if _, err := client.WaitAuthorization(ctx, p.authzURL); err != nil {
			return fmt.Errorf("let's encrypt could not verify %s: %w", p.record.Name, err)
		}
	}

	return nil
}

// waitForRecords polls until every record's value is published.
func waitForRecords(ctx context.Context, lookup func(context.Context, string) ([]string, error), records []Record, waiting func(int)) error {
	ctx, cancel := context.WithTimeout(ctx, dnsWaitTimeout)
	defer cancel()

	ticker := time.NewTicker(dnsPollInterval)
	defer ticker.Stop()

	var lastErr error

	for {
		missing, err := countMissing(ctx, lookup, records)
		if err != nil {
			lastErr = err
		}

		if missing == 0 {
			return nil
		}

		if waiting != nil {
			waiting(missing)
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("txt records not found in dns: %w (last lookup: %w)", ctx.Err(), lastErr)
			}

			return fmt.Errorf("txt records not found in dns: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// errNoNameservers is returned when no name from the domain up to its parent zone has NS records.
var errNoNameservers = errors.New("no nameservers found")

// countMissing reports how many records are not yet published, and the last lookup failure if any.
func countMissing(ctx context.Context, lookup func(context.Context, string) ([]string, error), records []Record) (int, error) {
	var (
		missing int
		lastErr error
	)

	for _, r := range records {
		values, err := lookup(ctx, r.Name)
		if err != nil {
			lastErr = err
		}

		if !slices.Contains(values, r.Value) {
			missing++
		}
	}

	return missing, lastErr
}

// authoritativeLookup queries the zone's nameservers directly so cached negative answers don't delay us.
func authoritativeLookup(ctx context.Context, domain string) (func(context.Context, string) ([]string, error), error) {
	nss, err := zoneNameservers(ctx, net.DefaultResolver.LookupNS, domain)
	if err != nil {
		return nil, err
	}

	resolvers := make([]*net.Resolver, 0, len(nss))
	for _, ns := range nss {
		resolvers = append(resolvers, resolverFor(net.JoinHostPort(strings.TrimSuffix(ns.Host, "."), dnsPort)))
	}

	return func(ctx context.Context, name string) ([]string, error) {
		var errs []error

		// A not-found answer is authoritative; only an unreachable nameserver moves on to the next.
		for _, r := range resolvers {
			values, err := r.LookupTXT(ctx, name)
			if dnsErr, ok := errors.AsType[*net.DNSError](err); err == nil || (ok && dnsErr.IsNotFound) {
				return values, nil
			}

			errs = append(errs, err)
		}

		return nil, fmt.Errorf("lookup %s: %w", name, errors.Join(errs...))
	}, nil
}

// zoneNameservers walks up from domain to the closest name with NS records, since a subdomain usually has none of its own.
func zoneNameservers(ctx context.Context, lookupNS func(context.Context, string) ([]*net.NS, error), domain string) ([]*net.NS, error) {
	lastErr := errNoNameservers

	for name := domain; strings.Contains(name, "."); _, name, _ = strings.Cut(name, ".") {
		nss, err := lookupNS(ctx, name)
		if err == nil && len(nss) > 0 {
			return nss, nil
		}

		if err != nil {
			lastErr = err
		}
	}

	return nil, fmt.Errorf("find nameservers for %s: %w", domain, lastErr)
}

func resolverFor(server string) *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, network, server)
		},
	}
}

func finalize(ctx context.Context, client *acme.Client, order *acme.Order, opts *Options) (*x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key: %w", err)
	}

	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: opts.Domain},
		DNSNames: []string{opts.Domain, "*." + opts.Domain},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("create csr: %w", err)
	}

	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize order: %w", err)
	}

	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}

	if err := save(opts, key, chain); err != nil {
		return nil, err
	}

	return leaf, nil
}

// save writes the key before the chain, since the server reloads when the chain file changes.
func save(opts *Options, key *ecdsa.PrivateKey, chain [][]byte) error {
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode key: %w", err)
	}

	var certPEM []byte
	for _, der := range chain {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}

	certFile, keyFile := Paths(opts.Dir, opts.Domain)

	if err := writeAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return err
	}

	return writeAtomic(certFile, certPEM)
}

// accountKey loads the ACME account key from dir, creating one on first use.
func accountKey(dir string) (crypto.Signer, error) {
	path := filepath.Join(dir, accountKeyName)

	data, err := os.ReadFile(path)
	if err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("account key %s: not pem", path)
		}

		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("account key %s: %w", path, err)
		}

		return key, nil
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read account key: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate account key: %w", err)
	}

	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode account key: %w", err)
	}

	if err := writeAtomic(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})); err != nil {
		return nil, err
	}

	return key, nil
}

// writeAtomic writes data readable only by the owner, replacing path in one step.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	_, err = tmp.Write(data)

	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}

	if err != nil {
		return errors.Join(fmt.Errorf("write %s: %w", path, err), os.Remove(tmp.Name()))
	}

	return nil
}
