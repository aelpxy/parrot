package ui

import (
	"time"
)

// TunnelRow is one line of the online status block.
type TunnelRow struct {
	Name   string
	URL    string
	Target string
}

// TunnelStatus is one line of a background tunnel listing.
type TunnelStatus struct {
	Name    string
	Proto   string
	URL     string
	Target  string
	Status  string
	Uptime  string
	Healthy bool
}

// Online shows the live tunnels and a footer hint; when stdout is not a terminal it prints only their URLs.
func (p *Printer) Online(rows []TunnelRow, footer string) {
	if !p.tty {
		urls := make([]string, 0, len(rows))
		for _, r := range rows {
			urls = append(urls, r.URL)
		}

		p.write(p.out, urls...)

		return
	}

	nameWidth, urlWidth := 0, 0
	for _, r := range rows {
		nameWidth = max(nameWidth, len(r.Name))
		urlWidth = max(urlWidth, len(r.URL))
	}

	lines := []string{"", " " + p.styles.badge.Render("parrot") + "  " + p.styles.success.Render("● online"), ""}

	for _, r := range rows {
		lines = append(lines, "  "+
			p.styles.bold.Render(pad(r.Name, nameWidth))+"  "+
			p.styles.link.Render(pad(r.URL, urlWidth))+"  "+
			p.styles.faint.Render("→ "+r.Target))
	}

	lines = append(lines, "", "  "+p.hint(footer, p.colorOut))
	p.write(p.out, lines...)
}

// Offline reports a dropped connection and when the next attempt happens.
func (p *Printer) Offline(err error, retryIn time.Duration) {
	msg := "reconnecting in " + retryIn.Round(time.Millisecond).String()
	if err != nil {
		msg += " · " + err.Error()
	}

	p.write(p.err, " "+p.styles.warn.Render("● offline")+"  "+p.styles.faint.Render(msg))
}

// TunnelList shows background tunnels with a colored status; plain tab-separated rows when not a terminal.
func (p *Printer) TunnelList(rows []TunnelStatus) {
	cells := make([][]string, 0, len(rows))

	for _, r := range rows {
		status := r.Status

		if p.tty {
			style := p.styles.warn
			if r.Healthy {
				style = p.styles.success
			}

			status = style.Render("● " + r.Status)
		}

		cells = append(cells, []string{r.Name, r.Proto, r.URL, r.Target, status, r.Uptime})
	}

	p.Table([]string{"NAME", "PROTO", "URL", "TARGET", "STATUS", "UPTIME"}, cells)
}
