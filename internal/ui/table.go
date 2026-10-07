package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

const columnGap = "   "

// Table prints aligned rows under a header; when stdout is not a terminal it prints tab-separated rows only.
func (p *Printer) Table(header []string, rows [][]string) {
	if !p.tty {
		lines := make([]string, 0, len(rows))
		for _, r := range rows {
			lines = append(lines, strings.Join(r, "\t"))
		}

		p.write(p.out, lines...)

		return
	}

	widths := make([]int, len(header))
	for _, r := range append([][]string{header}, rows...) {
		for i, cell := range r {
			widths[i] = max(widths[i], lipgloss.Width(cell))
		}
	}

	lines := []string{p.styles.muted.Render(joinPadded(header, widths))}
	for _, r := range rows {
		lines = append(lines, joinPadded(r, widths))
	}

	p.write(p.out, lines...)
}

func joinPadded(cells []string, widths []int) string {
	padded := make([]string, len(cells))
	for i, c := range cells {
		padded[i] = pad(c, widths[i])
	}

	return strings.TrimRight(strings.Join(padded, columnGap), " ")
}
