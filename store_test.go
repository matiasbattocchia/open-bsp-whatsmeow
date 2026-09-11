package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
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

	m := SessionMapping{
		DeviceJID: "5491100000000.0:1@s.whatsapp.net", OrganizationID: "org", Address: "5491100000000",
		WebhookURL: "http://localhost:8794",
	}
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

// The lookup behind every message's sender_name/conversation_name: a real
// whatsmeow store, a real contact row, and the JID built from the canonical
// digits the webhook carries. Worth proving against the database rather than
// reasoning about, because a lookup that silently misses looks exactly like a
// contact nobody has named — the message still ships, just anonymous.
func TestContactNameReadsTheStore(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()

	device := st.Container.NewDevice()
	own := types.JID{User: "5491100000000", Device: 17, Server: types.DefaultUserServer}
	device.ID = &own
	// PutDevice writes the pairing blobs; a test device only needs them non-null
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{0x01},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := st.Container.PutDevice(ctx, device); err != nil {
		t.Fatalf("PutDevice: %v", err)
	}
	peer := types.NewJID("15613518605", types.DefaultUserServer)
	if err := device.Contacts.PutContactName(ctx, peer, "Gianvito Sobisch", ""); err != nil {
		t.Fatalf("PutContactName: %v", err)
	}

	session := &Session{Client: whatsmeow.NewClient(device, waLog.Noop), Address: own.User}
	if got := contactName(session, peer.User, "gv"); got != "Gianvito Sobisch" {
		t.Errorf("contactName = %q, want the address-book name", got)
	}
	// nobody by that number: the live pushname is all there is, and a total
	// miss must stay empty so the consumer falls back to the address
	if got := contactName(session, "5490000000000", "sol tru"); got != "sol tru" {
		t.Errorf("contactName(unknown) = %q, want the live pushname", got)
	}
	if got := contactName(session, "5490000000000", ""); got != "" {
		t.Errorf("contactName(unknown, no pushname) = %q, want empty", got)
	}
}

// A mute is a deadline: "8 hours" stores a timestamp, "always" the far-future
// sentinel, an unmute zeroes it — marksOf just asks whether it is still ahead
// of the message. No settings row at all is an unmarked chat, never an error.
func TestMarksOfReadsTheDeadline(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		settings types.LocalChatSettings
		muted    bool
		archived bool
	}{
		{"no row: unmarked", types.LocalChatSettings{}, false, false},
		{"timed mute still running", types.LocalChatSettings{Found: true, MutedUntil: now.Add(time.Hour)}, true, false},
		{"timed mute expired", types.LocalChatSettings{Found: true, MutedUntil: now.Add(-time.Hour)}, false, false},
		{"muted forever", types.LocalChatSettings{Found: true, MutedUntil: store.MutedForever}, true, false},
		{"unmuted: zero time", types.LocalChatSettings{Found: true}, false, false},
		{"archived, not muted", types.LocalChatSettings{Found: true, Archived: true}, false, true},
	}
	for _, c := range cases {
		muted, archived := marksOf(c.settings, now)
		if muted != c.muted || archived != c.archived {
			t.Errorf("%s: marksOf = (%v, %v), want (%v, %v)", c.name, muted, archived, c.muted, c.archived)
		}
	}
}
