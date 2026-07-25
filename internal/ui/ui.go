// Package ui centralizes all terminal output so the whole CLI speaks with one
// voice: plain language in the default view, technical detail behind verbose.
package ui

import (
	"fmt"
	"io"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

var (
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleFail  = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleFaint = lipgloss.NewStyle().Faint(true)
	styleBold  = lipgloss.NewStyle().Bold(true)
)

type UI struct {
	Out     io.Writer
	Err     io.Writer
	Verbose bool
	// Interactive is false when stdin is not a TTY (CI, pipes); prompts then
	// fall back to safe defaults instead of hanging.
	Interactive bool
	// AssumeYes (--yes) answers every confirmation with its default.
	AssumeYes bool
}

func New(verbose, assumeYes bool) *UI {
	return &UI{
		Out:         os.Stdout,
		Err:         os.Stderr,
		Verbose:     verbose,
		Interactive: isTerminal(os.Stdin),
		AssumeYes:   assumeYes,
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func (u *UI) Printf(format string, a ...any)  { fmt.Fprintf(u.Out, format+"\n", a...) }
func (u *UI) Success(format string, a ...any) { u.glyph(u.Out, styleOK, "✓", format, a...) }
func (u *UI) Info(format string, a ...any)    { u.glyph(u.Out, styleFaint, "·", format, a...) }
func (u *UI) Bold(s string) string            { return styleBold.Render(s) }
func (u *UI) Faint(s string) string           { return styleFaint.Render(s) }
func (u *UI) Green(s string) string           { return styleOK.Render(s) }
func (u *UI) Red(s string) string             { return styleFail.Render(s) }

// Warn and Fail go to stderr: `donkey apps | grep` must not swallow the very
// lines that say something is wrong.
func (u *UI) Warn(format string, a ...any) { u.glyph(u.Err, styleWarn, "!", format, a...) }
func (u *UI) Fail(format string, a ...any) { u.glyph(u.Err, styleFail, "✗", format, a...) }

// Detail prints technical detail only in verbose mode.
func (u *UI) Detail(format string, a ...any) {
	if u.Verbose {
		fmt.Fprintf(u.Out, "  %s\n", styleFaint.Render(fmt.Sprintf(format, a...)))
	}
}

func (u *UI) glyph(w io.Writer, style lipgloss.Style, g, format string, a ...any) {
	fmt.Fprintf(w, "%s %s\n", style.Render(g), fmt.Sprintf(format, a...))
}

// Confirm asks a yes/no question. --yes and non-interactive sessions return def.
func (u *UI) Confirm(question string, def bool) bool {
	if u.AssumeYes || !u.Interactive {
		return def
	}
	v := def
	if err := huh.NewConfirm().Title(question).Value(&v).Run(); err != nil {
		// The prompt was interrupted (ctrl-c, stdin closed mid-run): treat it
		// as "no". Falling back to the default would let an abort proceed
		// with whatever the question suggested.
		return false
	}
	return v
}
