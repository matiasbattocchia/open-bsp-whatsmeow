package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A group route acts on a group and nothing else: a person's JID or a bare
// number has no roster to change.
func TestGroupJIDTakesGroupsOnly(t *testing.T) {
	jid, err := groupJID("120363001234567890@g.us")
	if err != nil || jid.Server != types.GroupServer || jid.User != "120363001234567890" {
		t.Fatalf("a group JID should parse: %v %v", jid, err)
	}
	for _, raw := range []string{"5491100000000@s.whatsapp.net", "5491100000000", ""} {
		if _, err := groupJID(raw); err == nil {
			t.Errorf("%q should not pass as a group", raw)
		}
	}
}

// Members arrive as the consumer spells people: digits, or a JID it read off a
// message. Anything else is refused before a query is sent.
func TestMemberJIDs(t *testing.T) {
	jids, err := memberJIDs([]string{"5491100000000", " 5492604586396 ", "", "1234@lid"})
	if err != nil {
		t.Fatalf("memberJIDs: %v", err)
	}
	want := []types.JID{
		types.NewJID("5491100000000", types.DefaultUserServer),
		types.NewJID("5492604586396", types.DefaultUserServer),
		types.NewJID("1234", types.HiddenUserServer),
	}
	if len(jids) != len(want) {
		t.Fatalf("got %v, want %v", jids, want)
	}
	for i := range want {
		if jids[i] != want[i] {
			t.Errorf("seat %d: got %v, want %v", i, jids[i], want[i])
		}
	}
	if _, err := memberJIDs([]string{"+54 9 11"}); err == nil {
		t.Error("a formatted number is not an address")
	}
	if _, err := memberJIDs([]string{"", " "}); err == nil {
		t.Error("nobody named should be refused")
	}
}

// WhatsApp's own answer to a group query is permanent for these bytes, so its
// code rides through — except its 401, which would read as this server's bearer
// refusal; that one is the group's 403. The network stays 502.
func TestGroupErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{whatsmeow.ErrIQNotAuthorized, http.StatusForbidden},
		{whatsmeow.ErrIQForbidden, http.StatusForbidden},
		{whatsmeow.ErrIQNotFound, http.StatusNotFound},
		{&whatsmeow.IQError{Code: 406, Text: "not-acceptable"}, http.StatusNotAcceptable},
		{&whatsmeow.IQError{Code: 500, Text: "internal-server-error"}, http.StatusBadGateway},
		{whatsmeow.ErrIQTimedOut, http.StatusBadGateway},
		{errors.New("websocket closed"), http.StatusBadGateway},
	}
	for _, c := range cases {
		if got := groupErrorStatus(c.err); got != c.want {
			t.Errorf("%v: got %d, want %d", c.err, got, c.want)
		}
	}
	if text := groupErrorText(whatsmeow.ErrIQNotAuthorized); !strings.Contains(text, "not an admin") {
		t.Errorf("a 401 should say what it means for the group: %q", text)
	}
	if text := groupErrorText(whatsmeow.ErrIQNotFound); !strings.Contains(text, "no such group") {
		t.Errorf("a 404 should say what it means for the group: %q", text)
	}
}

// A seat the server would not fill is named with its code and what the code
// means for the person; a filled one is not mentioned.
func TestParticipantFailures(t *testing.T) {
	seats := []types.GroupParticipant{
		{JID: types.NewJID("5491100000001", types.DefaultUserServer)},
		{JID: types.NewJID("5491100000002", types.DefaultUserServer), Error: 403},
		{JID: types.NewJID("5491100000003", types.DefaultUserServer), Error: 409},
		{JID: types.NewJID("5491100000004", types.DefaultUserServer), Error: 500},
	}
	failed := participantFailures(nil, seats)
	if len(failed) != 3 {
		t.Fatalf("got %v", failed)
	}
	if !strings.HasPrefix(failed[0], "5491100000002: 403") || !strings.Contains(failed[0], "invite") {
		t.Errorf("privacy refusal: %q", failed[0])
	}
	if !strings.HasPrefix(failed[1], "5491100000003: 409") || !strings.Contains(failed[1], "already") {
		t.Errorf("already in: %q", failed[1])
	}
	if failed[2] != "5491100000004: 500" {
		t.Errorf("an unknown code is the bare code: %q", failed[2])
	}
}

