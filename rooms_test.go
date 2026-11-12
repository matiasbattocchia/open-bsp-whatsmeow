package main

import (
	"testing"
	"time"

	waBinary "go.mau.fi/whatsmeow/binary"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var (
	roomAt  = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	anaJID  = types.NewJID("5491100000001", types.DefaultUserServer)
	beaJID  = types.NewJID("5491100000002", types.DefaultUserServer)
	groupID = types.NewJID("120363001234567890", types.GroupServer)
)

// The account's own view of a chat is a line in that chat, by nobody: the phone
// archived it, muted it until a time, pinned it, read it, cleared or deleted it.
func TestViewChange(t *testing.T) {
	session := &Session{}
	room, ok := viewChange(session, &events.Archive{
		JID: anaJID, Timestamp: roomAt,
		Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)},
	})
	if !ok || room.Address != "5491100000001" || room.By != nil || room.Timestamp != "2026-11-11T12:00:00Z" ||
		room.View == nil || room.View.Archived == nil || !*room.View.Archived {
		t.Fatalf("archived: %+v %v", room, ok)
	}
	if !silences(room) {
		t.Error("an archive is the news of the silence")
	}

	until := roomAt.Add(8 * time.Hour)
	room, _ = viewChange(session, &events.Mute{
		JID: groupID, Timestamp: roomAt,
		Action: &waSyncAction.MuteAction{Muted: proto.Bool(true), MuteEndTimestamp: proto.Int64(until.UnixMilli())},
	})
	if room.Address != "120363001234567890@g.us" || !*room.View.Muted || room.View.Until != "2026-11-11T20:00:00Z" {
		t.Errorf("muted until: %+v", room.View)
	}
	room, _ = viewChange(session, &events.Mute{
		JID: groupID, Timestamp: roomAt,
		Action: &waSyncAction.MuteAction{Muted: proto.Bool(true), MuteEndTimestamp: proto.Int64(-1)},
	})
	if !*room.View.Muted || room.View.Until != "" {
		t.Errorf("a mute always has no end: %+v", room.View)
	}

	room, _ = viewChange(session, &events.MarkChatAsRead{
		JID: anaJID, Timestamp: roomAt,
		Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(true)},
	})
	if room.View.Read == nil || !*room.View.Read || silences(room) {
		t.Errorf("read: %+v", room.View)
	}
	room, _ = viewChange(session, &events.Pin{
		JID: anaJID, Timestamp: roomAt, Action: &waSyncAction.PinAction{Pinned: proto.Bool(false)},
	})
	if room.View.Pinned == nil || *room.View.Pinned {
		t.Errorf("unpinned: %+v", room.View)
	}
	room, _ = viewChange(session, &events.ClearChat{JID: anaJID, Timestamp: roomAt})
	if !room.View.Cleared {
		t.Errorf("cleared: %+v", room.View)
	}
	room, _ = viewChange(session, &events.DeleteChat{JID: anaJID, Timestamp: roomAt})
	if !room.View.Deleted {
		t.Errorf("deleted: %+v", room.View)
	}

	if _, ok := viewChange(session, &events.Archive{
		JID: types.StatusBroadcastJID, Action: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)},
	}); ok {
		t.Error("Status is no conversation")
	}
}

// The chat lock reaches us only as the bare mutation; every other bare mutation
// is said by its typed event, and is no line here.
func TestViewChangeLock(t *testing.T) {
	room, ok := viewChange(&Session{}, &events.AppState{
		Index: []string{"lock", anaJID.String()},
		SyncActionValue: &waSyncAction.SyncActionValue{
			Timestamp:      proto.Int64(roomAt.UnixMilli()),
			LockChatAction: &waSyncAction.LockChatAction{Locked: proto.Bool(true)},
		},
	})
	if !ok || room.Address != "5491100000001" || room.View.Locked == nil || !*room.View.Locked ||
		room.Timestamp != "2026-11-11T12:00:00Z" {
		t.Errorf("locked: %+v %v", room, ok)
	}
	if _, ok := viewChange(&Session{}, &events.AppState{
		Index: []string{"archive", anaJID.String()},
		SyncActionValue: &waSyncAction.SyncActionValue{
			ArchiveChatAction: &waSyncAction.ArchiveChatAction{Archived: proto.Bool(true)},
		},
	}); ok {
		t.Error("an archive is said by its own event, not twice")
	}
}

