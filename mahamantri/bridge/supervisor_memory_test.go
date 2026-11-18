package bridge

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// explainingLauncher is a fakeLauncher that knows why sessions ended.
type explainingLauncher struct {
	*fakeLauncher
	why map[string]string
}

func (e explainingLauncher) WhyGone(s Session) string { return e.why[s.ID] }

// A lost session is recorded with the runtime's reason for ending it.
func TestSupervisorRecordsWhyASessionWasLost(t *testing.T) {
	r := newRig(t)
	el := explainingLauncher{fakeLauncher: r.l, why: map[string]string{}}
	r.sv.Launcher = el
	var logged bytes.Buffer
	r.sv.Logger = log.New(&logged, "", 0)
	w := r.ping("ENG-1", "c1")
	r.tick()
	r.attach(MainAgent)
	r.attach(w)
	r.advance(10 * time.Minute) // past the reattach grace after a start
	r.keepAlive()
	r.tick()
	a := mustAgent(t, r, w)
	el.why[a.Session] = "Claude Code retired it: idle-prompt, idle 4m [low memory]"
	delete(r.cli, w)
	r.advance(2 * time.Minute)
	r.keepAlive()
	r.tick()
	want := "supervisor: lost " + w + " (samrajya-" + w + "-g1): lost contact (Claude Code retired it: idle-prompt, idle 4m [low memory])"
	if !strings.Contains(logged.String(), want) {
		t.Errorf("log does not say why:\n%s", logged.String())
	}
}

// Low host memory is told to the person once, then again only after
// LowMemoryEvery.
func TestSupervisorWarnsOfLowMemoryOnce(t *testing.T) {
	r, chat := chatRig(t)
	r.s.Home = "chat:1"
	free := uint64(4 << 30)
	r.sv.FreeMemory = func() (uint64, bool) { return free, true }
	r.tick()
	if n := len(chat.got()); n != 0 {
		t.Fatalf("warned with plenty free: %v", chat.got())
	}
	free = 900 << 20
	r.tick()
	r.advance(time.Minute)
	r.tick()
	got := chat.got()
	if len(got) != 1 || !strings.Contains(got[0], "900 MB") {
		t.Fatalf("told %q", got)
	}
	r.advance(6 * time.Hour)
	r.tick()
	if n := len(chat.got()); n != 2 {
		t.Errorf("%d warnings after the quiet period", n)
	}
}
