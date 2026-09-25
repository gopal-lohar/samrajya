package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/state"
)

const tickInterval = time.Second

// Row is one line in the TUI - either the current Senapati session, a
// retired one from its rotation history, or a registered sainik.
type Row struct {
	Kind      string // "senapati" | "senapati-retired" | "sainik"
	Title     string
	SessionID string
	Status    string
	Reason    string
	Since     time.Time
	Model     string
}

// registry is the subset of *attention.Registry the TUI needs.
type registry interface {
	List() []attention.Instance
}

// manager is the subset of *senapati.Manager the TUI needs.
type manager interface {
	Model() string
	Current() state.SenapatiRecord
	History() []state.SenapatiRecord
}

type tickMsg struct{}

type Model struct {
	reg      registry
	mgr      manager
	attach   func(sessionID string) string
	problems func() []string

	rows     []Row
	selected int
	height   int
}

// New builds the model with its rows already populated, so the Senapati
// session and its attach command are on screen from the first frame rather
// than after the first refresh tick. attach turns a session ID into the
// command that opens it; problems lists anything the user should know is
// wrong right now (empty when healthy).
func New(reg registry, mgr manager, attach func(string) string, problems func() []string) Model {
	m := Model{reg: reg, mgr: mgr, attach: attach, problems: problems, height: 24}
	m.rows = buildRows(reg, mgr)
	return m
}

func (m Model) Init() tea.Cmd {
	return tick()
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height

	case tickMsg:
		m.rows = buildRows(m.reg, m.mgr)
		if m.selected >= len(m.rows) {
			m.selected = len(m.rows) - 1
		}
		return m, tick()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.rows)-1 {
				m.selected++
			}
		}
	}
	return m, nil
}

func buildRows(reg registry, mgr manager) []Row {
	var rows []Row
	if cur := mgr.Current(); cur.SessionID != "" {
		rows = append(rows, Row{Kind: "senapati", Title: cur.Title, SessionID: cur.SessionID, Since: cur.CreatedAt, Model: mgr.Model()})
	}
	history := mgr.History()
	for i := len(history) - 1; i >= 0; i-- { // most recently retired first
		rows = append(rows, Row{Kind: "senapati-retired", Title: history[i].Title, SessionID: history[i].SessionID})
	}
	for _, inst := range reg.List() {
		title := inst.Label
		if title == "" {
			title = inst.SessionID
		}
		rows = append(rows, Row{Kind: "sainik", Title: title, SessionID: inst.SessionID, Status: inst.Status, Reason: inst.Reason})
	}
	return rows
}

func (m Model) View() string {
	var b strings.Builder
	sainiks := 0
	for _, r := range m.rows {
		if r.Kind == "sainik" {
			sainiks++
		}
	}
	b.WriteString(headerStyle.Render(fmt.Sprintf("mahamantri - %d sainik(s)   up/down select   q quit", sainiks)))
	b.WriteString("\n")

	chrome := 1 + 4 // header + footer
	for _, p := range m.problems() {
		b.WriteString(problemStyle.Render("! " + p))
		b.WriteString("\n")
		chrome++
	}

	visible := m.height - chrome
	if visible < 1 {
		visible = 1
	}
	start := 0
	if m.selected >= visible {
		start = m.selected - visible + 1
	}
	end := start + visible
	if end > len(m.rows) {
		end = len(m.rows)
	}
	for i := start; i < end; i++ {
		marker := "  "
		if i == m.selected {
			marker = "> "
		}
		line := marker + renderRow(m.rows[i])
		if i == m.selected {
			line = selectedStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	if m.selected >= 0 && m.selected < len(m.rows) {
		b.WriteString("\n")
		b.WriteString(attachHintStyle.Render("open in opencode:"))
		b.WriteString("\n")
		b.WriteString(m.attach(m.rows[m.selected].SessionID))
		b.WriteString("\n")
	}
	return b.String()
}

func renderRow(row Row) string {
	switch row.Kind {
	case "senapati":
		text := fmt.Sprintf("%-22s %s", row.Title, row.SessionID)
		if !row.Since.IsZero() {
			text += "  since " + row.Since.Local().Format("Jan 2 15:04")
		}
		if row.Model != "" {
			text += "  model " + row.Model
		} else {
			text += "  model: server default"
		}
		return senapatiStyle.Render(text)
	case "senapati-retired":
		return retiredStyle.Render(fmt.Sprintf("%-22s %s  (retired)", row.Title, row.SessionID))
	default:
		text := fmt.Sprintf("%-22s %s  %s", row.Title, row.SessionID, row.Status)
		if row.Reason != "" {
			text += " (" + row.Reason + ")"
		}
		return styleForStatus(row.Status).Render(text)
	}
}
