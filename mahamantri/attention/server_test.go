package attention

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopal-lohar/samrajya/mahamantri/opencode"
)

func register(t *testing.T, srv *http.Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/instances", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	return rec
}

func lookupFrom(titles map[string]string, err error) SessionLookup {
	return func(ctx context.Context, id string) (string, error) {
		if err != nil {
			return "", err
		}
		title, ok := titles[id]
		if !ok {
			return "", opencode.ErrSessionNotFound
		}
		return title, nil
	}
}

func TestRegisterRejectsUnknownSession(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	srv := NewServer(r, NewBroadcaster(), lookupFrom(map[string]string{}, nil))
	if rec := register(t, srv, `{"sessionID":"ses_typo"}`); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a session that doesn't exist", rec.Code)
	}
	if len(r.List()) != 0 {
		t.Error("an unknown session must not be registered")
	}
}

func TestRegisterDefaultsLabelToSessionTitle(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	srv := NewServer(r, NewBroadcaster(), lookupFrom(map[string]string{"ses_a": "sainik-SEN-30-photos"}, nil))
	if rec := register(t, srv, `{"sessionID":"ses_a"}`); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if inst, _ := r.Get("ses_a"); inst.Label != "sainik-SEN-30-photos" {
		t.Errorf("label = %q, want the session title", inst.Label)
	}

	if rec := register(t, srv, `{"sessionID":"ses_a","label":"x"}`); rec.Code != http.StatusConflict {
		t.Errorf("duplicate status = %d, want 409", rec.Code)
	}
}

func TestRegisterKeepsExplicitLabel(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	srv := NewServer(r, NewBroadcaster(), lookupFrom(map[string]string{"ses_a": "title"}, nil))
	register(t, srv, `{"sessionID":"ses_a","label":"mine"}`)
	if inst, _ := r.Get("ses_a"); inst.Label != "mine" {
		t.Errorf("label = %q, want the explicit one", inst.Label)
	}
}

func TestRegisterReportsUnverifiableSessionAsBadGateway(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	srv := NewServer(r, NewBroadcaster(), lookupFrom(nil, errors.New("connection refused")))
	if rec := register(t, srv, `{"sessionID":"ses_a"}`); rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestRegisterProtectedSessionStillForbidden(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	r.Protect("ses_senapati")
	srv := NewServer(r, NewBroadcaster(), lookupFrom(map[string]string{"ses_senapati": "Senapati-1"}, nil))
	if rec := register(t, srv, `{"sessionID":"ses_senapati"}`); rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestPatchInstanceSetsPhaseAndLabelAndPersists(t *testing.T) {
	r := newTestRegistry(t, &fakeParents{parentOf: map[string]string{}})
	r.Register("ses_a", "orig")
	srv := NewServer(r, NewBroadcaster(), nil)

	req := httptest.NewRequest(http.MethodPatch, "/instances/ses_a", strings.NewReader(`{"phase":"awaiting plan approval"}`))
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if inst, _ := r.Get("ses_a"); inst.Phase != "awaiting plan approval" || inst.Label != "orig" {
		t.Errorf("instance = %+v, want the phase set and the label untouched", inst)
	}

	// Survives a restart: a fresh registry over the same file sees it.
	r2 := NewRegistry(r.path, &fakeParents{parentOf: map[string]string{}})
	if err := r2.ReloadFromDisk(); err != nil {
		t.Fatal(err)
	}
	if inst, _ := r2.Get("ses_a"); inst.Phase != "awaiting plan approval" {
		t.Errorf("phase not persisted: %+v", inst)
	}

	req = httptest.NewRequest(http.MethodPatch, "/instances/ses_missing", strings.NewReader(`{"phase":"x"}`))
	rec = httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown instance: status = %d, want 404", rec.Code)
	}
}
