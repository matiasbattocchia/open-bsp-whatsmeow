package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// A LID the store can map is the phone number it stands for; one it cannot
// keeps its server, because bare digits are a phone number to every reader —
// sent back as one, it addressed a number that does not exist and every reply
// failed (live, Royal Care, 2026-09-25). The address goes out and comes back
// in a dispatch to the same JID, and the id minted on that send names the
// chat the way the inbound side does.
func TestLIDOnlyPeerKeepsItsServer(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()

	device := st.Container.NewDevice()
	own := types.JID{User: "60102454715", Device: 3, Server: types.DefaultUserServer}
	device.ID = &own
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{0x01},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := st.Container.PutDevice(ctx, device); err != nil {
		t.Fatalf("PutDevice: %v", err)
	}
	mapped := types.NewJID("102030405060708", types.HiddenUserServer)
	phone := types.NewJID("60123456789", types.DefaultUserServer)
	if err := device.LIDs.PutLIDMapping(ctx, mapped, phone); err != nil {
		t.Fatalf("PutLIDMapping: %v", err)
	}
	session := &Session{Client: whatsmeow.NewClient(device, waLog.Noop), Address: own.User}

	if got := canonicalUser(session, mapped, types.JID{}); got != phone.User {
		t.Errorf("mapped LID = %q, want the phone %q", got, phone.User)
	}

	unmapped := types.JID{User: "120460981321756", Device: 12, Server: types.HiddenUserServer}
	address := canonicalUser(session, unmapped, types.JID{})
	if address != "120460981321756@lid" {
		t.Fatalf("unmapped LID = %q, want 120460981321756@lid", address)
	}

	var req dispatchRequest
	req.Record.ConversationAddress = address
	chat, err := dispatchChatJID(req)
	if err != nil {
		t.Fatalf("dispatchChatJID(%q): %v", address, err)
	}
	if chat != unmapped.ToNonAD() {
		t.Errorf("dispatch to %q went to %s, want %s", address, chat, unmapped.ToNonAD())
	}
	if segment := canonicalUser(session, chat, types.JID{}); segment != address {
		t.Errorf("send mints chat segment %q, the inbound side %q", segment, address)
	}

	req.Record.ConversationAddress = phone.User
	if chat, _ := dispatchChatJID(req); chat != phone {
		t.Errorf("dispatch to %q went to %s, want the phone number", phone.User, chat)
	}
	if got := addressJID("120363413059075642@g.us"); got.Server != types.GroupServer {
		t.Errorf("a group address parsed to %s", got)
	}
}