// A removal names people the way the group holds them: found on the roster by
// canonical digits or by either JID, answered under the roster's own JID. A
// person not on it is reported, not sent.
func TestSeatsOfResolvesAgainstTheRoster(t *testing.T) {
	ana := types.GroupParticipant{
		JID:         types.NewJID("111", types.HiddenUserServer),
		LID:         types.NewJID("111", types.HiddenUserServer),
		PhoneNumber: types.NewJID("5491100000001", types.DefaultUserServer),
	}
	bea := types.GroupParticipant{
		JID:         types.NewJID("5491100000002", types.DefaultUserServer),
		PhoneNumber: types.NewJID("5491100000002", types.DefaultUserServer),
	}
	info := &types.GroupInfo{Participants: []types.GroupParticipant{ana, bea}}
	seats, missing := seatsOf(nil, info, []types.JID{
		types.NewJID("5491100000001", types.DefaultUserServer),
		types.NewJID("5491100000002", types.DefaultUserServer),
		types.NewJID("5491100000009", types.DefaultUserServer),
	})
	if len(seats) != 2 || seats[0] != ana.JID || seats[1] != bea.JID {
		t.Errorf("seats: got %v", seats)
	}
	if len(missing) != 1 || missing[0] != "5491100000009" {
		t.Errorf("missing: got %v", missing)
	}
	seats, missing = seatsOf(nil, info, []types.JID{types.NewJID("111", types.HiddenUserServer)})
	if len(missing) != 0 || len(seats) != 1 || seats[0] != ana.JID {
		t.Errorf("a LID names its seat too: %v %v", seats, missing)
	}
}

// The roster reads as the consumer reads people: canonical digits, the admin
// seats marked, no name when the session keeps no book to ask.
func TestRosterOfIsCanonical(t *testing.T) {
	info := &types.GroupInfo{
		JID:       types.NewJID("120363001234567890", types.GroupServer),
		GroupName: types.GroupName{Name: "ops"},
		Participants: []types.GroupParticipant{
			{
				JID:         types.NewJID("111", types.HiddenUserServer),
				PhoneNumber: types.NewJID("5491100000001", types.DefaultUserServer),
				IsAdmin:     true,
			},
			{
				JID:          types.NewJID("5491100000002", types.DefaultUserServer),
				IsSuperAdmin: true,
			},
			{JID: types.NewJID("5491100000003", types.DefaultUserServer)},
		},
	}
	view := viewOf(&Session{}, info)
	if view.Address != "120363001234567890@g.us" || view.Name != "ops" {
		t.Errorf("view: %+v", view)
	}
	want := []groupMember{
		{Address: "5491100000001", Admin: true},
		{Address: "5491100000002", Admin: true},
		{Address: "5491100000003"},
	}
	if len(view.Members) != len(want) {
		t.Fatalf("members: %+v", view.Members)
	}
	for i := range want {
		if view.Members[i] != want[i] {
			t.Errorf("seat %d: got %+v, want %+v", i, view.Members[i], want[i])
		}
	}
}

// A roster change reads as the consumer reads people: who joined, who left,
// who did it. A change the consumer keeps no line for is not posted.
func TestGroupChange(t *testing.T) {
	group := types.NewJID("120363001234567890", types.GroupServer)
	ana := types.NewJID("5491100000001", types.DefaultUserServer)
	bea := types.NewJID("5491100000002", types.DefaultUserServer)
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	change, ok := groupChange(&Session{}, &events.GroupInfo{
		JID: group, Sender: &ana, Timestamp: at,
		Join: []types.JID{bea},
	})
	if !ok || change.Address != "120363001234567890@g.us" || change.Timestamp != "2026-09-28T12:00:00Z" {
		t.Fatalf("an add: %+v %v", change, ok)
	}
	if len(change.Joined) != 1 || change.Joined[0].Address != "5491100000002" || len(change.Left) != 0 {
		t.Errorf("joined: %+v", change)
	}
	if change.By == nil || change.By.Address != "5491100000001" || change.Reason != "" {
		t.Errorf("by: %+v", change.By)
	}

	change, ok = groupChange(&Session{}, &events.GroupInfo{
		JID: group, Timestamp: at, JoinReason: "invite", Join: []types.JID{bea},
	})
	if !ok || change.By != nil || change.Reason != "invite" {
		t.Errorf("a join by the link: %+v", change)
	}

	change, ok = groupChange(&Session{}, &events.GroupInfo{
		JID: group, Sender: &ana, Timestamp: at, Leave: []types.JID{bea},
	})
	if !ok || len(change.Left) != 1 || change.Left[0].Address != "5491100000002" {
		t.Errorf("a removal: %+v", change)
	}

	change, ok = groupChange(&Session{}, &events.GroupInfo{
		JID: group, Sender: &ana, Timestamp: at, Name: &types.GroupName{Name: "ops"},
	})
	if !ok || change.Name != "ops" || !change.Renamed || change.By == nil ||
		change.By.Address != "5491100000001" || len(change.Joined) != 0 {
		t.Errorf("a rename is the new subject, by whoever set it: %+v", change)
	}

	if _, ok := groupChange(&Session{}, &events.GroupInfo{
		JID: group, Timestamp: at, ParticipantVersionID: "2",
	}); ok {
		t.Error("a notification that moves nothing is no line")
	}
}

