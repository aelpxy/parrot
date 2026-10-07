// Package ui renders styled terminal output, degrading to plain text when not on a TTY.
//
// Every message uses one vocabulary: ✓ success, ✗ error, ! warning, • note. Hints are
// lowercase sentences in which commands are written in single quotes; on color terminals
// the quotes are replaced by the accent color.
package ui

import (
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// The palette shared by messages, tables, help, and logs (xterm-256 codes, downsampled as needed).
const (
	colorAccent  = "86"
	colorSuccess = "42"
	colorWarn    = "192"
	colorError   = "204"
	colorDebug   = "63"
	colorMuted   = "244"
	colorSubtle  = "240"
)

// Options configures a Printer.
type Options struct {
	NoColor bool
}

// Printer writes styled messages to stdout and stderr. It is safe for concurrent use.
type Printer struct {
	styles styles
	tty    bool

	// Logs go to the raw stderr because charm log applies logProfile itself.
	rawErr     io.Writer
	logProfile colorprofile.Profile

	colorOut bool
	colorErr bool

	mu  sync.Mutex
	out io.Writer
	err io.Writer
}

type styles struct {
	success lipgloss.Style
	warn    lipgloss.Style
	error   lipgloss.Style
	accent  lipgloss.Style
	link    lipgloss.Style
	faint   lipgloss.Style
	bold    lipgloss.Style
	muted   lipgloss.Style
	title   lipgloss.Style
	badge   lipgloss.Style
}

type hintError interface {
	error
	Hint() string
}

// New returns a Printer that adapts styling to each writer's capabilities.
func New(stdout, stderr io.Writer, opts Options) *Printer {
	outWriter := colorWriter(stdout, opts)
	errWriter := colorWriter(stderr, opts)

	return &Printer{
		out:        outWriter,
		err:        errWriter,
		rawErr:     stderr,
		logProfile: errWriter.Profile,
		colorOut:   hasColor(outWriter.Profile),
		colorErr:   hasColor(errWriter.Profile),
		tty:        colorprofile.Detect(stdout, os.Environ()) != colorprofile.NoTTY,
		styles:     newStyles(),
	}
}

func newStyles() styles {
	color := func(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(c)) }

	return styles{
		success: color(colorSuccess).Bold(true),
		warn:    color(colorWarn).Bold(true),
		error:   color(colorError).Bold(true),
		accent:  color(colorAccent),
		link:    color(colorAccent).Bold(true),
		faint:   lipgloss.NewStyle().Faint(true),
		bold:    lipgloss.NewStyle().Bold(true),
		muted:   color(colorMuted),
		title:   color(colorAccent).Bold(true),
		badge:   lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Black).Background(lipgloss.Color(colorAccent)).Padding(0, 1),
	}
}

// Success prints a confirmation line to stderr, keeping stdout for data that scripts capture.
func (p *Printer) Success(msg string) {
	p.write(p.err, p.styles.success.Render("✓")+" "+msg)
}

// Warn prints a non-fatal problem to stderr.
func (p *Printer) Warn(msg string) {
	p.write(p.err, p.styles.warn.Render("!")+" "+msg)
}

// Note prints neutral information, such as an empty result, followed by optional hints.
func (p *Printer) Note(msg string, hints ...string) {
	lines := make([]string, 0, 1+len(hints))
	lines = append(lines, p.styles.muted.Render("•")+" "+msg)

	for _, h := range hints {
		lines = append(lines, "  "+p.hint(h, p.colorErr))
	}

	p.write(p.err, lines...)
}

// Error prints err to stderr, followed by its hint if it has one.
func (p *Printer) Error(err error) {
	first, rest, _ := strings.Cut(err.Error(), "\n")
	lines := []string{p.styles.error.Render("✗") + " " + first}

	for line := range strings.SplitSeq(rest, "\n") {
		if line != "" {
			lines = append(lines, "  "+line)
		}
	}

	if h, ok := errors.AsType[hintError](err); ok {
		lines = append(lines, "  "+p.hint(h.Hint(), p.colorErr))
	}

	p.write(p.err, lines...)
}

// Highlight shows one important value with hints; when stdout is not a terminal it prints only the value.
func (p *Printer) Highlight(value string, hints ...string) {
	if !p.tty {
		p.write(p.out, value)

		return
	}

	lines := []string{"", "  " + p.styles.link.Render(value), ""}
	for _, h := range hints {
		lines = append(lines, "  "+p.hint(h, p.colorOut))
	}

	p.write(p.out, lines...)
}

// hint renders a hint faintly, with 'quoted' commands in the accent color when color is available.
func (p *Printer) hint(s string, color bool) string {
	parts := strings.Split(s, "'")
	if len(parts)%2 == 0 {
		return p.styles.faint.Render(s)
	}

	var b strings.Builder

	for i, part := range parts {
		switch {
		case i%2 == 0:
			b.WriteString(p.styles.faint.Render(part))
		case color:
			b.WriteString(p.styles.accent.Render(part))
		default:
			b.WriteString("'" + part + "'")
		}
	}

	return b.String()
}

func (p *Printer) write(w io.Writer, lines ...string) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Failing to write to the terminal leaves nothing useful to report.
	_, _ = io.WriteString(w, b.String())
}

// pad right-aligns s to width display cells, ignoring ANSI styling.
func pad(s string, width int) string {
	return s + strings.Repeat(" ", max(0, width-lipgloss.Width(s)))
}

func colorWriter(w io.Writer, opts Options) *colorprofile.Writer {
	cw := colorprofile.NewWriter(w, os.Environ())
	if opts.NoColor {
		cw.Profile = colorprofile.Ascii
	}

	return cw
}

func hasColor(p colorprofile.Profile) bool {
	return p == colorprofile.ANSI || p == colorprofile.ANSI256 || p == colorprofile.TrueColor
}
