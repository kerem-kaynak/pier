// Package ui is pier's shared terminal look: one accent, ANSI-palette colors
// (they follow the user's terminal theme), lots of dim. lipgloss degrades
// everything to plain text when stdout isn't a TTY, so piped output stays
// clean.
package ui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

var (
	Accent = lipgloss.NewStyle().Foreground(lipgloss.Color("6")) // teal — nautical
	Title  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	Bold   = lipgloss.NewStyle().Bold(true)
	Dim    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	OK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	Warn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	Bad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	Strain = lipgloss.NewStyle().Foreground(lipgloss.Color("208")) // orange

	// Box wraps input areas (the claude-code look).
	Box = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("8")).
		Padding(0, 1).
		MarginLeft(1)
)

// Step renders a progress-step line: accent chevron, indented under the
// command's bold header — the shared shape for every long-running command
// (create, bake, resize).
func Step(s string) string {
	return Accent.Render("  ▸") + " " + s
}

// Mark renders a green ✓ or red ✗.
func Mark(ok bool) string {
	if ok {
		return OK.Render("✓")
	}
	return Bad.Render("✗")
}

// Tilde shortens a path under the user's home to ~/... — for display only,
// never for opening. Absolute paths overflow status lines and put the local
// username in every screenshot someone shares.
func Tilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if rest, ok := strings.CutPrefix(p, home+string(os.PathSeparator)); ok {
		return "~" + string(os.PathSeparator) + rest
	}
	return p
}

// Keys renders a footer hint from key/description pairs:
// Keys("enter", "attach", "q", "quit") → "enter attach · q quit".
func Keys(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, Accent.Render(pairs[i])+" "+Dim.Render(pairs[i+1]))
	}
	return strings.Join(parts, Dim.Render(" · "))
}