// Every field a group's change moves rides its line: the admin seats, the
// about text (deleted is ""), who may send and who may edit, the timer, join
// approval, the link reset, the group deleted, its community, WhatsApp's own
// hand, and what whatsmeow could not read, by tag.
func TestGroupChangeCarriesEveryField(t *testing.T) {
	group := types.NewJID("120363001234567890", types.GroupServer)
	community := types.NewJID("120363009999999999", types.GroupServer)
	ana := types.NewJID("5491100000001", types.DefaultUserServer)
	bea := types.NewJID("5491100000002", types.DefaultUserServer)
	link := "https://chat.whatsapp.com/abc"

	change, ok := groupChange(&Session{}, &events.GroupInfo{
		JID: group, Sender: &ana,
		Promote:                []types.JID{bea},
		Topic:                  &types.GroupTopic{Topic: "Sábado 13 h"},
		Announce:               &types.GroupAnnounce{IsAnnounce: true},
		Locked:                 &types.GroupLocked{IsLocked: false},
		Ephemeral:              &types.GroupEphemeral{IsEphemeral: true, DisappearingTimer: 604800},
		MembershipApprovalMode: &types.GroupMembershipApprovalMode{IsJoinApprovalRequired: true},
		NewInviteLink:          &link,
		Link: &types.GroupLinkChange{
			Type:  types.GroupLinkChangeTypeParent,
			Group: types.GroupLinkTarget{JID: community, GroupName: types.GroupName{Name: "Barrio"}},
		},
	})
	if !ok || change.By == nil || change.By.Address != "5491100000001" {
		t.Fatalf("a change: %+v %v", change, ok)
	}
	if len(change.Promoted) != 1 || change.Promoted[0].Address != "5491100000002" {
		t.Errorf("promoted: %+v", change.Promoted)
	}
	if change.Description == nil || *change.Description != "Sábado 13 h" {
		t.Errorf("description: %v", change.Description)
	}
	if change.Announce == nil || !*change.Announce || change.Locked == nil || *change.Locked {
		t.Errorf("announce and locked are what they became: %v %v", change.Announce, change.Locked)
	}
	if change.Disappearing == nil || *change.Disappearing != 604800 {
		t.Errorf("timer: %v", change.Disappearing)
	}
	if change.Approval == nil || !*change.Approval || change.InviteLink == nil || *change.InviteLink != link {
		t.Errorf("approval and link: %v %v", change.Approval, change.InviteLink)
	}
	if len(change.Linked) != 1 || change.Linked[0] != (roomLink{
		Address: "120363009999999999@g.us", Name: "Barrio", Type: "parent_group",
	}) {
		t.Errorf("linked: %+v", change.Linked)
	}

	change, _ = groupChange(&Session{}, &events.GroupInfo{
		JID: group, Sender: &ana,
		Topic:     &types.GroupTopic{Topic: "old", TopicDeleted: true},
		Ephemeral: &types.GroupEphemeral{IsEphemeral: false, DisappearingTimer: 86400},
	})
	if change.Description == nil || *change.Description != "" || change.Disappearing == nil || *change.Disappearing != 0 {
		t.Errorf("a deleted description is empty and a timer off is 0: %+v", change)
	}

	change, ok = groupChange(&Session{}, &events.GroupInfo{
		JID: group, Suspended: true,
		UnknownChanges: []*waBinary.Node{{Tag: "membership_approval_request"}},
	})
	if !ok || !change.Suspended || change.By != nil ||
		len(change.Unknown) != 1 || change.Unknown[0] != "membership_approval_request" {
		t.Errorf("WhatsApp's own hand, and what nobody could read: %+v", change)
	}

	change, ok = groupChange(&Session{}, &events.GroupInfo{
		JID: group, Sender: &ana, Delete: &types.GroupDelete{Deleted: true},
	})
	if !ok || !change.Deleted {
		t.Errorf("deleted: %+v", change)
	}
}

