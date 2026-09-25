package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "senapati.json")
	s, ok, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("ok = true for a missing file, want false")
	}
	if s.Current.SessionID != "" {
		t.Errorf("s = %+v, want zero value", s)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "senapati.json")
	want := SenapatiState{
		Current: SenapatiRecord{SessionID: "ses_2", Title: "Senapati-2", CreatedAt: time.Now().UTC().Truncate(time.Second)},
		History: []SenapatiRecord{
			{SessionID: "ses_1", Title: "Senapati-1", CreatedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)},
		},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(path)
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.Current.SessionID != want.Current.SessionID || len(got.History) != 1 {
		t.Errorf("Load() = %+v, want %+v", got, want)
	}
}
