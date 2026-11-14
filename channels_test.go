package main

import (
	"context"
	"errors"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

var channelJID = types.NewJID("120363400000000001", types.NewsletterServer)

func channelMeta(role types.NewsletterRole) *types.NewsletterMetadata {
	return &types.NewsletterMetadata{
		ID:         channelJID,
		ThreadMeta: types.NewsletterThreadMetadata{Name: types.NewsletterText{Text: "Obra al día"}},
		ViewerMeta: &types.NewsletterViewerMetadata{Role: role},
	}
}

// The account runs a channel as its owner or an admin; following it, or
// guesting, runs nothing, and neither does metadata that names no role.
func TestChannelOf(t *testing.T) {
	for role, runs := range map[types.NewsletterRole]bool{
		types.NewsletterRoleOwner:      true,
		types.NewsletterRoleAdmin:      true,
		types.NewsletterRoleSubscriber: false,
		types.NewsletterRoleGuest:      false,
	} {
		if c := channelOf(channelMeta(role)); c.runs != runs || c.name != "Obra al día" {
			t.Errorf("%s: %+v", role, c)
		}
	}
	bare := channelMeta(types.NewsletterRoleOwner)
	bare.ViewerMeta = nil
	if channelOf(bare).runs {
		t.Error("no viewer metadata says the account runs nothing")
	}
}

// A channel's role is read once and kept, a followed one included; a channel
// that cannot be read keeps nothing, so its next post asks again.
func TestReadChannelKeepsTheRole(t *testing.T) {
	session := &Session{Address: "5491100000009"}
	reads := 0
	read := func(context.Context, types.JID) (*types.NewsletterMetadata, error) {
		reads++
		return channelMeta(types.NewsletterRoleSubscriber), nil
	}
	if c, ok := readChannel(session, channelJID, read); !ok || c.runs {
		t.Fatalf("followed: %+v %v", c, ok)
	}
	readChannel(session, channelJID, read)
	if reads != 1 {
		t.Errorf("a kept channel is not read again: %d reads", reads)
	}
	session.forgetChannel(channelJID.String())
	readChannel(session, channelJID, read)
	if reads != 2 {
		t.Errorf("a channel the account left is read again: %d reads", reads)
	}

	other := types.NewJID("120363400000000002", types.NewsletterServer)
	failing := func(context.Context, types.JID) (*types.NewsletterMetadata, error) {
		return nil, errors.New("timed out")
	}
	if _, ok := readChannel(session, other, failing); ok {
		t.Error("an unreadable channel has no standing")
	}
	if _, ok := session.cachedChannel(other.String()); ok {
		t.Error("an unreadable channel keeps nothing")
	}
}

// A channel the account runs is addressed by its JID, and its post is the
// account's: the session's own number writes it, in the id as on the message.
// A followed one is no chat at all.
func TestChannelAddresses(t *testing.T) {
	session := &Session{Address: "5491100000009"}
	session.noteChannel(channelJID.String(), channelMeta(types.NewsletterRoleAdmin))
	post := types.MessageSource{Chat: channelJID, Sender: channelJID, IsFromMe: true}
	if got := conversationAddressFor(session, post); got != "120363400000000001@newsletter" {
		t.Errorf("conversation = %q", got)
	}
	if got := chatSegment(session, post); got != "120363400000000001" {
		t.Errorf("chat segment = %q", got)
	}
	if got := senderAddressFor(session, post); got != session.Address {
		t.Errorf("sender = %q, want the account", got)
	}
	if got := authorSegment(session, types.MessageInfo{MessageSource: post}); got != session.Address {
		t.Errorf("author segment = %q, want the account", got)
	}
	// a revoke's key names a post, and every post is the account's
	if got := keySender(session, post, session.Address, false, ""); got != session.Address {
		t.Errorf("a key in a channel = %q, want the account", got)
	}
	if address, ok := chatAddress(session, channelJID); !ok || address != "120363400000000001@newsletter" {
		t.Errorf("a run channel's room address: %q %v", address, ok)
	}
	if session.channelName(channelJID.String()) != "Obra al día" {
		t.Errorf("name = %q", session.channelName(channelJID.String()))
	}

	followed := types.NewJID("120363400000000003", types.NewsletterServer)
	session.noteChannel(followed.String(), channelMeta(types.NewsletterRoleSubscriber))
	if _, ok := chatAddress(session, followed); ok {
		t.Error("a followed channel is no conversation")
	}
}

// What whatsmeow says of a broadcast list's message: the chat is the list and
// the author its participant. Received, the live wire flags it a group and
// IsIncomingBroadcast holds; the history import flags nothing. Either way the
// list keeps its address and the author is who wrote it; the account's own
// message to a list is the account's, at the list.
func TestBroadcastListAddresses(t *testing.T) {
	session := &Session{Address: "5491100000009"}
	list := types.NewJID("1731234567", types.BroadcastServer)
	ana := types.NewJID("5491100000001", types.DefaultUserServer)

	live := types.MessageSource{Chat: list, Sender: ana, IsGroup: true}
	history := types.MessageSource{Chat: list, Sender: ana}
	for name, source := range map[string]types.MessageSource{"live": live, "history": history} {
		if got := conversationAddressFor(session, source); got != "1731234567@broadcast" {
			t.Errorf("%s: conversation = %q", name, got)
		}
		if got := senderAddressFor(session, source); got != ana.User {
			t.Errorf("%s: sender = %q, want the author", name, got)
		}
		if got := chatSegment(session, source); got != list.User {
			t.Errorf("%s: chat segment = %q", name, got)
		}
	}
	if !live.IsIncomingBroadcast() {
		t.Error("a list's message from somebody else is an incoming broadcast")
	}

	own := types.MessageSource{Chat: list, Sender: types.NewJID(session.Address, types.DefaultUserServer), IsFromMe: true}
	if got := conversationAddressFor(session, own); got != "1731234567@broadcast" {
		t.Errorf("own: conversation = %q", got)
	}
	if got := senderAddressFor(session, own); got != session.Address {
		t.Errorf("own: sender = %q", got)
	}
	if own.IsIncomingBroadcast() {
		t.Error("the account's own message to its list is no incoming broadcast")
	}
	if address, ok := chatAddress(session, list); !ok || address != "1731234567@broadcast" {
		t.Errorf("a list's room address: %q %v", address, ok)
	}
}
