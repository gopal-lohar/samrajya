package main

import (
	"context"
	"testing"
	"time"
)

func TestKeepMainStartsAMissingSessionAfterGraceAndNotTooOften(t *testing.T) {
	live := true
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runs := 0
	k := newKeepMain([]string{"start"}, func() bool { return live }, quiet)
	k.now = func() time.Time { return now }
	k.run = func(context.Context, []string) ([]byte, error) { runs++; return nil, nil }
	step := func(d time.Duration) { now = now.Add(d); k.check(context.Background()) }

	step(0)
	live = false
	step(0)
	step(time.Minute)
	if runs != 0 {
		t.Fatal("started within the grace period")
	}
	step(time.Minute + time.Second)
	if runs != 1 {
		t.Fatalf("not started after the grace period: %d", runs)
	}
	step(time.Minute)
	if runs != 1 {
		t.Fatal("started again within the gap")
	}
	step(5 * time.Minute)
	if runs != 2 {
		t.Fatalf("not retried after the gap: %d", runs)
	}
	live = true
	step(time.Minute)
	live = false
	step(time.Minute)
	if runs != 2 {
		t.Fatal("a fresh outage skipped its grace period")
	}
}
