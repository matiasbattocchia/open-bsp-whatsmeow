package main

import (
	"math"
	"time"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waWa6"
	"go.mau.fi/whatsmeow/store"
	"google.golang.org/protobuf/proto"
)

// HistoryAsk is the history a pairing asks the phone for, named by the
// consumer that pairs the number: it knows what it already holds of it. The
// zero value asks what every pairing asks (askHistory: a full sync of
// HISTORY_DAYS).
//
// The phone hears the ask once, in the props the device registers with, and
// sends what it sends to every new device besides: the history handler posts
// only what was asked. The ask is the session's for the process's life; a
// session a restart brings back posts whatever history still comes.
type HistoryAsk struct {
	// None: no history. The phone is told no full sync is needed, and nothing
	// of what it sends is posted, its contact names included.
	None bool
	// Since: the history from then on. The phone is asked for the whole days
	// back to it, and a message dated before it is not posted.
	Since time.Time
}

// daysBack is the whole days from now that reach since: never less than one,
// the least a full sync can be asked for.
func daysBack(now, since time.Time) uint32 {
	days := math.Ceil(now.Sub(since).Hours() / 24)
	if days < 1 {
		return 1
	}
	return uint32(days)
}

// propsAsking is the device props every pairing registers with, asking the
// phone for this history. The zero ask is the props as they stand.
func propsAsking(ask HistoryAsk, now time.Time) *waCompanionReg.DeviceProps {
	props := proto.Clone(store.DeviceProps).(*waCompanionReg.DeviceProps)
	switch {
	case ask.None:
		props.RequireFullSync = proto.Bool(false)
	case !ask.Since.IsZero():
		props.RequireFullSync = proto.Bool(true)
		props.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(daysBack(now, ask.Since))
	}
	return props
}

// pairingPayload is the payload a pairing client registers with: the
// device's own, its props asking for this history. A device already
// registered logs in with a payload that carries no props, and is left be.
func pairingPayload(device *store.Device, ask HistoryAsk) func() *waWa6.ClientPayload {
	return func() *waWa6.ClientPayload {
		payload := device.GetClientPayload()
		if payload.DevicePairingData == nil || (!ask.None && ask.Since.IsZero()) {
			return payload
		}
		props, err := proto.Marshal(propsAsking(ask, time.Now()))
		if err == nil {
			payload.DevicePairingData.DeviceProps = props
		}
		return payload
	}
}

// posts says whether the history handler posts a message dated at: none
// when nothing was asked, and nothing older than the ask.
func (ask HistoryAsk) posts(at time.Time) bool {
	return !ask.None && (ask.Since.IsZero() || !at.Before(ask.Since))
}
