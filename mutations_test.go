package main

import (
	"net/http"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
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

// Who a contact IS, in the order the account would answer: the name it wrote in
// its own address book beats what the contact calls itself, and the live event's
// pushname beats the stored copy of the same fact.
func TestPickNamePrefersTheAddressBook(t *testing.T) {
	cases := []struct {
		name    string
		contact types.ContactInfo
		live    string
		want    string
	}{
		{"address book wins", types.ContactInfo{FullName: "Gianvito", PushName: "gv 🇮🇹"}, "gv 🇮🇹", "Gianvito"},
		{"first name when that is all there is", types.ContactInfo{FirstName: "Ana", PushName: "any"}, "", "Ana"},
		{"live pushname beats the stored one", types.ContactInfo{PushName: "old"}, "new", "new"},
		{"business name is the last resort", types.ContactInfo{BusinessName: "Ferretería"}, "", "Ferretería"},
		{"blank is blank — the consumer falls back to the address", types.ContactInfo{}, "   ", ""},
	}
	for _, c := range cases {
		if got := pickName(c.contact, c.live); got != c.want {
			t.Errorf("%s: pickName = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every id the bridge mints names the chat in ONE namespace — the canonical one the
// consumer stores. A LID-addressed chat speaks lids on the wire, and an id built from the
// lid matches nothing the other side wrote: our own sends address the peer by number, so
// a reply-to, a reaction and a receipt all pointed at a chat that did not exist there.
func TestChatSegmentIsCanonicalInBothNamespaces(t *testing.T) {
	session := &Session{Address: "5491100000000"}
	phone := types.NewJID("5491199999999", types.DefaultUserServer)
	lid := types.NewJID("168723612700722", types.HiddenUserServer)

	byPhone := chatSegment(session, types.MessageSource{Chat: phone})
	byLID := chatSegment(session, types.MessageSource{Chat: lid, SenderAlt: phone})
	if byPhone != "5491199999999" || byLID != byPhone {
		t.Fatalf("chat segment: phone %q, lid %q — want both %q", byPhone, byLID, "5491199999999")
	}
	// a group is already its own namespace: the JID user, untouched
	group := types.NewJID("120363429869958481", types.GroupServer)
	if got := chatSegment(session, types.MessageSource{Chat: group, IsGroup: true}); got != group.User {
		t.Errorf("group chat segment = %q, want %q", got, group.User)
	}
}

// An edit is where an @name most often arrives — the original was sent half-typed and
// corrected a second later. What the consumer needs is the new text with the mention
// resolved beside it; both wire shapes (protocol message, message-secret envelope)
// converge here, so this is the one place that has to be right.
func TestEditBodyCarriesTextAndMentions(t *testing.T) {
	session := &Session{Address: "5491100000000"}
	edited := extendedTextWithMentions(
		"asado en tu casa el domingo @5491100000001 ?",
		"5491100000001@s.whatsapp.net",
	)

	body, mentions, ok := editBody(session, edited)
	if !ok {
		t.Fatalf("an edit with text must publish")
	}
	if body != "asado en tu casa el domingo @5491100000001 ?" {
		t.Fatalf("the new text must survive verbatim, got %q", body)
	}
	if len(mentions) != 1 || mentions[0].Address != "5491100000001" {
		t.Fatalf("the edit must name who it mentions, got %+v", mentions)
	}
}

// Nothing readable means nothing to publish: an edit of a kind we cannot render must
// not reach the consumer as an empty message that blanks the original.
func TestEditBodyRefusesUnreadableContent(t *testing.T) {
	session := &Session{Address: "5491100000000"}
	if _, _, ok := editBody(session, &waE2E.Message{}); ok {
		t.Fatalf("an edit with no readable text must not publish")
	}
}

// A suspended host is one whose wall clock ran on while its timers did not. The tick
// itself accounts for some of the gap; anything well past it is sleep, and the sockets
// from before it are dead whether or not they say so.
func TestHostSleptReadsTheWallClockGap(t *testing.T) {
	base := time.Date(2026, 9, 10, 12, 46, 49, 0, time.UTC)

	if hostSlept(base, base.Add(SLEEP_TICK)) {
		t.Fatalf("a tick that lands on time is not a sleep")
	}
	if hostSlept(base, base.Add(SLEEP_TICK+SLEEP_GAP-time.Second)) {
		t.Fatalf("a late tick inside the margin is not a sleep")
	}
	// the real one: 45 minutes on the lid, 2026-09-10
	if !hostSlept(base, base.Add(45*time.Minute)) {
		t.Fatalf("45 minutes of wall clock with no ticks is a sleep")
	}
}

// A queue drained after downtime is ONE arrival: held while it drains, handed back whole
// and in order, and nothing held once it has been handed over. Posted message by message
// instead, a backlog reads to the consumer as a conversation happening now.
func TestOfflineQueueIsHeldAndHandedBackWhole(t *testing.T) {
	s := &Session{Address: "5491100000000"}
	one := WebhookBatch{Messages: []WebhookMessage{{ExternalID: "a"}}}

	// with no drain open, a batch is the caller's to post
	if s.holdOffline(one) {
		t.Fatalf("a live message must not be held")
	}

	s.beginDrain(3)
	for _, id := range []string{"a", "b", "c"} {
		if !s.holdOffline(WebhookBatch{Messages: []WebhookMessage{{ExternalID: id}}}) {
			t.Fatalf("a queued message must be held")
		}
	}

	held := s.endDrain()
	if len(held) != 3 || held[0].ExternalID != "a" || held[2].ExternalID != "c" {
		t.Fatalf("the queue must come back whole and in order, got %+v", held)
	}
	if again := s.endDrain(); len(again) != 0 {
		t.Fatalf("a drained queue holds nothing, got %+v", again)
	}
	if s.holdOffline(one) {
		t.Fatalf("the drain is closed — live messages post again")
	}
}
