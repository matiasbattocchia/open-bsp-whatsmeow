package main

import (
	"sync"
	"time"

	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A room's line is what a chat says about itself rather than in it: a group's
// change (groups.go), a call, the disappearing timer of a direct chat, a group's
// picture, and how the account's own app shows the chat — archived, muted,
// pinned, read, cleared, deleted, labelled, locked, a favorite, its peer
// blocked. Each rides the batch's rooms feed (WebhookRoom).
//
// The account's own view comes from app state: the phone changes a chat, and
// the change syncs to this device as a mutation. A pairing's first full sync is
// not announced as events (whatsmeow keeps the settings and says nothing), so
// the view a chat already had at linking comes with the history instead, one
// line per chat that has one (historyView).

// roomMemory is what a line needs from events before it, kept for the process
// lifetime: a call rings in one event and ends in another; a label is named in
// one mutation and put on a chat in the next; favorites arrive as the whole
// list, and only the last one says which chat moved.
type roomMemory struct {
	mu        sync.Mutex
	calls     map[string]ringing
	labels    map[string]string
	favorites map[string]bool
}

// ringing is a call offered to the account and not yet over.
type ringing struct {
	chat     string
	caller   groupMember
	kind     string
	offered  time.Time
	answered time.Time
}

// CALL_MEMORY bounds how long a call that never ended is remembered: WhatsApp
// ends every call it offers, so one still ringing after this lost its end.
const CALL_MEMORY = 24 * time.Hour

// chatAddress is a chat as the feeds address it: the JID of a group, a
// broadcast list or a channel, a person's canonical digits. ok is false for
// what is no conversation here — Status, and a channel the account only
// follows, are dropped wherever they appear.
func chatAddress(session *Session, jid types.JID) (address string, ok bool) {
	jid = jid.ToNonAD()
	if jid.IsEmpty() || jid == types.StatusBroadcastJID {
		return "", false
	}
	if isChannel(jid) && !runsChannel(session, jid) {
		return "", false
	}
	if chatByJID(types.MessageSource{Chat: jid}) {
		return jid.String(), true
	}
	return canonicalUser(session, jid, types.JID{}), true
}

// viewChange shapes a change the account made to its own view of a chat, from
// the app-state event that announced it. ok is false for any other event, and
// for a lock mutation that names no chat.
func viewChange(session *Session, evt any) (room WebhookRoom, ok bool) {
	view := &roomView{}
	var chat types.JID
	var at time.Time
	switch v := evt.(type) {
	case *events.Archive:
		chat, at = v.JID, v.Timestamp
		view.Archived = new(v.Action.GetArchived())
	case *events.Mute:
		chat, at = v.JID, v.Timestamp
		view.Muted = new(v.Action.GetMuted())
		// a mute "always" ends at -1
		if end := v.Action.GetMuteEndTimestamp(); v.Action.GetMuted() && end > 0 {
			view.Until = time.UnixMilli(end).UTC().Format(time.RFC3339)
		}
	case *events.Pin:
		chat, at = v.JID, v.Timestamp
		view.Pinned = new(v.Action.GetPinned())
	case *events.MarkChatAsRead:
		chat, at = v.JID, v.Timestamp
		view.Read = new(v.Action.GetRead())
	case *events.ClearChat:
		chat, at = v.JID, v.Timestamp
		view.Cleared = true
	case *events.DeleteChat:
		chat, at = v.JID, v.Timestamp
		view.Deleted = true
	case *events.LabelAssociationChat:
		chat, at = v.JID, v.Timestamp
		label := session.rooms.labelName(v.LabelID)
		if v.Action.GetLabeled() {
			view.Labels = &roomLabels{Added: []string{label}}
		} else {
			view.Labels = &roomLabels{Removed: []string{label}}
		}
	case *events.AppState:
		// The chat lock has no event of its own: it reaches us as the bare mutation,
		// indexed [lock, chat].
		if len(v.Index) < 2 || v.Index[0] != appstate.IndexLock || v.GetLockChatAction() == nil {
			return room, false
		}
		chat, _ = types.ParseJID(v.Index[1])
		at = time.UnixMilli(v.GetTimestamp())
		view.Locked = new(v.GetLockChatAction().GetLocked())
	default:
		return room, false
	}
	address, ok := chatAddress(session, chat)
	if !ok {
		return room, false
	}
	return WebhookRoom{
		Address:   address,
		View:      view,
		Timestamp: changedAt(at).Format(time.RFC3339),
	}, true
}

// favoriteChanges reads the favorites mutation, which carries the whole list:
// each chat that came onto it or left it since the last one is a line. The first
// list a process sees has nothing to compare with and only sets the baseline.
func favoriteChanges(session *Session, v *events.AppState) []WebhookRoom {
	if len(v.Index) < 1 || v.Index[0] != appstate.IndexFavorites || v.GetFavoritesAction() == nil {
		return nil
	}
	now := map[string]bool{}
	for _, favorite := range v.GetFavoritesAction().GetFavorites() {
		jid, err := types.ParseJID(favorite.GetID())
		if err != nil {
			continue
		}
		if address, ok := chatAddress(session, jid); ok {
			now[address] = true
		}
	}
	before, known := session.rooms.swapFavorites(now)
	if !known {
		return nil
	}
	at := changedAt(time.UnixMilli(v.GetTimestamp())).Format(time.RFC3339)
	var rooms []WebhookRoom
	for address := range now {
		if !before[address] {
			rooms = append(rooms, WebhookRoom{Address: address, View: &roomView{Favorite: new(true)}, Timestamp: at})
		}
	}
	for address := range before {
		if !now[address] {
			rooms = append(rooms, WebhookRoom{Address: address, View: &roomView{Favorite: new(false)}, Timestamp: at})
		}
	}
	return rooms
}

// blockChanges is a line in the direct chat of each contact the account blocked
// or unblocked. A blocklist that only says it changed, without the changes, names
// no chat and is no line.
func blockChanges(session *Session, v *events.Blocklist) []WebhookRoom {
	at := changedAt(time.Time{}).Format(time.RFC3339)
	var rooms []WebhookRoom
	for _, change := range v.Changes {
		address, ok := chatAddress(session, change.JID)
		if !ok {
			continue
		}
		blocked := change.Action == events.BlocklistChangeActionBlock
		rooms = append(rooms, WebhookRoom{Address: address, View: &roomView{Blocked: &blocked}, Timestamp: at})
	}
	return rooms
}

// pictureChange is a group's photo set or removed, by whoever did it. A person's
// own photo is no line: it is about them, not about a room.
func pictureChange(session *Session, v *events.Picture) (room WebhookRoom, ok bool) {
	if v.JID.Server != types.GroupServer {
		return room, false
	}
	room = WebhookRoom{
		Address:   v.JID.String(),
		Picture:   "set",
		Timestamp: changedAt(v.Timestamp).Format(time.RFC3339),
	}
	if v.Remove {
		room.Picture = "removed"
	}
	if !v.Author.IsEmpty() {
		by := personOf(session, v.Author, types.JID{}, "")
		room.By = &by
	}
	return room, true
}

// disappearingChange is a direct chat's message timer turned on or off, by
// whoever set it. A group's timer is the group's own change (GroupInfo) and its
// protocol message would only say it twice.
func disappearingChange(session *Session, evt *events.Message, pm *waE2E.ProtocolMessage) (room WebhookRoom, ok bool) {
	if pm.GetType() != waE2E.ProtocolMessage_EPHEMERAL_SETTING || evt.Info.IsGroup {
		return room, false
	}
	timer := pm.GetEphemeralExpiration()
	by := groupMember{Address: senderAddressFor(session, evt.Info.MessageSource)}
	return WebhookRoom{
		Address:      conversationAddressFor(session, evt.Info.MessageSource),
		By:           &by,
		Disappearing: &timer,
		Timestamp:    changedAt(evt.Info.Timestamp).Format(time.RFC3339),
	}, true
}

// callKind reads an offer's media: a video call offers a video stream beside
// the audio.
func callKind(offer *events.CallOffer) string {
	if offer.Data != nil {
		if _, ok := offer.Data.GetOptionalChildByTag("video"); ok {
			return "video"
		}
	}
	return "audio"
}

// callChat is where a call is said: the group it rings in, else the direct chat
// with whoever placed it.
func callChat(session *Session, meta types.BasicCallMeta) string {
	if !meta.GroupJID.IsEmpty() {
		return meta.GroupJID.ToNonAD().String()
	}
	return canonicalUser(session, meta.CallCreator.ToNonAD(), meta.CallCreatorAlt)
}

// callChange follows a call from its offer to its end, and answers the line
// when it has one: ended, with how long it lasted from its answer; missed, when
// nobody answered; rejected; or a group call started, which rings nobody here
// and is said when it is offered. ok is false while a call is still ringing,
// and for the end of a call this process never saw offered.
func callChange(session *Session, evt any) (room WebhookRoom, ok bool) {
	memory := &session.rooms
	switch v := evt.(type) {
	case *events.CallOffer:
		memory.ring(v.CallID, ringing{
			chat:    callChat(session, v.BasicCallMeta),
			caller:  personOf(session, v.CallCreator.ToNonAD(), v.CallCreatorAlt, ""),
			kind:    callKind(v),
			offered: v.Timestamp,
		})
		return room, false
	case *events.CallAccept:
		memory.answer(v.CallID, v.Timestamp)
		return room, false
	case *events.CallTerminate:
		call, known := memory.hangUp(v.CallID)
		if !known {
			return room, false
		}
		line := roomCall{State: "missed", Kind: call.kind}
		if !call.answered.IsZero() {
			line.State = "ended"
			line.Seconds = int(v.Timestamp.Sub(call.answered).Round(time.Second) / time.Second)
		}
		return WebhookRoom{
			Address:   call.chat,
			By:        &call.caller,
			Call:      &line,
			Timestamp: changedAt(v.Timestamp).Format(time.RFC3339),
		}, true
	case *events.CallReject:
		call, known := memory.hangUp(v.CallID)
		if !known {
			call.chat = callChat(session, v.BasicCallMeta)
		}
		by := personOf(session, v.From.ToNonAD(), types.JID{}, "")
		return WebhookRoom{
			Address:   call.chat,
			By:        &by,
			Call:      &roomCall{State: "rejected", Kind: call.kind},
			Timestamp: changedAt(v.Timestamp).Format(time.RFC3339),
		}, true
	case *events.CallOfferNotice:
		by := personOf(session, v.CallCreator.ToNonAD(), v.CallCreatorAlt, "")
		return WebhookRoom{
			Address:   callChat(session, v.BasicCallMeta),
			By:        &by,
			Call:      &roomCall{State: "started", Kind: v.Media},
			Timestamp: changedAt(v.Timestamp).Format(time.RFC3339),
		}, true
	}
	return room, false
}

// historyView is the view a chat already had when the account was linked, read
// off the history sync: archived, muted while its mute lasts, pinned. ok is
// false for a chat with none of them, which says nothing.
func historyView(address string, conversation *waHistorySync.Conversation, now time.Time) (room WebhookRoom, ok bool) {
	view := &roomView{}
	if conversation.GetArchived() {
		view.Archived = new(true)
	}
	// seconds; a mute "always" ends past any date a clock reads
	if end := conversation.GetMuteEndTime(); end > uint64(now.Unix()) {
		view.Muted = new(true)
		if end < uint64(store.MutedForever.Unix()) {
			view.Until = time.Unix(int64(end), 0).UTC().Format(time.RFC3339)
		}
	}
	if conversation.GetPinned() > 0 {
		view.Pinned = new(true)
	}
	if view.Archived == nil && view.Muted == nil && view.Pinned == nil {
		return room, false
	}
	at := now
	if ts := conversation.GetConversationTimestamp(); ts > 0 {
		at = time.Unix(int64(ts), 0)
	}
	return WebhookRoom{Address: address, View: view, Timestamp: at.UTC().Format(time.RFC3339)}, true
}

// silences is whether a line is the news that the chat was archived or muted,
// or no longer is. Such a line carries no marks: it would hide itself.
func silences(room WebhookRoom) bool {
	return room.View != nil && (room.View.Archived != nil || room.View.Muted != nil)
}

// postRoom posts a room's line on the rooms feed, named as the room is named
// now. A rename keeps the subject it replaces as Was before the new one is
// noted, so the messages after it carry the name it now has.
func (m *Manager) postRoom(session *Session, room WebhookRoom) {
	if session.Address == "" {
		return // still pairing
	}
	chat, err := types.ParseJID(room.Address)
	group := err == nil && chat.Server == types.GroupServer
	switch {
	case room.Renamed:
		if was := session.groupName(room.Address); was != room.Name {
			room.Was = was
		}
	case room.Name == "" && group:
		room.Name = session.groupName(room.Address)
	case room.Name == "" && err == nil && isChannel(chat):
		room.Name = session.channelName(room.Address)
	case room.Name == "" && session.Client != nil && session.Client.Store != nil:
		// a direct chat is its peer, and the peer's name names it
		room.Name, _ = contactName(session, room.Address, "")
	}
	if group {
		session.noteGroupName(room.Address, room.Name)
		// read after the rename has kept the subject it replaces
		if facts, ok := groupOf(session, chat); ok {
			stampRoom(session, &room, facts)
			if room.Name == "" {
				room.Name = session.groupName(room.Address)
			}
		}
	}
	if !silences(room) {
		room.Muted, room.Archived = chatMarks(session, addressJID(room.Address), time.Now())
	}
	batch := WebhookBatch{OrganizationAddress: session.Address, Rooms: []WebhookRoom{room}}
	if err := m.outbox.Deliver(session, batch); err != nil {
		m.log.Errorf("Post room line for %s failed: %v", room.Address, err)
	}
}

func (r *roomMemory) ring(id string, call ringing) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls == nil {
		r.calls = make(map[string]ringing)
	}
	for old, c := range r.calls {
		if call.offered.Sub(c.offered) > CALL_MEMORY {
			delete(r.calls, old)
		}
	}
	r.calls[id] = call
}

func (r *roomMemory) answer(id string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if call, ok := r.calls[id]; ok && call.answered.IsZero() {
		call.answered = at
		r.calls[id] = call
	}
}

func (r *roomMemory) hangUp(id string) (ringing, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call, ok := r.calls[id]
	delete(r.calls, id)
	return call, ok
}

// noteLabel keeps a label's name, or forgets a deleted one; labelName answers
// it, or the label's id when no edit has named it this process lifetime — the
// labels a chat had at linking are named by the full sync, which says nothing.
func (r *roomMemory) noteLabel(id, name string, deleted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.labels == nil {
		r.labels = make(map[string]string)
	}
	if deleted || name == "" {
		delete(r.labels, id)
		return
	}
	r.labels[id] = name
}

func (r *roomMemory) labelName(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if name, ok := r.labels[id]; ok {
		return name
	}
	return id
}

// swapFavorites keeps the favorites list now and answers the one before it;
// known is false when there was none.
func (r *roomMemory) swapFavorites(now map[string]bool) (before map[string]bool, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	before, known = r.favorites, r.favorites != nil
	r.favorites = now
	return before, known
}
