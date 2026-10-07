package ui

import "strings"

// ServiceState is what `service status` shows for one service.
type ServiceState struct {
	Name    string
	State   string
	Details []string
	Fields  []HelpRow
	Healthy bool
}

// Service prints a service's state, details, labeled fields, and hints.
func (p *Printer) Service(s *ServiceState, hints ...string) {
	style := p.styles.warn
	if s.Healthy {
		style = p.styles.success
	}

	lines := []string{style.Render("●") + " " + p.styles.bold.Render(s.Name) + "  " + style.Render(s.State)}

	if len(s.Details) > 0 {
		lines = append(lines, "  "+p.styles.faint.Render(strings.Join(s.Details, " · ")))
	}

	width := 0
	for _, f := range s.Fields {
		width = max(width, len(f.Name))
	}

	for _, f := range s.Fields {
		lines = append(lines, "  "+p.styles.muted.Render(pad(f.Name, width))+"  "+p.hint(f.Description, p.colorOut))
	}

	for _, h := range hints {
		lines = append(lines, "  "+p.hint(h, p.colorOut))
	}

	p.write(p.out, lines...)
}
