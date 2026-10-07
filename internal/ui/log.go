package ui

import (
	"log/slog"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/log/v2"
)

// levelBadgeWidth fits the longest level name ("ERROR") plus padding.
const levelBadgeWidth = 7

// Logger returns a structured logger that writes styled lines to stderr.
func (p *Printer) Logger(level slog.Level) *slog.Logger {
	l := log.NewWithOptions(p.rawErr, log.Options{
		Level:           log.Level(level),
		ReportTimestamp: true,
		TimeFormat:      time.TimeOnly,
	})
	l.SetColorProfile(p.logProfile)
	l.SetStyles(logStyles())

	return slog.New(l)
}

// LogLine prints one tunnel log entry in the same style as the structured logs.
func (p *Printer) LogLine(t time.Time, level slog.Level, msg string) {
	s := logStyles()

	badge, ok := s.Levels[log.Level(level)]
	if !ok {
		badge = s.Levels[log.InfoLevel]
	}

	p.write(p.out, s.Timestamp.Render(t.Format(time.TimeOnly))+" "+badge.String()+" "+msg)
}

func logStyles() *log.Styles {
	st := newStyles()

	s := log.DefaultStyles()
	s.Timestamp = st.faint
	s.Message = st.bold
	s.Key = st.muted
	s.Separator = lipgloss.NewStyle().Foreground(lipgloss.Color(colorSubtle))

	levels := map[log.Level]string{
		log.DebugLevel: colorDebug,
		log.InfoLevel:  colorAccent,
		log.WarnLevel:  colorWarn,
		log.ErrorLevel: colorError,
	}

	for level, color := range levels {
		s.Levels[level] = lipgloss.NewStyle().
			SetString(strings.ToUpper(level.String())).
			Bold(true).
			Padding(0, 1).
			Width(levelBadgeWidth).
			Foreground(lipgloss.Black).
			Background(lipgloss.Color(color))
	}

	s.Values["url"] = st.link
	s.Values["public_url"] = st.link
	s.Values["error"] = lipgloss.NewStyle().Foreground(lipgloss.Color(colorError))
	s.Values["reason"] = s.Values["error"]
	s.Values["tunnel"] = st.bold

	return s
}