// A label is named by its last edit, and by its id before any edit named it.
func TestViewChangeLabels(t *testing.T) {
	session := &Session{}
	label := func(id string, on bool) *roomLabels {
		room, _ := viewChange(session, &events.LabelAssociationChat{
			JID: anaJID, Timestamp: roomAt, LabelID: id,
			Action: &waSyncAction.LabelAssociationAction{Labeled: proto.Bool(on)},
		})
		return room.View.Labels
	}
	if got := label("3", true); len(got.Added) != 1 || got.Added[0] != "3" {
		t.Errorf("an unnamed label is its id: %+v", got)
	}
	session.rooms.noteLabel("3", "Pendiente", false)
	if got := label("3", false); len(got.Removed) != 1 || got.Removed[0] != "Pendiente" {
		t.Errorf("a named label: %+v", got)
	}
	session.rooms.noteLabel("3", "", true)
	if got := label("3", true); got.Added[0] != "3" {
		t.Errorf("a deleted label is forgotten: %+v", got)
	}
}

// Favorites arrive as the whole list: the first one is the baseline, and each
// later one says which chats came onto it and which left.
func TestFavoriteChanges(t *testing.T) {
	session := &Session{}
	list := func(jids ...types.JID) *events.AppState {
		favorites := make([]*waSyncAction.FavoritesAction_Favorite, 0, len(jids))
		for _, jid := range jids {
			favorites = append(favorites, &waSyncAction.FavoritesAction_Favorite{ID: proto.String(jid.String())})
		}
		return &events.AppState{
			Index: []string{"favorites"},
			SyncActionValue: &waSyncAction.SyncActionValue{
				Timestamp:       proto.Int64(roomAt.UnixMilli()),
				FavoritesAction: &waSyncAction.FavoritesAction{Favorites: favorites},
			},
		}
	}
	if rooms := favoriteChanges(session, list(anaJID)); len(rooms) != 0 {
		t.Errorf("the first list is the baseline: %+v", rooms)
	}
	rooms := favoriteChanges(session, list(groupID))
	if len(rooms) != 2 {
		t.Fatalf("one on, one off: %+v", rooms)
	}
	for _, room := range rooms {
		switch room.Address {
		case "120363001234567890@g.us":
			if !*room.View.Favorite {
				t.Errorf("came on: %+v", room.View)
			}
		case "5491100000001":
			if *room.View.Favorite {
				t.Errorf("left: %+v", room.View)
			}
		default:
			t.Errorf("unexpected chat: %+v", room)
		}
	}
	if rooms := favoriteChanges(session, list(groupID)); len(rooms) != 0 {
		t.Errorf("the same list moves nothing: %+v", rooms)
	}
}

// A block is a line in the blocked contact's direct chat.
func TestBlockChanges(t *testing.T) {
	rooms := blockChanges(&Session{}, &events.Blocklist{Changes: []events.BlocklistChange{
		{JID: anaJID, Action: events.BlocklistChangeActionBlock},
		{JID: beaJID, Action: events.BlocklistChangeActionUnblock},
	}})
	if len(rooms) != 2 || rooms[0].Address != "5491100000001" || !*rooms[0].View.Blocked ||
		rooms[1].Address != "5491100000002" || *rooms[1].View.Blocked {
		t.Errorf("blocks: %+v", rooms)
	}
	if rooms := blockChanges(&Session{}, &events.Blocklist{Action: events.BlocklistActionModify}); len(rooms) != 0 {
		t.Errorf("a change that names nobody is no line: %+v", rooms)
	}
}

// A group's photo is the group's line, by whoever changed it; a person's is not.
func TestPictureChange(t *testing.T) {
	room, ok := pictureChange(&Session{}, &events.Picture{JID: groupID, Author: anaJID, Timestamp: roomAt})
	if !ok || room.Picture != "set" || room.By == nil || room.By.Address != "5491100000001" {
		t.Errorf("set: %+v %v", room, ok)
	}
	room, _ = pictureChange(&Session{}, &events.Picture{JID: groupID, Author: anaJID, Remove: true})
	if room.Picture != "removed" {
		t.Errorf("removed: %+v", room)
	}
	if _, ok := pictureChange(&Session{}, &events.Picture{JID: anaJID, Author: anaJID}); ok {
		t.Error("a person's photo is no room's line")
	}
}

