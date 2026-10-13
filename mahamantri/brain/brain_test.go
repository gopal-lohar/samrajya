package brain

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "brain")
	s, err := Open(dir, func() []string { return []string{"app", "site"} })
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestPutGetListDeleteAndTheIndexFile(t *testing.T) {
	s, dir := open(t)
	if created, err := s.Put(Note{Name: "ask-before-push", Description: "Ask before pushing", Body: "Never push or open a PR without asking first."}); err != nil || !created {
		t.Fatalf("put: %v %v", created, err)
	}
	if _, err := s.Put(Note{Name: "app-tests", Description: "How to run app tests", Project: "app", Body: "Use make check."}); err != nil {
		t.Fatal(err)
	}
	if created, _ := s.Put(Note{Name: "app-tests", Description: "How to run app tests", Project: "app", Body: "Use make check-all."}); created {
		t.Error("replacing a note is not creating one")
	}
	n, err := s.Get("app-tests")
	if err != nil || n.Body != "Use make check-all." || n.Project != "app" || n.Updated.IsZero() {
		t.Errorf("get = %+v %v", n, err)
	}
	list, _ := s.List()
	if len(list) != 2 || list[0].Body != "" {
		t.Errorf("list = %+v", list)
	}
	idx, _ := os.ReadFile(filepath.Join(dir, "INDEX.md"))
	for _, want := range []string{"- app-tests (project app): How to run app tests", "- ask-before-push (everywhere): Ask before pushing"} {
		if !strings.Contains(string(idx), want) || !strings.Contains(s.Index(), want) {
			t.Errorf("index missing %q:\n%s", want, idx)
		}
	}
	if err := s.Delete("app-tests"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("app-tests"); !errors.Is(err, ErrNotFound) || errors.Is(s.Delete("app-tests"), nil) {
		t.Errorf("deleted note still there: %v", err)
	}
	if idx, _ := os.ReadFile(filepath.Join(dir, "INDEX.md")); strings.Contains(string(idx), "app-tests") {
		t.Error("index not rewritten after delete")
	}
}

func TestNotesStaySmallAndValid(t *testing.T) {
	s, _ := open(t)
	for name, n := range map[string]Note{
		"bad name":         {Name: "Bad Name", Description: "x", Body: "x"},
		"no description":   {Name: "a", Body: "x"},
		"two-line desc":    {Name: "a", Description: "x\ny", Body: "x"},
		"long description": {Name: "a", Description: strings.Repeat("x", MaxDescription+1), Body: "x"},
		"empty body":       {Name: "a", Description: "x"},
		"long body":        {Name: "a", Description: "x", Body: strings.Repeat("x", MaxBody+1)},
		"unknown project":  {Name: "a", Description: "x", Body: "x", Project: "elsewhere"},
	} {
		if _, err := s.Put(n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for i := 0; i < MaxNotes; i++ {
		if _, err := s.Put(Note{Name: fmt.Sprintf("n%d", i), Description: "x", Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Put(Note{Name: "one-more", Description: "x", Body: "x"}); !errors.Is(err, ErrFull) || !strings.Contains(err.Error(), "Merge") {
		t.Errorf("past the cap: %v", err)
	}
	if _, err := s.Put(Note{Name: "n0", Description: "y", Body: "updated"}); err != nil {
		t.Errorf("updating a note at the cap must still work: %v", err)
	}
}

func TestForProjectAndHandWrittenNotes(t *testing.T) {
	s, dir := open(t)
	s.Put(Note{Name: "app-a", Description: "A", Project: "app", Body: "first"})
	s.Put(Note{Name: "site-b", Description: "B", Project: "site", Body: "other"})
	s.Put(Note{Name: "general", Description: "G", Body: "everywhere"})
	os.WriteFile(filepath.Join(dir, "by-hand.md"), []byte("# Staging is at stage.example.com\n\nAsk ops for access."), 0o644)
	os.WriteFile(filepath.Join(dir, "README.txt"), []byte("not a note"), 0o644)
	if got := s.ForProject("app"); len(got) != 1 || got[0].Body != "first" {
		t.Errorf("ForProject(app) = %+v", got)
	}
	if s.ForProject("") != nil {
		t.Error("no project: no notes")
	}
	n, err := s.Get("by-hand")
	if err != nil || n.Description != "Staging is at stage.example.com" || n.Updated.IsZero() {
		t.Errorf("hand-written note = %+v %v", n, err)
	}
	if list, _ := s.List(); len(list) != 4 {
		t.Errorf("list = %d notes, want 4 (INDEX.md and README.txt are not notes)", len(list))
	}
}

func TestAPI(t *testing.T) {
	s, _ := open(t)
	mux := http.NewServeMux()
	Routes(mux, s)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	if rec := do("PUT", "/brain/ask-before-push", `{"description":"Ask before pushing","body":"Never push without asking."}`); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do("PUT", "/brain/ask-before-push", `{"description":"Ask before pushing","body":"Never push or merge without asking."}`); rec.Code != http.StatusOK {
		t.Errorf("update: %d", rec.Code)
	}
	if rec := do("GET", "/brain", ""); !strings.Contains(rec.Body.String(), `"count":1`) || !strings.Contains(rec.Body.String(), `"max":50`) {
		t.Errorf("list: %s", rec.Body)
	}
	if rec := do("GET", "/brain/ask-before-push", ""); !strings.Contains(rec.Body.String(), "merge without asking") {
		t.Errorf("get: %s", rec.Body)
	}
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"PUT", "/brain/x", `{"description":"","body":"x"}`, 400},
		{"PUT", "/brain/x", `not json`, 400},
		{"GET", "/brain/nope", "", 404},
		{"DELETE", "/brain/nope", "", 404},
		{"DELETE", "/brain/ask-before-push", "", 204},
	} {
		if rec := do(c.method, c.path, c.body); rec.Code != c.want {
			t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, rec.Code, c.want, rec.Body)
		}
	}
}