// The account's own arrival: a new group arrives with its founding roster and
// its creator; an existing one with the account alone.
func TestJoinedGroup(t *testing.T) {
	group := types.NewJID("120363001234567890", types.GroupServer)
	ana := types.NewJID("5491100000001", types.DefaultUserServer)
	own := types.NewJID("5491100000009", types.DefaultUserServer)
	session := &Session{Address: "5491100000009"}

	info := types.GroupInfo{
		JID:          group,
		GroupName:    types.GroupName{Name: "ops"},
		GroupCreated: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		Participants: []types.GroupParticipant{{JID: ana}, {JID: own}},
	}
	change := joinedGroup(session, &events.JoinedGroup{Type: "new", Sender: &ana, GroupInfo: info})
	if change.Name != "ops" || change.Renamed || !change.Created || len(change.Joined) != 2 || change.By == nil ||
		change.By.Address != "5491100000001" || change.Timestamp != "2026-09-28T12:00:00Z" {
		t.Errorf("a new group: %+v", change)
	}

	change = joinedGroup(session, &events.JoinedGroup{Sender: &ana, GroupInfo: info})
	if len(change.Joined) != 1 || change.Joined[0].Address != "5491100000009" || change.By == nil || change.Created {
		t.Errorf("added to a group: %+v", change)
	}

	change = joinedGroup(session, &events.JoinedGroup{Reason: "invite", GroupInfo: info})
	if change.By != nil || change.Reason != "invite" || change.Joined[0].Address != "5491100000009" {
		t.Errorf("in by the link: %+v", change)
	}
}

// A group's facts: its size as WhatsApp counts it, which a big group's partial
// list would undercount; the account an admin under either of its JIDs; and the
// community it sits in.
func TestFactsOf(t *testing.T) {
	community := types.NewJID("120363000000000001", types.GroupServer)
	info := &types.GroupInfo{
		JID:               types.NewJID("120363001234567890", types.GroupServer),
		ParticipantCount:  312,
		GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: community},
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("5491100000001", types.DefaultUserServer), IsSuperAdmin: true},
			{
				JID:         types.NewJID("168723612700722", types.HiddenUserServer),
				PhoneNumber: types.NewJID("5491100000009", types.DefaultUserServer),
				IsAdmin:     true,
			},
		},
	}
	facts := factsOf(info, "5491100000009")
	if facts.members != 312 || !facts.admin || facts.parent != "120363000000000001@g.us" {
		t.Errorf("by number: %+v", facts)
	}
	if facts := factsOf(info, "5491100000007", "168723612700722"); !facts.admin {
		t.Errorf("an admin seat held under the LID is the account's too: %+v", facts)
	}
	if facts := factsOf(info, "5491100000007"); facts.admin {
		t.Errorf("somebody else's admin seat is not the account's: %+v", facts)
	}

	small := &types.GroupInfo{
		JID: types.NewJID("120363001234567891", types.GroupServer),
		Participants: []types.GroupParticipant{
			{JID: types.NewJID("5491100000001", types.DefaultUserServer), IsAdmin: true},
			{JID: types.NewJID("5491100000009", types.DefaultUserServer)},
		},
	}
	if facts := factsOf(small, "5491100000009"); facts.members != 2 || facts.admin || facts.parent != "" {
		t.Errorf("a plain seat in a group of two, in no community: %+v", facts)
	}
}