// A direct chat's timer is its line, by whoever set it; a group's is the group's
// own change and not said twice.
func TestDisappearingChange(t *testing.T) {
	session := &Session{Address: "5491100000009"}
	pm := &waE2E.ProtocolMessage{
		Type:                waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(),
		EphemeralExpiration: proto.Uint32(86400),
	}
	evt := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: anaJID, Sender: anaJID},
		Timestamp:     roomAt,
	}}
	room, ok := disappearingChange(session, evt, pm)
	if !ok || room.Address != "5491100000001" || room.By == nil || room.By.Address != "5491100000001" ||
		room.Disappearing == nil || *room.Disappearing != 86400 {
		t.Errorf("a DM's timer: %+v %v", room, ok)
	}

	evt.Info.MessageSource = types.MessageSource{Chat: groupID, Sender: anaJID, IsGroup: true}
	if _, ok := disappearingChange(session, evt, pm); ok {
		t.Error("a group's timer is its GroupInfo")
	}
}

// A call is one line when it is over: ended with its length from the answer,
// missed when nobody answered, rejected; a group call is said when it starts.
func TestCallChange(t *testing.T) {
	session := &Session{}
	meta := func(id string, at time.Time) types.BasicCallMeta {
		return types.BasicCallMeta{From: anaJID, CallCreator: anaJID, CallID: id, Timestamp: at}
	}
	video := &waBinary.Node{Tag: "offer", Content: []waBinary.Node{{Tag: "audio"}, {Tag: "video"}}}

	if _, ok := callChange(session, &events.CallOffer{BasicCallMeta: meta("c1", roomAt), Data: video}); ok {
		t.Error("a call ringing is no line yet")
	}
	callChange(session, &events.CallAccept{BasicCallMeta: meta("c1", roomAt.Add(5*time.Second))})
	room, ok := callChange(session, &events.CallTerminate{BasicCallMeta: meta("c1", roomAt.Add(317*time.Second))})
	if !ok || room.Address != "5491100000001" || room.By == nil || room.By.Address != "5491100000001" ||
		*room.Call != (roomCall{State: "ended", Kind: "video", Seconds: 312}) {
		t.Errorf("ended: %+v %+v", room, room.Call)
	}

	callChange(session, &events.CallOffer{BasicCallMeta: meta("c2", roomAt)})
	room, _ = callChange(session, &events.CallTerminate{BasicCallMeta: meta("c2", roomAt.Add(30*time.Second))})
	if *room.Call != (roomCall{State: "missed", Kind: "audio"}) {
		t.Errorf("missed: %+v", room.Call)
	}

	callChange(session, &events.CallOffer{BasicCallMeta: meta("c3", roomAt)})
	room, ok = callChange(session, &events.CallReject{BasicCallMeta: meta("c3", roomAt)})
	if !ok || *room.Call != (roomCall{State: "rejected", Kind: "audio"}) || room.By.Address != "5491100000001" {
		t.Errorf("rejected: %+v", room.Call)
	}
	if _, ok := callChange(session, &events.CallTerminate{BasicCallMeta: meta("c3", roomAt)}); ok {
		t.Error("a rejected call's end says nothing more")
	}

	notice := meta("c4", roomAt)
	notice.GroupJID = groupID
	room, ok = callChange(session, &events.CallOfferNotice{BasicCallMeta: notice, Media: "video", Type: "group"})
	if !ok || room.Address != "120363001234567890@g.us" || *room.Call != (roomCall{State: "started", Kind: "video"}) {
		t.Errorf("a group call: %+v", room)
	}
}

// The view a chat had at linking is one line after its history; a chat with none
// says nothing.
func TestHistoryView(t *testing.T) {
	until := roomAt.Add(time.Hour)
	room, ok := historyView("5491100000001", &waHistorySync.Conversation{
		Archived:              proto.Bool(true),
		MuteEndTime:           proto.Uint64(uint64(until.Unix())),
		ConversationTimestamp: proto.Uint64(uint64(roomAt.Add(-time.Hour).Unix())),
	}, roomAt)
	if !ok || !*room.View.Archived || !*room.View.Muted || room.View.Until != "2026-11-11T13:00:00Z" ||
		room.View.Pinned != nil || room.Timestamp != "2026-11-11T11:00:00Z" {
		t.Errorf("archived and muted: %+v %+v", room, room.View)
	}
	room, _ = historyView("5491100000001", &waHistorySync.Conversation{
		MuteEndTime: proto.Uint64(^uint64(0)), Pinned: proto.Uint32(1),
	}, roomAt)
	if !*room.View.Muted || room.View.Until != "" || !*room.View.Pinned {
		t.Errorf("muted always and pinned: %+v", room.View)
	}
	if _, ok := historyView("5491100000001", &waHistorySync.Conversation{
		MuteEndTime: proto.Uint64(uint64(roomAt.Add(-time.Hour).Unix())),
	}, roomAt); ok {
		t.Error("a mute already over is no view")
	}
}
