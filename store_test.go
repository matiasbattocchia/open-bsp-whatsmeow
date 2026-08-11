package main

import (
	"context"
	"path/filepath"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestIsSQLite(t *testing.T) {
	sqlite := []string{"file:bridge.db", "sqlite:bridge.db", "sqlite3:/data/bridge.db"}
	for _, dsn := range sqlite {
		if !isSQLite(dsn) {
			t.Errorf("isSQLite(%q) = false, want true", dsn)
		}
	}
	postgres := []string{
		"postgres://postgres:postgres@db:5432/postgres",
		"postgresql://user@host/db",
	}
	for _, dsn := range postgres {
		if isSQLite(dsn) {
			t.Errorf("isSQLite(%q) = true, want false", dsn)
		}
	}
}

// The SQLite tier end to end: whatsmeow's own migrations run (Upgrade), and the
// bridge's mapping table round-trips. The mapping queries use `$1` placeholders
// written for pgx — SQLite reads those as NAMED parameters and numbers them in
// order of first appearance, so positional binding still lines up. That is the
// one thing in this file worth proving rather than reasoning about.
func TestSQLiteStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "bridge.db")

	st, err := OpenStore(ctx, dsn, waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()
	if st.Container == nil {
		t.Fatal("OpenStore returned no whatsmeow container")
	}

	// whatsmeow's store is usable, not merely created.
	if _, err := st.Container.GetAllDevices(ctx); err != nil {
		t.Fatalf("GetAllDevices: %v", err)
	}

	m := SessionMapping{DeviceJID: "5491100000000.0:1@s.whatsapp.net", OrganizationID: "org", Address: "5491100000000"}
	if err := st.SaveMapping(ctx, m); err != nil {
		t.Fatalf("SaveMapping: %v", err)
	}

	got, err := st.GetMapping(ctx, m.DeviceJID)
	if err != nil {
		t.Fatalf("GetMapping: %v", err)
	}
	if got == nil || *got != m {
		t.Fatalf("GetMapping = %+v, want %+v", got, m)
	}

	// The `on conflict do update` half of the upsert.
	m.Address = "5491199999999"
	if err := st.SaveMapping(ctx, m); err != nil {
		t.Fatalf("SaveMapping (update): %v", err)
	}
	if got, err = st.GetMapping(ctx, m.DeviceJID); err != nil || got == nil || got.Address != m.Address {
		t.Fatalf("GetMapping after update = %+v (err %v), want address %s", got, err, m.Address)
	}

	if err := st.DeleteMapping(ctx, m.DeviceJID); err != nil {
		t.Fatalf("DeleteMapping: %v", err)
	}
	if got, err = st.GetMapping(ctx, m.DeviceJID); err != nil {
		t.Fatalf("GetMapping after delete: %v", err)
	} else if got != nil {
		t.Fatalf("GetMapping after delete = %+v, want nil", got)
	}
}
