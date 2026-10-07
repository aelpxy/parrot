package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/aelpxy/parrot/internal/ports"
)

const maxSuggestedPorts = 3

// pick lists local listening ports, lets the user choose one, and exposes it with opts.
func (a *App) pick(ctx context.Context, opts *tunnelOptions) error {
	listeners, err := (&ports.Scanner{}).Listening()
	if errors.Is(err, ports.ErrUnsupported) {
		return withHint(errors.New("can't list local ports on this system"), "pass a port instead, like 'parrot 3000'")
	}

	if err != nil {
		return fmt.Errorf("list local ports: %w", err)
	}

	if len(listeners) == 0 {
		return withHint(errors.New("nothing is listening on a local port"), "start your app and run 'parrot' again, or pass a port, like 'parrot 3000'")
	}

	if !a.interactive() {
		return withHint(errors.New("no port given"), "try 'parrot "+strconv.Itoa(int(listeners[0].Addr.Port()))+"'; listening now: "+summarize(listeners))
	}

	i, err := a.ui().Pick(ctx, "What do you want to expose?", "↑/↓ to move · enter to start · esc to cancel", pickLabels(listeners))
	if err != nil {
		return fmt.Errorf("pick port: %w", err)
	}

	l := listeners[i]

	return a.startTunnel(ctx, l.Proto(), l.Target(), opts)
}

// interactive reports whether prompts can be shown, which needs a terminal on stdin.
func (a *App) interactive() bool {
	f, ok := a.stdin.(*os.File)

	return ok && term.IsTerminal(int(f.Fd()))
}

// pickLabels renders aligned "port  process  proto" labels.
func pickLabels(ls []ports.Listener) []string {
	portWidth, procWidth := 0, 0

	for _, l := range ls {
		portWidth = max(portWidth, len(portLabel(l)))
		procWidth = max(procWidth, len(processName(l)))
	}

	labels := make([]string, 0, len(ls))
	for _, l := range ls {
		labels = append(labels, fmt.Sprintf("%-*s   %-*s   %s", portWidth, portLabel(l), procWidth, processName(l), l.Proto()))
	}

	return labels
}

func portLabel(l ports.Listener) string {
	if strings.HasPrefix(l.Target(), "localhost:") {
		return ":" + strconv.Itoa(int(l.Addr.Port()))
	}

	return l.Target()
}

func processName(l ports.Listener) string {
	if l.Process == "" {
		return "unknown process"
	}

	return l.Process
}

// summarize lists the first few listeners as "3000 (node), 5432 (postgres)".
func summarize(ls []ports.Listener) string {
	parts := make([]string, 0, maxSuggestedPorts)

	for _, l := range ls[:min(len(ls), maxSuggestedPorts)] {
		name, _, _ := strings.Cut(processName(l), " (")
		parts = append(parts, strconv.Itoa(int(l.Addr.Port()))+" ("+name+")")
	}

	return strings.Join(parts, ", ")
}
