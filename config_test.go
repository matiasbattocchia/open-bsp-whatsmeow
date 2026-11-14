package main

import (
	"testing"

	"go.mau.fi/whatsmeow/store"
)

// A pairing asks for sixty days unless HISTORY_DAYS says otherwise, and a value
// that is no count of days stops the bridge at start.
func TestHistoryDays(t *testing.T) {
	if days, err := historyDays(""); err != nil || days != 60 {
		t.Errorf("unset: %d %v, want 60", days, err)
	}
	if days, err := historyDays("30"); err != nil || days != 30 {
		t.Errorf("30: %d %v", days, err)
	}
	for _, raw := range []string{"0", "-5", "sixty", "1.5"} {
		if _, err := historyDays(raw); err == nil {
			t.Errorf("%q should be refused", raw)
		}
	}
}

// The request rides the device props the pairing sends: a full sync, as far
// back as asked.
func TestAskHistory(t *testing.T) {
	full, limit := store.DeviceProps.RequireFullSync, store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit
	defer func() {
		store.DeviceProps.RequireFullSync = full
		store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = limit
	}()

	askHistory(45)
	if !store.DeviceProps.GetRequireFullSync() {
		t.Error("a pairing should require the full sync")
	}
	if got := store.DeviceProps.GetHistorySyncConfig().GetFullSyncDaysLimit(); got != 45 {
		t.Errorf("days limit = %d, want 45", got)
	}
}
