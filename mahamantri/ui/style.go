package ui

import "github.com/charmbracelet/lipgloss"

var (
	headerStyle     = lipgloss.NewStyle().Bold(true).Padding(0, 1).Background(lipgloss.Color("236"))
	selectedStyle   = lipgloss.NewStyle().Background(lipgloss.Color("237"))
	senapatiStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14")).Bold(true)
	retiredStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	blockedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	manualStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("13")).Bold(true)
	idleStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	runningStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	problemStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Bold(true)
	attachHintStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Italic(true)
)

func styleForStatus(status string) lipgloss.Style {
	switch status {
	case "blocked":
		return blockedStyle
	case "manual":
		return manualStyle
	case "idle":
		return idleStyle
	default:
		return runningStyle
	}
}
