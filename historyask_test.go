package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func TestDaysBackIsWholeDaysAndAtLeastOne(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		since time.Time
		days  uint32
	}{
		{now.Add(-30 * time.Minute), 1},
		{now.Add(-24 * time.Hour), 1},
		{now.Add(-25 * time.Hour), 2},
		{now.AddDate(0, 0, -60), 60},
		{now.Add(time.Hour), 1}, // a start ahead of the clock still asks for a day
	} {
		if got := daysBack(now, c.since); got != c.days {
			t.Errorf("daysBack(%v) = %d, want %d", now.Sub(c.since), got, c.days)
		}
	}
}

func TestPropsAskTheRangeAndLeaveTheDefaultBe(t *testing.T) {
	askHistory(60)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	ranged := propsAsking(HistoryAsk{Since: now.Add(-50 * time.Hour)}, now)
	if !ranged.GetRequireFullSync() || ranged.GetHistorySyncConfig().GetFullSyncDaysLimit() != 3 {
		t.Errorf("a range asks a full sync of its days: %v, %d",
			ranged.GetRequireFullSync(), ranged.GetHistorySyncConfig().GetFullSyncDaysLimit())
	}
	none := propsAsking(HistoryAsk{None: true}, now)
	if none.GetRequireFullSync() {
		t.Error("no history asks no full sync")
	}
	same := propsAsking(HistoryAsk{}, now)
	if !proto.Equal(same, store.DeviceProps) {
		t.Error("the zero ask is the props as they stand")
	}
	// the ask is the pairing's own: every other pairing still asks the default
	if store.DeviceProps.GetHistorySyncConfig().GetFullSyncDaysLimit() != 60 ||
		!store.DeviceProps.GetRequireFullSync() {
		t.Error("an ask changed the props every pairing shares")
	}
}

func TestPairingRegistersWithItsAskAndALoginCarriesNone(t *testing.T) {
	askHistory(60)
	st, err := OpenStore(context.Background(), "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()

	device := st.Container.NewDevice()
	since := time.Now().Add(-36 * time.Hour)
	payload := pairingPayload(device, HistoryAsk{Since: since})()
	var props waCompanionReg.DeviceProps
	if err := proto.Unmarshal(payload.GetDevicePairingData().GetDeviceProps(), &props); err != nil {
		t.Fatalf("props: %v", err)
	}
	if props.GetHistorySyncConfig().GetFullSyncDaysLimit() != 2 {
		t.Errorf("registers asking %d days, want 2", props.GetHistorySyncConfig().GetFullSyncDaysLimit())
	}

	own := types.JID{User: "5491100000000", Device: 3, Server: types.DefaultUserServer}
	device.ID = &own
	if login := pairingPayload(device, HistoryAsk{None: true})(); login.GetDevicePairingData() != nil {
		t.Error("a registered device logs in, and names no props")
	}
}

func TestHistoryPostsWhatThePairingAsked(t *testing.T) {
	since := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	ranged := HistoryAsk{Since: since}
	if ranged.posts(since.Add(-time.Second)) || !ranged.posts(since) || !ranged.posts(since.Add(time.Hour)) {
		t.Error("a range posts from its start on")
	}
	if (HistoryAsk{None: true}).posts(since) {
		t.Error("none posts nothing")
	}
	if !(HistoryAsk{}).posts(since.AddDate(-1, 0, 0)) {
		t.Error("the default posts whatever the phone sends")
	}
}
