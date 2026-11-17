package main

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// holdOnline keeps the session online until the window a send names closes.
func (m *Manager) holdOnline(session *Session, until time.Time) {
	session.online.extend(until, time.Now(), func(p types.Presence) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return session.Client.SendPresence(ctx, p)
	}, func(format string, args ...any) {
		m.log.Infof("Session %s: "+format, append([]any{session.Address}, args...)...)
	})
}

// onlineHold shows the session online while a conversation it spoke in is
// engaged. Online is the one state WhatsApp sends chat states to, and the one
// in which the account's phone stops ringing for new messages, so it lasts
// only until the latest window a send asked for closes.
type onlineHold struct {
	mu    sync.Mutex
	until time.Time
	timer *time.Timer
	shown bool
	// says the session's presence to WhatsApp; set by the first extend
	send func(types.Presence) error
	// tells what each change did
	note func(format string, args ...any)
}

// extend keeps the session online until `until`, the later of it and the
// window already held, and shows it online if it is not yet.
func (h *onlineHold) extend(until, now time.Time, send func(types.Presence) error, note func(string, ...any)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.send, h.note = send, note
	if !until.After(now) || !until.After(h.until) {
		return
	}
	h.until = until
	if h.timer == nil {
		h.timer = time.AfterFunc(until.Sub(now), func() { h.release(time.Now()) })
	} else {
		h.timer.Reset(until.Sub(now))
	}
	h.show(now)
}

// reassert shows the session online again on a fresh connection, which starts
// offline, when its window is still open.
func (h *onlineHold) reassert(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.shown = false
	if h.send != nil && h.until.After(now) {
		h.show(now)
	}
}

// release takes the session offline once its window has closed.
func (h *onlineHold) release(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now.Before(h.until) || !h.shown {
		return
	}
	h.shown = false
	if err := h.send(types.PresenceUnavailable); err != nil {
		h.note("Offline failed: %v", err)
		return
	}
	h.note("Offline")
}

func (h *onlineHold) show(now time.Time) {
	if h.shown {
		return
	}
	if err := h.send(types.PresenceAvailable); err != nil {
		h.note("Online failed: %v", err)
		return
	}
	h.shown = true
	h.note("Online for %s", h.until.Sub(now).Round(time.Second))
}
