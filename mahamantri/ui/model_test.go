package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gopal-lohar/samrajya/mahamantri/attention"
	"github.com/gopal-lohar/samrajya/mahamantri/state"
)

type fakeReg struct{ list []attention.Instance }

func (f fakeReg) List() []attention.Instance { return f.list }

type fakeMgr struct {
	cur   state.SenapatiRecord
	hist  []state.SenapatiRecord
	model string
}

func (f fakeMgr) Model() string { return f.model }

func (f fakeMgr) Current() state.SenapatiRecord   { return f.cur }
func (f fakeMgr) History() []state.SenapatiRecord { return f.hist }

func attachFor(id string) string { return "opencode --server http://x --session " + id }
func healthy() []string          { return nil }

func newModel(mgr fakeMgr, sainiks ...attention.Instance) Model {
	return New(fakeReg{sainiks}, mgr, attachFor, healthy)
}

// Regression: rows used to be built only on the first 1s tick, so at
// startup the screen showed no Senapati session ID at all.
func TestFirstFrameShowsSenapatiSessionAndAttachCommand(t *testing.T) {
	m := newModel(fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_abc", Title: "Senapati-1", CreatedAt: time.Now()}})
	view := m.View() // no Update, no tick - exactly what the user sees at startup
	for _, want := range []string{"Senapati-1", "ses_abc", "opencode --server http://x --session ses_abc"} {
		if !strings.Contains(view, want) {
			t.Errorf("first frame is missing %q:\n%s", want, view)
		}
	}
}

func TestSelectionMovesAttachCommandToThatRow(t *testing.T) {
	m := newModel(
		fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_sen", Title: "Senapati-1"}},
		attention.Instance{SessionID: "ses_sai", Label: "sainik-SEN-30-photos", Status: "running"},
	)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	view := next.(Model).View()
	if !strings.Contains(view, "--session ses_sai") || strings.Contains(view, "--session ses_sen") {
		t.Errorf("attach command should follow the selected row:\n%s", view)
	}
	if !strings.Contains(view, "sainik-SEN-30-photos") {
		t.Errorf("sainik label missing:\n%s", view)
	}
}

func TestProblemsAreShown(t *testing.T) {
	m := New(fakeReg{}, fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_a", Title: "Senapati-1"}}, attachFor,
		func() []string { return []string{"opencode rejected the password"} })
	if view := m.View(); !strings.Contains(view, "opencode rejected the password") {
		t.Errorf("problem not shown:\n%s", view)
	}
}

func TestRetiredSenapatiSessionsAreListedNewestFirst(t *testing.T) {
	m := newModel(fakeMgr{
		cur:  state.SenapatiRecord{SessionID: "ses_3", Title: "Senapati-3"},
		hist: []state.SenapatiRecord{{SessionID: "ses_1", Title: "Senapati-1"}, {SessionID: "ses_2", Title: "Senapati-2"}},
	})
	if got := []string{m.rows[0].Title, m.rows[1].Title, m.rows[2].Title}; got[0] != "Senapati-3" || got[1] != "Senapati-2" || got[2] != "Senapati-1" {
		t.Errorf("row order = %v", got)
	}
}

func TestSelectedRowStaysVisibleWhenListIsLong(t *testing.T) {
	var sainiks []attention.Instance
	for i := 0; i < 30; i++ {
		sainiks = append(sainiks, attention.Instance{SessionID: "ses_" + string(rune('a'+i%26)) + string(rune('0'+i/26)), Label: "sainik-" + string(rune('A'+i%26)), Status: "running"})
	}
	m := newModel(fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_sen", Title: "Senapati-1"}}, sainiks...)
	var model tea.Model = m
	model, _ = model.Update(tea.WindowSizeMsg{Height: 12})
	for i := 0; i < 25; i++ {
		model, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	view := model.(Model).View()
	if !strings.Contains(view, "> ") || !strings.Contains(view, model.(Model).rows[25].SessionID) {
		t.Errorf("selected row scrolled out of view:\n%s", view)
	}
}

// The model was invisible, so a session quietly running on the server's
// free default looked identical to one on the model the user chose.
func TestSenapatiRowShowsTheModelInUse(t *testing.T) {
	m := newModel(fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_a", Title: "Senapati-1"}, model: "openai/gpt-6-sol"})
	if view := m.View(); !strings.Contains(view, "model openai/gpt-6-sol") {
		t.Errorf("model not shown:\n%s", view)
	}
	m = newModel(fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_a", Title: "Senapati-1"}})
	if view := m.View(); !strings.Contains(view, "server default") {
		t.Errorf("an unconfigured model must say so:\n%s", view)
	}
}

func TestSainikRowShowsItsPhase(t *testing.T) {
	m := newModel(fakeMgr{cur: state.SenapatiRecord{SessionID: "ses_s", Title: "Senapati-1"}},
		attention.Instance{SessionID: "ses_a", Label: "sainik-SEN-30-x", Status: "idle", Phase: "awaiting plan approval"})
	if view := m.View(); !strings.Contains(view, "[awaiting plan approval]") {
		t.Errorf("phase not shown:\n%s", view)
	}
}
