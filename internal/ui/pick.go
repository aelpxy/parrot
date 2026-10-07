package ui

import (
	"context"
	"errors"
	"fmt"

	"charm.land/bubbles/v2/key"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
)

// ErrAborted is returned when the user cancels an interactive prompt.
var ErrAborted = errors.New("aborted")

// Pick asks the user to choose one of options on the terminal and returns its index.
func (p *Printer) Pick(ctx context.Context, title, description string, options []string) (int, error) {
	choice := 0

	opts := make([]huh.Option[int], 0, len(options))
	for i, o := range options {
		opts = append(opts, huh.NewOption(o, i))
	}

	field := huh.NewSelect[int]().Title(title).Description(description).Options(opts...).Value(&choice)

	keys := huh.NewDefaultKeyMap()
	keys.Quit = key.NewBinding(key.WithKeys("ctrl+c", "esc"), key.WithHelp("esc", "cancel"))

	err := huh.NewForm(huh.NewGroup(field)).
		WithTheme(pickerTheme()).
		WithKeyMap(keys).
		WithShowHelp(false).
		WithOutput(p.rawErr).
		RunWithContext(ctx)

	switch {
	case errors.Is(err, huh.ErrUserAborted), errors.Is(err, context.Canceled):
		return 0, ErrAborted
	case err != nil:
		return 0, fmt.Errorf("prompt: %w", err)
	}

	return choice, nil
}

// pickerTheme restyles huh's base theme with the shared palette.
func pickerTheme() huh.Theme {
	return huh.ThemeFunc(func(isDark bool) *huh.Styles {
		st := newStyles()
		t := huh.ThemeBase(isDark)

		t.Focused.Base = t.Focused.Base.BorderForeground(lipgloss.Color(colorAccent))
		t.Focused.Title = st.title
		t.Focused.Description = st.faint
		t.Focused.SelectSelector = st.accent.SetString("› ")
		t.Focused.SelectedOption = st.link
		t.Focused.UnselectedOption = lipgloss.NewStyle()
		t.Focused.ErrorIndicator = st.error.SetString(" ✗")
		t.Focused.ErrorMessage = st.error

		t.Help.ShortKey = st.muted
		t.Help.ShortDesc = st.faint
		t.Help.ShortSeparator = st.faint

		t.Blurred = t.Focused
		t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())

		return t
	})
}
