// Package theme defines the color themes for the TUI.
package theme

import "github.com/charmbracelet/lipgloss"

// Theme groups all styles used by the interface.
type Theme struct {
	Name string

	Title     lipgloss.Style // header title
	Header    lipgloss.Style
	StatusBar lipgloss.Style
	Accent    lipgloss.Style
	UserTag   lipgloss.Style
	UserBar   lipgloss.Style
	AsstTag   lipgloss.Style
	AsstBar   lipgloss.Style
	ToolTag   lipgloss.Style
	Thinking  lipgloss.Style
	Dim       lipgloss.Style
	Border    lipgloss.Style
	Selected  lipgloss.Style
	OK        lipgloss.Style
	Warn      lipgloss.Style
	Err       lipgloss.Style
	InputBox  lipgloss.Style
	PanelBox  lipgloss.Style
	DialogBox lipgloss.Style
}

// GlamourStyle maps a theme name to glamour's built-in style name.
func (t Theme) GlamourStyle() string {
	switch t.Name {
	case "light":
		return "light"
	case "dracula":
		return "dracula"
	default:
		return "dark"
	}
}

func baseColors(name string) (bg, accent, user, asst, tool, dim string) {
	switch name {
	case "light":
		return "252", "62", "31", "29", "130", "244"
	case "dracula":
		return "61", "212", "141", "84", "219", "243"
	default: // dark
		return "236", "39", "45", "114", "179", "243"
	}
}

// Get returns the named theme.
func Get(name string) Theme {
	bgC, accentC, userC, asstC, toolC, dimC := baseColors(name)
	t := Theme{Name: name}

	t.Title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(accentC))
	t.Header = lipgloss.NewStyle().
		Background(lipgloss.Color(bgC)).
		Foreground(lipgloss.Color("252")).
		Bold(true).
		Padding(0, 1)
	t.StatusBar = lipgloss.NewStyle().
		Background(lipgloss.Color(bgC)).
		Foreground(lipgloss.Color("252")).
		Padding(0, 1)
	t.Accent = lipgloss.NewStyle().Foreground(lipgloss.Color(accentC)).Bold(true)
	t.UserTag = lipgloss.NewStyle().Foreground(lipgloss.Color(userC)).Bold(true)
	t.UserBar = lipgloss.NewStyle().Foreground(lipgloss.Color(userC))
	t.AsstTag = lipgloss.NewStyle().Foreground(lipgloss.Color(asstC)).Bold(true)
	t.AsstBar = lipgloss.NewStyle().Foreground(lipgloss.Color(asstC))
	t.ToolTag = lipgloss.NewStyle().Foreground(lipgloss.Color(toolC)).Bold(true)
	t.Thinking = lipgloss.NewStyle().Foreground(lipgloss.Color(dimC)).Italic(true)
	t.Dim = lipgloss.NewStyle().Foreground(lipgloss.Color(dimC))
	t.Border = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(bgC))
	t.Selected = lipgloss.NewStyle().Foreground(lipgloss.Color(accentC)).Bold(true)
	t.OK = lipgloss.NewStyle().Foreground(lipgloss.Color(asstC))
	t.Warn = lipgloss.NewStyle().Foreground(lipgloss.Color(toolC))
	t.Err = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	t.InputBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(accentC)).
		Padding(0, 1)
	t.PanelBox = lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(accentC)).
		Padding(0, 1)
	t.DialogBox = lipgloss.NewStyle().
		Border(lipgloss.DoubleBorder()).
		BorderForeground(lipgloss.Color(toolC)).
		Padding(0, 1)
	return t
}
