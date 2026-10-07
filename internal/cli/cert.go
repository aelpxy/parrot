package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/certs"
)

// renewAhead is how long before expiry we suggest renewing; Let's Encrypt certificates last 90 days.
const renewAhead = 30 * 24 * time.Hour

func (a *App) certCmd(dataDir *string) *cobra.Command {
	var (
		domain  string
		staging bool
	)

	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Get or renew the wildcard TLS certificate",
		Long: "Get or renew a Let's Encrypt certificate for the domain and *.domain.\n" +
			"You add two TXT records at your DNS provider by hand; parrot waits until they are live.",
		Example: "  sudo parrot server cert --domain example.com\n  sudo parrot server cert --domain example.com --staging",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if domain == "" {
				return withHint(errors.New("missing --domain"), "try 'parrot server cert --domain example.com'")
			}

			directoryURL := ""
			if staging {
				directoryURL = certs.StagingURL
			}

			return a.issueCert(cmd.Context(), *dataDir, domain, directoryURL)
		},
	}

	cmd.Flags().StringVar(&domain, "domain", "", "base domain; the certificate covers it and *.domain (required)")
	cmd.Flags().BoolVar(&staging, "staging", false, "use Let's Encrypt's staging CA to test without rate limits (untrusted cert)")

	return cmd
}

// issueCert runs the manual DNS-01 flow against directoryURL ("" for Let's Encrypt production).
func (a *App) issueCert(ctx context.Context, dataDir, domain, directoryURL string) error {
	p := a.ui()
	opts := &certs.Options{Domain: domain, Dir: dataDir, DirectoryURL: directoryURL}

	opts.Records = func(records []certs.Record) {
		rows := make([][]string, 0, len(records))
		for _, r := range records {
			rows = append(rows, []string{"TXT", r.Name, r.Value})
		}

		p.Note("add these DNS records at your DNS provider; parrot checks for them automatically")
		p.Table([]string{"TYPE", "NAME", "VALUE"}, rows)
	}

	waitingShown := false
	opts.Waiting = func(missing int) {
		if !waitingShown {
			p.Note("waiting for " + strconv.Itoa(missing) + " record(s) to show up in DNS (this can take a minute; ctrl+c to stop)")

			waitingShown = true
		}
	}

	leaf, err := certs.Issue(ctx, opts)
	if err != nil {
		return fmt.Errorf("issue certificate: %w", err)
	}

	certFile, _ := certs.Paths(dataDir, domain)
	renewBy := leaf.NotAfter.Add(-renewAhead).Format(time.DateOnly)

	p.Success("certificate issued for " + domain + " and *." + domain + ", valid until " + leaf.NotAfter.Format(time.DateOnly))
	p.Note("saved to "+certFile,
		"'parrot server' uses it automatically and reloads it after renewals",
		"renew before "+renewBy+" with 'parrot server cert --domain "+domain+"'",
		"you can delete the TXT records now",
	)

	if directoryURL == certs.StagingURL {
		p.Warn("this is a staging certificate that browsers won't trust; rerun without '--staging' for a real one")
	}

	return nil
}
