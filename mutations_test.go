package main

import (
	"net/http"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

// The guards on the three data kinds that act on a message already sent. Building the
// stanza itself needs a paired client; what is testable without one is the refusal —
// and refusing is the whole point: a dispatch that can't name its referent must answer
// 422 (permanent, stamps the send failed) rather than send something else instead.
func TestMutationsRefuseWithoutAReferent(t *testing.T) {
	session := &Session{Address: "5491100000000"}
	chat := types.NewJID("5491199999999", types.DefaultUserServer)

	cases := []struct {
		name    string
		content MessageContent
	}{
		{"edit without a referent", MessageContent{Type: "data", Kind: "edit", Text: "nuevo"}},
		{"revoke without a referent", MessageContent{Type: "data", Kind: "revoke"}},
		{"reaction without a referent", MessageContent{Type: "data", Kind: "reaction"}},
		{"edit of an id from another service", MessageContent{
			Type: "data", Kind: "edit", Text: "nuevo", ReMessageID: "slack:T1:C1:111.222",
		}},
	}
	for _, c := range cases {
		var status int
		var err error
		switch c.content.Kind {
		case "edit":
			_, status, err = buildEdit(session, chat, c.content)
		case "revoke":
			_, status, err = buildRevoke(session, chat, c.content)
		case "reaction":
			_, status, err = buildReaction(session, chat, c.content)
		}
		if err == nil || status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: expected 422, got status %d err %v", c.name, status, err)
		}
	}
}

// An edit with a referent but nothing to say is refused too — WhatsApp has no such
// message, and an empty edit would read as a blank line rather than a correction.
func TestEditRefusesEmptyText(t *testing.T) {
	session := &Session{Address: "5491100000000"}
	chat := types.NewJID("5491199999999", types.DefaultUserServer)
	content := MessageContent{
		Type: "data", Kind: "edit", Text: "   ",
		ReMessageID: externalID("5491100000000", "5491199999999", "5491199999999", "ABC123"),
	}
	if _, status, err := buildEdit(session, chat, content); err == nil ||
		status != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for an empty edit, got status %d err %v", status, err)
	}
}
