package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// Outbox is how a batch reaches whatsapp-web-webhook. whatsmeow acks a message
// to WhatsApp before our handler sees it, so a post that fails is a message no
// one will ever send again: the batch is written to bridge_outbox first, and the
// handler returns. A lane per session posts its rows oldest first and deletes
// each one the receiver took or refused for good; anything that may clear on its
// own is posted again on DELIVERY_BACKOFF. The rows live in the session store,
// so a restart resumes where the last process stopped.
//
// Order is the session's: a lane never posts a row while an older one is owed,
// so an edit never lands before its original and a receipt never before its
// message. The webhook upserts on external_id, so a batch posted twice — a
// timeout the receiver went on to finish — is one batch.
type Outbox struct {
	db      *sql.DB
	openbsp *OpenBSP
	log     waLog.Logger

	mu    sync.Mutex
	lanes map[string]chan struct{} // by session address
}

// DELIVERY_BACKOFF is the wait before post number N+1 of a row, by posts already failed;
// the last rung repeats. The dispatch cron's history is the guide: a retry that ever
// succeeded did so within five minutes, so the ladder climbs fast and then stays at a
// minute, which is all an outage costs past its recovery.
var DELIVERY_BACKOFF = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
	16 * time.Second,
	32 * time.Second,
	1 * time.Minute,
}

// DELIVERY_WINDOW is how long a row is owed: an outage longer than this costs the
// messages it held, the same 12 hours the dispatch cron gives an outbound row.
// FAULT_WINDOW is how long a row the receiver keeps answering 500 for is owed. A 500
// is the webhook throwing — a database blip clears in minutes, a batch it cannot read
// never does — and the lane behind it waits for as long as it is retried.
var (
	DELIVERY_WINDOW = 12 * time.Hour
	FAULT_WINDOW    = 5 * time.Minute
)

const webhookPath = "/whatsapp-web-webhook"

func NewOutbox(db *sql.DB, openbsp *OpenBSP, log waLog.Logger) *Outbox {
	return &Outbox{
		db:      db,
		openbsp: openbsp,
		log:     log,
		lanes:   make(map[string]chan struct{}),
	}
}

// Start resumes every lane a previous process left rows in.
func (o *Outbox) Start(ctx context.Context) error {
	rows, err := o.db.QueryContext(ctx, `select distinct address from bridge_outbox`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var addresses []string
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return err
		}
		addresses = append(addresses, address)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, address := range addresses {
		o.wake(address)
	}
	return nil
}

// Deliver queues batch for session's receiver and returns once it is durable. A store
// that refuses the write gets the batch posted right here instead — on the event loop,
// with no retry, but still sent.
func (o *Outbox) Deliver(session *Session, batch WebhookBatch) error {
	payload, err := json.Marshal(batch)
	if err != nil {
		return err
	}

	if _, err := o.db.Exec(`
		insert into bridge_outbox (address, webhook_url, payload, queued_at)
		values ($1, $2, $3, $4)`,
		session.Address, session.WebhookURL, string(payload), time.Now().UnixMilli(),
	); err != nil {
		o.log.Errorf("Queue batch for %s failed, posting it now: %v", session.Address, err)
		return session.receiver.postJSON(webhookPath, payload)
	}

	o.wake(session.Address)
	return nil
}

// wake tells address's lane there is a row, starting the lane if it has none. The
// signal is a one-slot buffer: a lane busy posting finds it on its next look, and
// any number of wakes meanwhile are the one look.
func (o *Outbox) wake(address string) {
	o.mu.Lock()
	lane, ok := o.lanes[address]
	if !ok {
		lane = make(chan struct{}, 1)
		o.lanes[address] = lane
		go o.drain(address, lane)
	}
	o.mu.Unlock()

	select {
	case lane <- struct{}{}:
	default:
	}
}

type outboxRow struct {
	id         int64
	webhookURL string
	payload    []byte
	queuedAt   time.Time
}

// head is address's oldest row, or nil when the lane owes nothing.
func (o *Outbox) head(address string) (*outboxRow, error) {
	var (
		row      outboxRow
		payload  string
		queuedAt int64
	)
	err := o.db.QueryRow(`
		select id, webhook_url, payload, queued_at from bridge_outbox
		where address = $1 order by id limit 1`,
		address,
	).Scan(&row.id, &row.webhookURL, &payload, &queuedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.payload = []byte(payload)
	row.queuedAt = time.UnixMilli(queuedAt)
	return &row, nil
}

func (o *Outbox) remove(id int64) error {
	_, err := o.db.Exec(`delete from bridge_outbox where id = $1`, id)
	return err
}

// drain is address's lane: post the head, settle it, repeat; sleep when there is none.
func (o *Outbox) drain(address string, lane <-chan struct{}) {
	failed := 0
	var faultSince time.Time

	for {
		row, err := o.head(address)
		if err != nil {
			o.log.Errorf("Read outbox for %s failed: %v", address, err)
			time.Sleep(deliveryWait(failed))
			failed++
			continue
		}
		if row == nil {
			<-lane
			continue
		}

		err = o.openbsp.at(row.webhookURL).postJSON(webhookPath, row.payload)
		settled := true
		switch deliveryVerdict(err) {
		case delivered:
		case refused:
			o.log.Errorf("Receiver refused batch %d for %s, dropping it: %v", row.id, address, err)
		case faulted:
			if faultSince.IsZero() {
				faultSince = time.Now()
			}
			if time.Since(faultSince) < FAULT_WINDOW {
				settled = false
			} else {
				o.log.Errorf("Receiver kept failing batch %d for %s since %s, dropping it: %v",
					row.id, address, faultSince.Format(time.RFC3339), err)
			}
		case unreachable:
			if time.Since(row.queuedAt) < DELIVERY_WINDOW {
				settled = false
			} else {
				o.log.Errorf("Batch %d for %s went undelivered since %s, dropping it: %v",
					row.id, address, row.queuedAt.Format(time.RFC3339), err)
			}
		}

		if !settled {
			wait := deliveryWait(failed)
			failed++
			o.log.Warnf("Post batch %d for %s failed (attempt %d), retrying in %s: %v",
				row.id, address, failed, wait, err)
			time.Sleep(wait)
			continue
		}

		if err := o.remove(row.id); err != nil {
			// Posted again on the next look; the receiver upserts, so twice is once.
			o.log.Errorf("Remove batch %d for %s failed: %v", row.id, address, err)
			time.Sleep(deliveryWait(failed))
		}
		failed = 0
		faultSince = time.Time{}
	}
}

func deliveryWait(failed int) time.Duration {
	return DELIVERY_BACKOFF[min(failed, len(DELIVERY_BACKOFF)-1)]
}

type verdict int

const (
	delivered verdict = iota
	// The receiver read the batch and said no; the same batch gets the same answer.
	refused
	// The receiver threw on it: a blip, or a batch it will never take.
	faulted
	// The batch never reached a receiver able to answer: the network, a timeout, a
	// gateway with nothing behind it, a rate limit.
	unreachable
)

func deliveryVerdict(err error) verdict {
	if err == nil {
		return delivered
	}
	var response *ResponseError
	if !errors.As(err, &response) {
		return unreachable
	}
	switch response.Status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return unreachable
	case http.StatusInternalServerError:
		return faulted
	}
	return refused
}