// A group is read once and kept: its facts, its subject, and its community's
// subject, read with it. A change forgets the facts, and the next line reads
// them again; a group that cannot be read keeps nothing.
func TestReadGroupKeepsWhatItRead(t *testing.T) {
	group := types.NewJID("120363001234567890", types.GroupServer)
	community := types.NewJID("120363000000000001", types.GroupServer)
	reads := map[string]int{}
	members := 25
	read := func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		reads[jid.String()]++
		if jid == community {
			return &types.GroupInfo{JID: community, GroupName: types.GroupName{Name: "Obra"}}, nil
		}
		return &types.GroupInfo{
			JID:               group,
			GroupName:         types.GroupName{Name: "Ventas"},
			ParticipantCount:  members,
			GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: community},
		}, nil
	}
	session := &Session{Address: "5491100000009"}

	facts, ok := readGroup(session, group, read)
	if !ok || facts.members != 25 || facts.admin || facts.parent != community.String() {
		t.Fatalf("first read: %+v %v", facts, ok)
	}
	if session.groupName(group.String()) != "Ventas" || session.groupName(community.String()) != "Obra" {
		t.Errorf("names: %q %q", session.groupName(group.String()), session.groupName(community.String()))
	}
	readGroup(session, group, read)
	if reads[group.String()] != 1 || reads[community.String()] != 1 {
		t.Errorf("a kept group is not read again: %v", reads)
	}

	var message WebhookMessage
	stampGroup(session, &message, facts)
	if message.ConversationMembers != 25 || message.ConversationAdmin || message.ConversationCommunity != "Obra" {
		t.Errorf("stamped: %+v", message)
	}

	members = 26
	session.forgetGroup(group.String())
	if facts, _ := readGroup(session, group, read); facts.members != 26 || reads[group.String()] != 2 {
		t.Errorf("a forgotten group is read afresh: %+v %v", facts, reads)
	}
	if reads[community.String()] != 1 {
		t.Errorf("a community already named is not read again: %v", reads)
	}

	failing := func(context.Context, types.JID) (*types.GroupInfo, error) {
		return nil, errors.New("timed out")
	}
	other := types.NewJID("120363001234567899", types.GroupServer)
	if _, ok := readGroup(session, other, failing); ok {
		t.Error("an unreadable group has no facts")
	}
	if _, ok := session.cachedGroup(other.String()); ok {
		t.Error("an unreadable group keeps nothing")
	}
}

// The history import reads every group at once, and names each community the
// list leaves unnamed with a read of its own.
func TestReadAllGroups(t *testing.T) {
	ventas := types.NewJID("120363001234567890", types.GroupServer)
	asado := types.NewJID("120363001234567891", types.GroupServer)
	community := types.NewJID("120363000000000001", types.GroupServer)
	list := func(context.Context) ([]*types.GroupInfo, error) {
		return []*types.GroupInfo{
			{
				JID: ventas, GroupName: types.GroupName{Name: "Ventas"},
				GroupLinkedParent: types.GroupLinkedParent{LinkedParentJID: community},
				Participants: []types.GroupParticipant{
					{JID: types.NewJID("5491100000009", types.DefaultUserServer), IsAdmin: true},
					{JID: types.NewJID("5491100000001", types.DefaultUserServer)},
				},
			},
			{JID: asado, GroupName: types.GroupName{Name: "Asado"}, ParticipantCount: 40},
		}, nil
	}
	var read []types.JID
	reader := func(_ context.Context, jid types.JID) (*types.GroupInfo, error) {
		read = append(read, jid)
		return &types.GroupInfo{JID: jid, GroupName: types.GroupName{Name: "Obra"}}, nil
	}
	session := &Session{Address: "5491100000009"}
	if err := readAllGroups(session, list, reader); err != nil {
		t.Fatal(err)
	}
	if facts, ok := session.cachedGroup(ventas.String()); !ok || facts.members != 2 || !facts.admin ||
		session.groupName(facts.parent) != "Obra" {
		t.Errorf("ventas: %+v %v", facts, ok)
	}
	if facts, ok := session.cachedGroup(asado.String()); !ok || facts.members != 40 || facts.parent != "" {
		t.Errorf("asado: %+v %v", facts, ok)
	}
	if len(read) != 1 || read[0] != community {
		t.Errorf("only the unnamed community is read on its own: %v", read)
	}
}
