package main

import (
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestOnlineHoldsUntilTheLatestWindowCloses(t *testing.T) {
	var said []types.Presence
	send := func(p types.Presence) error { said = append(said, p); return nil }
	note := func(string, ...any) {}
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	var h onlineHold
	defer func() { h.timer.Stop() }()

	h.extend(now.Add(10*time.Minute), now, send, note)
	h.extend(now.Add(15*time.Minute), now.Add(5*time.Minute), send, note)
	h.extend(now.Add(12*time.Minute), now.Add(6*time.Minute), send, note)
	if want := []types.Presence{types.PresenceAvailable}; !reflect.DeepEqual(said, want) {
		t.Fatalf("while held, said %v; want %v", said, want)
	}

	h.release(now.Add(10 * time.Minute))
	if len(said) != 1 {
		t.Fatalf("an extended window released early: %v", said)
	}

	h.reassert(now.Add(11 * time.Minute))
	h.release(now.Add(15 * time.Minute))
	h.release(now.Add(16 * time.Minute))
	want := []types.Presence{types.PresenceAvailable, types.PresenceAvailable, types.PresenceUnavailable}
	if !reflect.DeepEqual(said, want) {
		t.Fatalf("said %v; want %v", said, want)
	}

	h.reassert(now.Add(20 * time.Minute))
	if len(said) != 3 {
		t.Fatalf("a closed window came back online on reconnect: %v", said)
	}
}

func TestOnlineIgnoresAWindowAlreadyClosed(t *testing.T) {
	var said []types.Presence
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	var h onlineHold
	h.extend(now.Add(-time.Minute), now, func(p types.Presence) error { said = append(said, p); return nil }, func(string, ...any) {})
	if len(said) != 0 || h.timer != nil {
		t.Fatalf("a past window went online: %v", said)
	}
}
