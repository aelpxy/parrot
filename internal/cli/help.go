package cli

import (
	"cmp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/aelpxy/parrot/internal/ui"
)

func (a *App) help(cmd *cobra.Command, _ []string) {
	a.ui().Help(helpFor(cmd))
}

// helpFor converts a cobra command into the framework-independent help screen.
func helpFor(cmd *cobra.Command) *ui.Help {
	h := &ui.Help{Description: cmp.Or(cmd.Long, cmd.Short)}

	if cmd.Runnable() {
		h.Usage = append(h.Usage, cmd.UseLine())
	}

	if cmd.HasAvailableSubCommands() {
		h.Usage = append(h.Usage, cmd.CommandPath()+" <command>")
		h.Footer = "run '" + cmd.CommandPath() + " <command> --help' for details on a command"
	}

	for line := range strings.Lines(cmd.Example) {
		if t := strings.TrimSpace(line); t != "" {
			h.Examples = append(h.Examples, t)
		}
	}

	h.Sections = append(h.Sections, commandSections(cmd)...)
	global := pflag.NewFlagSet("global", pflag.ContinueOnError)
	global.AddFlagSet(cmd.PersistentFlags())
	global.AddFlagSet(cmd.InheritedFlags())

	h.Sections = append(h.Sections,
		ui.HelpSection{Title: "flags", Rows: flagRows(cmd.LocalNonPersistentFlags())},
		ui.HelpSection{Title: "global flags", Rows: flagRows(global)},
	)

	return h
}

// commandSections lists subcommands under their group titles, with ungrouped ones last.
func commandSections(cmd *cobra.Command) []ui.HelpSection {
	groups := cmd.Groups()
	sections := make([]ui.HelpSection, 0, len(groups)+1)

	for _, g := range groups {
		sections = append(sections, ui.HelpSection{Title: g.Title, Rows: commandRows(cmd, g.ID)})
	}

	title := "more"
	if len(groups) == 0 {
		title = "commands"
	}

	return append(sections, ui.HelpSection{Title: title, Rows: commandRows(cmd, "")})
}

func commandRows(cmd *cobra.Command, groupID string) []ui.HelpRow {
	var rows []ui.HelpRow

	for _, c := range cmd.Commands() {
		if c.GroupID != groupID || !c.IsAvailableCommand() || c.Name() == "help" {
			continue
		}

		rows = append(rows, ui.HelpRow{Name: c.Name(), Description: c.Short})
	}

	return rows
}

func flagRows(fs *pflag.FlagSet) []ui.HelpRow {
	var rows []ui.HelpRow

	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}

		name := "    --" + f.Name
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", --" + f.Name
		}

		desc := f.Usage

		switch f.Value.Type() {
		case "bool":
		default:
			name += " " + f.Value.Type()

			if f.DefValue != "" && f.DefValue != "0" {
				desc += " (default " + f.DefValue + ")"
			}
		}

		rows = append(rows, ui.HelpRow{Name: name, Description: desc})
	})

	return rows
}
