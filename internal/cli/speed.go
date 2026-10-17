package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/aelpxy/parrot/internal/client"
)

const (
	bitsPerMegabit = 1e6
	megabitsPerGig = 1000

	// Latencies below this get a decimal place, since local servers answer in a few milliseconds.
	preciseLatency = 10 * time.Millisecond
)

// speedReport is what 'parrot speedtest --json' prints.
type speedReport struct {
	Server       string  `json:"server"`
	LatencyMS    float64 `json:"latency_ms"`
	DownloadMbps float64 `json:"download_mbps"`
	UploadMbps   float64 `json:"upload_mbps"`
}

func (a *App) speedTestCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:     "speedtest",
		Short:   "Measure latency and throughput to the server",
		Example: "  parrot speedtest\n  parrot speedtest --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts, err := a.clientOptions()
			if err != nil {
				return err
			}

			show := a.ui().SpeedRow
			if asJSON {
				show = func(string, string) {}
			} else {
				a.ui().SpeedHeader(serverHost(opts.Server))
			}

			report, err := runSpeedTest(cmd.Context(), opts, show)
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSON(a.stdout, report)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	return cmd
}

// runSpeedTest measures latency, download, then upload, passing each result to show as it completes.
func runSpeedTest(ctx context.Context, opts *client.Options, show func(label, value string)) (*speedReport, error) {
	st, err := client.DialSpeedTest(ctx, opts)
	if err != nil {
		return nil, connectError(err)
	}
	defer func() { _ = st.Close() }()

	report := &speedReport{Server: serverHost(opts.Server)}

	latency, err := st.Latency()
	if err != nil {
		return nil, fmt.Errorf("measure latency: %w", err)
	}

	report.LatencyMS = float64(latency) / float64(time.Millisecond)
	show("latency", formatLatency(latency))

	down, err := st.Download()
	if err != nil {
		return nil, speedTestError(err)
	}

	report.DownloadMbps = down / bitsPerMegabit
	show("download", formatRate(down))

	up, err := st.Upload()
	if err != nil {
		return nil, speedTestError(err)
	}

	report.UploadMbps = up / bitsPerMegabit
	show("upload", formatRate(up))

	return report, nil
}

func speedTestError(err error) error {
	if errors.Is(err, client.ErrSpeedTestUnsupported) {
		return withHint(err, "the server runs an older parrot; its operator can update it")
	}

	return err
}

func serverHost(server string) string {
	host, _, err := net.SplitHostPort(server)
	if err != nil {
		return server
	}

	return host
}

func formatLatency(d time.Duration) string {
	ms := float64(d) / float64(time.Millisecond)
	if d < preciseLatency {
		return strconv.FormatFloat(ms, 'f', 1, 64) + " ms"
	}

	return strconv.FormatFloat(ms, 'f', 0, 64) + " ms"
}

func formatRate(bps float64) string {
	mbps := bps / bitsPerMegabit

	switch {
	case mbps >= megabitsPerGig:
		return strconv.FormatFloat(mbps/megabitsPerGig, 'f', 2, 64) + " Gbit/s"
	case mbps >= 1:
		return strconv.FormatFloat(mbps, 'f', 1, 64) + " Mbit/s"
	}

	return strconv.FormatFloat(mbps*megabitsPerGig, 'f', 0, 64) + " kbit/s"
}
