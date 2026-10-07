package ui

import "strings"

// Help is a command's help screen, independent of the CLI framework that produced it.
type Help struct {
	Description string
	Usage       []string
	Examples    []string
	Sections    []HelpSection
	Footer      string
}

// HelpSection is a titled list of names (commands or flags) and their descriptions.
type HelpSection struct {
	Title string
	Rows  []HelpRow
}

// HelpRow is one command or flag.
type HelpRow struct {
	Name        string
	Description string
}

const exampleGap = "  "

// Help prints h to stdout using the same palette as every other message.
func (p *Printer) Help(h *Help) {
	lines := []string{h.Description, ""}
	lines = append(lines, p.helpBlock("usage", p.accentLines(h.Usage))...)

	if len(h.Examples) > 0 {
		lines = append(lines, p.helpBlock("examples", p.examples(h.Examples))...)
	}

	for _, s := range h.Sections {
		if len(s.Rows) > 0 {
			lines = append(lines, p.helpBlock(s.Title, p.helpRows(s.Rows))...)
		}
	}

	if h.Footer != "" {
		lines = append(lines, p.hint(h.Footer, p.colorOut))
	}

	p.write(p.out, lines...)
}

func (p *Printer) helpBlock(title string, body []string) []string {
	lines := []string{p.styles.title.Render(strings.ToUpper(title))}

	for _, b := range body {
		lines = append(lines, "  "+b)
	}

	return append(lines, "")
}

func (p *Printer) accentLines(in []string) []string {
	out := make([]string, 0, len(in))
	for _, l := range in {
		out = append(out, p.styles.accent.Render(l))
	}

	return out
}

// examples renders "command  description" lines with the command in the accent color.
func (p *Printer) examples(in []string) []string {
	width := 0

	for _, e := range in {
		cmd, _, _ := strings.Cut(e, exampleGap)
		width = max(width, len(cmd))
	}

	out := make([]string, 0, len(in))

	for _, e := range in {
		cmd, desc, _ := strings.Cut(e, exampleGap)
		if desc = strings.TrimSpace(desc); desc == "" {
			out = append(out, p.styles.accent.Render(cmd))

			continue
		}

		out = append(out, p.styles.accent.Render(pad(cmd, width))+exampleGap+p.styles.faint.Render(desc))
	}

	return out
}

func (p *Printer) helpRows(rows []HelpRow) []string {
	width := 0
	for _, r := range rows {
		width = max(width, len(r.Name))
	}

	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, p.styles.bold.Render(pad(r.Name, width))+columnGap+r.Description)
	}

	return out
}
