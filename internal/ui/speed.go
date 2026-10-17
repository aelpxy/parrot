package ui

// speedLabelWidth fits the longest row label, "download".
const speedLabelWidth = 8

// SpeedHeader introduces a speed test against server; plain output skips it.
func (p *Printer) SpeedHeader(server string) {
	if !p.tty {
		return
	}

	p.write(p.out, "", " "+p.styles.badge.Render("parrot")+"  "+p.styles.bold.Render("speed test")+p.styles.faint.Render(" · "+server), "")
}

// SpeedRow prints one measurement as soon as it completes; tab-separated when not a terminal.
func (p *Printer) SpeedRow(label, value string) {
	if !p.tty {
		p.write(p.out, label+"\t"+value)

		return
	}

	p.write(p.out, "  "+p.styles.muted.Render(pad(label, speedLabelWidth))+"  "+p.styles.link.Render(value))
}
