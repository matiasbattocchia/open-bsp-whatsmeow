package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// A receiver that answers each post with the next status in its script (200 once the
// script runs out) and keeps the external ids of every batch it was sent, in order. A
// batch named in always gets that status every time, script or not.
type scriptedReceiver struct {
	mu     sync.Mutex
	script []int
	always map[string]int
	posts  []string
	server *httptest.Server
}

func newScriptedReceiver(t *testing.T, script ...int) *scriptedReceiver {
	r := &scriptedReceiver{script: script}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var batch WebhookBatch
		_ = json.Unmarshal(body, &batch)

		r.mu.Lock()
		r.posts = append(r.posts, batch.Messages[0].ExternalID)
		status := http.StatusOK
		if fixed, ok := r.always[batch.Messages[0].ExternalID]; ok {
			status = fixed
		} else if len(r.script) > 0 {
			status, r.script = r.script[0], r.script[1:]
		}
		r.mu.Unlock()

		w.WriteHeader(status)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *scriptedReceiver) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.posts...)
}

func testOutbox(t *testing.T) (*Outbox, *Store) {
	t.Helper()
	backoff, window, fault := DELIVERY_BACKOFF, DELIVERY_WINDOW, FAULT_WINDOW
	DELIVERY_BACKOFF = []time.Duration{time.Millisecond}
	t.Cleanup(func() { DELIVERY_BACKOFF, DELIVERY_WINDOW, FAULT_WINDOW = backoff, window, fault })

	st, err := OpenStore(context.Background(),
		"file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { st.DB.Close() })
	return NewOutbox(st.DB, NewOpenBSP(&Config{BridgeToken: "t"}), waLog.Noop), st
}

func sessionFor(o *Outbox, r *scriptedReceiver) *Session {
	return &Session{
		Address:    "5491100000000",
		WebhookURL: r.server.URL,
		receiver:   o.openbsp.at(r.server.URL),
	}
}

func batchOf(id string) WebhookBatch {
	return WebhookBatch{
		OrganizationAddress: "5491100000000",
		Messages:            []WebhookMessage{{ExternalID: id}},
	}
}

// waitFor polls until the receiver has seen want posts and the outbox is empty.
func waitFor(t *testing.T, st *Store, r *scriptedReceiver, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var owed int
		if err := st.DB.QueryRow(`select count(*) from bridge_outbox`).Scan(&owed); err != nil {
			t.Fatalf("count outbox: %v", err)
		}
		if got := r.received(); len(got) >= want && owed == 0 {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("receiver saw %v, want %d posts and an empty outbox", r.received(), want)
	return nil
}

func assertPosts(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("posts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("posts = %v, want %v", got, want)
		}
	}
}

// A session's batches arrive in the order they happened, and a batch the receiver
// could not take yet holds the ones behind it rather than being overtaken.
func TestOutboxDeliversInOrderThroughOutages(t *testing.T) {
	o, st := testOutbox(t)
	r := newScriptedReceiver(t, http.StatusServiceUnavailable, http.StatusGatewayTimeout)
	session := sessionFor(o, r)

	for _, id := range []string{"a", "b", "c"} {
		if err := o.Deliver(session, batchOf(id)); err != nil {
			t.Fatalf("Deliver %s: %v", id, err)
		}
	}

	assertPosts(t, waitFor(t, st, r, 5), "a", "a", "a", "b", "c")
}

// A refusal is the receiver's last word on that batch: it is dropped, and the lane
// moves on to the next one instead of asking again.
func TestOutboxDropsWhatTheReceiverRefuses(t *testing.T) {
	o, st := testOutbox(t)
	r := newScriptedReceiver(t, http.StatusPaymentRequired)
	session := sessionFor(o, r)

	_ = o.Deliver(session, batchOf("capped"))
	_ = o.Deliver(session, batchOf("next"))

	assertPosts(t, waitFor(t, st, r, 2), "capped", "next")
}

// A 500 is retried while it may be a blip; one that outlasts FAULT_WINDOW is a batch
// the receiver will never take, and the lane behind it must not wait forever.
func TestOutboxGivesUpOnAPersistentFault(t *testing.T) {
	o, st := testOutbox(t)
	FAULT_WINDOW = 20 * time.Millisecond
	DELIVERY_BACKOFF = []time.Duration{10 * time.Millisecond}
	r := newScriptedReceiver(t)
	r.always = map[string]int{"poison": http.StatusInternalServerError}
	session := sessionFor(o, r)

	_ = o.Deliver(session, batchOf("poison"))
	_ = o.Deliver(session, batchOf("next"))

	got := waitFor(t, st, r, 2)
	if got[len(got)-1] != "next" {
		t.Fatalf("posts = %v, want the poison batch dropped and then next", got)
	}
	for _, id := range got[:len(got)-1] {
		if id != "poison" {
			t.Fatalf("posts = %v, want only poison retries before next", got)
		}
	}
}

// What one process queued and could not post, the next one posts on Start.
func TestOutboxResumesAfterRestart(t *testing.T) {
	o, st := testOutbox(t)
	r := newScriptedReceiver(t)
	session := sessionFor(o, r)

	payload, _ := json.Marshal(batchOf("left-behind"))
	if _, err := st.DB.Exec(`
		insert into bridge_outbox (address, webhook_url, payload, queued_at)
		values ($1, $2, $3, $4)`,
		session.Address, session.WebhookURL, string(payload), time.Now().UnixMilli(),
	); err != nil {
		t.Fatalf("seed outbox: %v", err)
	}

	restarted := NewOutbox(st.DB, o.openbsp, waLog.Noop)
	if err := restarted.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	assertPosts(t, waitFor(t, st, r, 1), "left-behind")
}

func TestDeliveryVerdict(t *testing.T) {
	cases := []struct {
		err  error
		want verdict
	}{
		{nil, delivered},
		{errors.New("dial tcp: connection refused"), unreachable},
		{context.DeadlineExceeded, unreachable},
		{&ResponseError{Status: http.StatusRequestTimeout}, unreachable},
		{&ResponseError{Status: http.StatusTooManyRequests}, unreachable},
		{&ResponseError{Status: http.StatusBadGateway}, unreachable},
		{&ResponseError{Status: http.StatusServiceUnavailable}, unreachable},
		{&ResponseError{Status: http.StatusGatewayTimeout}, unreachable},
		{&ResponseError{Status: http.StatusInternalServerError}, faulted},
		{&ResponseError{Status: http.StatusBadRequest}, refused},
		{&ResponseError{Status: http.StatusUnauthorized}, refused},
		{&ResponseError{Status: http.StatusPaymentRequired}, refused},
	}
	for _, c := range cases {
		if got := deliveryVerdict(c.err); got != c.want {
			t.Errorf("%v: got %d, want %d", c.err, got, c.want)
		}
	}
}
