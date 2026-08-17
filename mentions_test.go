package main

import (
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// extendedTextWithMentions is the shape an inbound tagged message arrives in.
func extendedTextWithMentions(text string, jids ...string) *waE2E.Message {
	return &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text:        proto.String(text),
			ContextInfo: &waE2E.ContextInfo{MentionedJID: jids},
		},
	}
}

// The namespace decision itself, without a paired client: what inbound
// traffic taught (noteAddressingMode) settles it, and a LID chat is
// lid-addressed by its own JID. Only the never-seen group reaches the
// network, which is why the cache is worth its lines.
func TestChatSpeaksLID(t *testing.T) {
	s := &Session{Address: "5491100000000"}
	group := types.NewJID("123456-789", types.GroupServer)
	s.noteAddressingMode(group.String(), types.AddressingModeLID)
	if !s.chatSpeaksLID(group) {
		t.Fatalf("a lid-addressed group must speak LID")
	}

	pnGroup := types.NewJID("987654-321", types.GroupServer)
	s.noteAddressingMode(pnGroup.String(), types.AddressingModePN)
	if s.chatSpeaksLID(pnGroup) {
		t.Fatalf("a phone-addressed group must not speak LID")
	}

	// A direct chat with a hidden user: the JID says it outright.
	if !s.chatSpeaksLID(types.NewJID("236302099558894", types.HiddenUserServer)) {
		t.Fatalf("a lid JID speaks LID")
	}
	// A direct chat with a phone peer: no network, no LID.
	if s.chatSpeaksLID(types.NewJID("5491199999999", types.DefaultUserServer)) {
		t.Fatalf("a phone JID must not speak LID")
	}

	// An empty mode never poisons the cache (unknown stays unknown).
	unseen := types.NewJID("555000-111", types.GroupServer)
	s.noteAddressingMode(unseen.String(), "")
	if s.addressingMode(unseen.String()) != "" {
		t.Fatalf("an empty addressing mode must not be recorded")
	}
}

// Inbound: the text token lands in the same namespace as Mention.Address.
// Phone-form mentions are already canonical — nothing moves, and no LID
// store lookup happens (canonicalUser short-circuits on a non-hidden JID).
func TestMentionsInLeavesCanonicalTokensAlone(t *testing.T) {
	s := &Session{Address: "5491100000000"}
	msg := extendedTextWithMentions(
		"@5492604560911 y @5491177777777 vengan",
		"5492604560911@s.whatsapp.net",
		"5491177777777@s.whatsapp.net",
	)
	text, mentions := mentionsIn(s, msg, "@5492604560911 y @5491177777777 vengan")
	if text != "@5492604560911 y @5491177777777 vengan" {
		t.Fatalf("canonical tokens must survive verbatim, got %q", text)
	}
	if len(mentions) != 2 ||
		mentions[0].Address != "5492604560911" ||
		mentions[1].Address != "5491177777777" {
		t.Fatalf("unexpected mentions: %+v", mentions)
	}
}
