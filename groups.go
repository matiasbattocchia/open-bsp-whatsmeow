package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// The group routes: a consumer opens and changes the account's groups through
// the same session its messages ride. Each call answers when WhatsApp has, and
// keeps nothing — the group is WhatsApp's, and what the consumer sees of a change
// afterwards is the group's own line about it.
//
// The routes sit at their own root, beside /contacts, for the reason that one
// does: a pattern under /sessions/{address}/ collides with /sessions/pending/{id}.
// {address} is the session, the way every other route names it; {group} is the
// group's JID, the conversation_address every message in it carries.
//
// Members are canonical phone digits both ways, the namespace every other
// address the bridge emits is in. A LID-addressed group knows its people by LID,
// so a removal is resolved against the roster and sent under the JID the group
// itself uses; an addition goes by number, which the server maps.

// groupRequest is the body of a create, a rename, an add and a removal: the
// fields each takes, the rest ignored.
type groupRequest struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

// groupMember is one seat on the roster as the consumer reads it: the canonical
// address, the name the wire gives the person when it has one, and whether the
// seat is an admin's.
type groupMember struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	Admin   bool   `json:"admin,omitempty"`
}

// groupView is what a group looks like from outside: its address, its subject,
// and who is in it.
type groupView struct {
	Address string        `json:"address"`
	Name    string        `json:"name,omitempty"`
	Members []groupMember `json:"members"`
}

// groupFacts is what every line of a group says of it: how many are in it,
// whether the account is an admin or super-admin there, and the JID of the
// community it belongs to ("" when none).
type groupFacts struct {
	members int
	admin   bool
	parent  string
}

// factsOf reads a group's facts off its info. own names the account by the user
// part of each JID it goes by, number and LID. WhatsApp counts a big group's
// members in its size and may list only some of them, its admins among them.
func factsOf(info *types.GroupInfo, own ...string) groupFacts {
	facts := groupFacts{members: max(info.ParticipantCount, len(info.Participants))}
	for _, p := range info.Participants {
		if (p.IsAdmin || p.IsSuperAdmin) && isOwn(p, own) {
			facts.admin = true
		}
	}
	if !info.LinkedParentJID.IsEmpty() {
		facts.parent = info.LinkedParentJID.ToNonAD().String()
	}
	return facts
}

// isOwn is whether a seat is the account's, under any JID it goes by.
func isOwn(p types.GroupParticipant, own []string) bool {
	for _, user := range own {
		if user != "" && (p.JID.User == user || p.PhoneNumber.User == user || p.LID.User == user) {
			return true
		}
	}
	return false
}

// ownUsers is the user part of each JID the account goes by: its number, and
// its LID once the device knows it.
func ownUsers(session *Session) []string {
	own := []string{session.Address}
	if session.Client != nil && session.Client.Store != nil {
		if id := session.Client.Store.ID; id != nil {
			own = append(own, id.User)
		}
		own = append(own, session.Client.Store.LID.User)
	}
	return own
}

// GROUP_READ bounds a read of a group's info. A line waits for it, since every
// line of a group carries the group's facts, and a line whose group cannot be
// read in time goes without them.
const GROUP_READ = 10 * time.Second

// groupReader reads a group's info off the wire: Client.GetGroupInfo.
type groupReader func(ctx context.Context, jid types.JID) (*types.GroupInfo, error)

// readGroup answers a group's facts: kept from the last read, else read now and
// kept. The read names the group and, when it sits in a community the bridge
// has not named yet, reads the community's subject too. ok is false when the
// group cannot be read.
func readGroup(session *Session, jid types.JID, read groupReader) (groupFacts, bool) {
	address := jid.ToNonAD().String()
	if facts, ok := session.cachedGroup(address); ok {
		return facts, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), GROUP_READ)
	defer cancel()
	info, err := read(ctx, jid.ToNonAD())
	if err != nil || info == nil {
		return groupFacts{}, false
	}
	facts := session.noteGroup(info, ownUsers(session)...)
	if facts.parent != "" && session.groupName(facts.parent) == "" {
		if parent, err := types.ParseJID(facts.parent); err == nil {
			if community, err := read(ctx, parent); err == nil && community != nil {
				session.noteGroupName(facts.parent, community.Name)
			}
		}
	}
	return facts, true
}

// groupOf is readGroup over the session's own connection.
func groupOf(session *Session, jid types.JID) (groupFacts, bool) {
	if facts, ok := session.cachedGroup(jid.ToNonAD().String()); ok || session.Client == nil {
		return facts, ok
	}
	return readGroup(session, jid, session.Client.GetGroupInfo)
}

// readAllGroups reads every group the account is in at once, for the history
// import: one query, where a group at a time would be hundreds. A community
// the list does not name is read on its own.
func readAllGroups(session *Session, list func(context.Context) ([]*types.GroupInfo, error), read groupReader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*GROUP_READ)
	defer cancel()
	infos, err := list(ctx)
	if err != nil {
		return err
	}
	own := ownUsers(session)
	parents := map[string]bool{}
	for _, info := range infos {
		if info == nil {
			continue
		}
		if facts := session.noteGroup(info, own...); facts.parent != "" {
			parents[facts.parent] = true
		}
	}
	for parent := range parents {
		if session.groupName(parent) != "" {
			continue
		}
		if jid, err := types.ParseJID(parent); err == nil {
			if community, err := read(ctx, jid); err == nil && community != nil {
				session.noteGroupName(parent, community.Name)
			}
		}
	}
	return nil
}

// readGroups is readAllGroups over the session's own connection, once a
// process: a group joined since is read on its own by its first line.
func (m *Manager) readGroups(session *Session) {
	if session.Client == nil {
		return
	}
	session.groupsRead.Do(func() {
		err := readAllGroups(session, session.Client.GetJoinedGroups, session.Client.GetGroupInfo)
		if err != nil {
			m.log.Warnf("Reading the groups of %s failed: %v — the history import goes without their facts",
				session.Address, err)
		}
	})
}

// stampGroup puts a group's facts on a message bound for the feed.
func stampGroup(session *Session, message *WebhookMessage, facts groupFacts) {
	message.ConversationMembers = facts.members
	message.ConversationAdmin = facts.admin
	message.ConversationCommunity = session.groupName(facts.parent)
}

// stampEdit puts a group's facts on an edit.
func stampEdit(session *Session, edit *WebhookEdit, facts groupFacts) {
	edit.ConversationMembers = facts.members
	edit.ConversationAdmin = facts.admin
	edit.ConversationCommunity = session.groupName(facts.parent)
}

// stampRoom puts a group's facts on a room's line.
func stampRoom(session *Session, room *WebhookRoom, facts groupFacts) {
	room.Members = facts.members
	room.Admin = facts.admin
	room.Community = session.groupName(facts.parent)
}

// groupSession answers the session a group route acts on, or writes the
// refusal: an unknown session is permanent, a disconnected one transient.
func (s *Server) groupSession(w http.ResponseWriter, r *http.Request) *Session {
	session := s.manager.Get(r.PathValue("address"))
	if session == nil {
		http.Error(w, "unknown session "+r.PathValue("address"), http.StatusNotFound)
		return nil
	}
	if !session.Client.IsConnected() {
		http.Error(w, "session not connected", http.StatusServiceUnavailable)
		return nil
	}
	return session
}

// groupJID parses the {group} segment: it must be a group's JID, since every
// other chat has no roster to change.
func groupJID(raw string) (types.JID, error) {
	jid, err := types.ParseJID(raw)
	if err != nil {
		return types.JID{}, fmt.Errorf("%s is not a JID: %w", raw, err)
	}
	if jid.Server != types.GroupServer {
		return types.JID{}, fmt.Errorf("%s is not a group", raw)
	}
	return jid, nil
}

// memberJIDs turns the consumer's addresses into the JIDs the wire takes:
// digits are a phone number; a full JID passes as it is.
func memberJIDs(addresses []string) ([]types.JID, error) {
	jids := make([]types.JID, 0, len(addresses))
	for _, address := range addresses {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		if strings.Contains(address, "@") {
			jid, err := types.ParseJID(address)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", address, err)
			}
			jids = append(jids, jid)
			continue
		}
		if digitsOf(address) != address {
			return nil, fmt.Errorf("%s is not a phone number", address)
		}
		jids = append(jids, types.NewJID(address, types.DefaultUserServer))
	}
	if len(jids) == 0 {
		return nil, errors.New("no members named")
	}
	return jids, nil
}

// groupErrorStatus maps a group query's failure onto the transient/permanent
// split the dispatch uses. WhatsApp answers a group IQ it will not honour with
// a code of its own — 401 when the account is no admin of the group, 403 when
// the group forbids it, 404 when there is no such group, 406 when the subject is
// unacceptable — and that is a permanent answer for these bytes, so the code
// rides through as a 4xx. Its 401 becomes a 403 here: a 401 from this server
// means the bearer, not the group. Everything else is the network, and 502.
func groupErrorStatus(err error) int {
	var iq *whatsmeow.IQError
	if errors.As(err, &iq) && iq.Code >= 400 && iq.Code < 500 {
		if iq.Code == http.StatusUnauthorized {
			return http.StatusForbidden
		}
		return iq.Code
	}
	return http.StatusBadGateway
}

// groupErrorText says what the code means in the group's terms, where WhatsApp's
// own word (`not-authorized`) does not.
func groupErrorText(err error) string {
	var iq *whatsmeow.IQError
	if errors.As(err, &iq) {
		switch iq.Code {
		case http.StatusUnauthorized:
			return fmt.Sprintf("%v — the account is not an admin of the group", err)
		case http.StatusNotFound:
			return fmt.Sprintf("%v — the account is in no such group", err)
		}
	}
	return err.Error()
}

// participantFailures reads the seats the server would not fill off the answer to
// a create or an add: each names its address and WhatsApp's code for it — 403 for
// somebody whose settings keep strangers from adding them (an invite is the way
// in), 409 for somebody already in — so the consumer can say who did not make it.
func participantFailures(session *Session, seats []types.GroupParticipant) []string {
	var failed []string
	for _, p := range seats {
		if p.Error == 0 {
			continue
		}
		why := fmt.Sprintf("%d", p.Error)
		switch p.Error {
		case http.StatusForbidden:
			why += " (their settings keep strangers from adding them — they join by invite)"
		case http.StatusConflict:
			why += " (already in the group)"
		}
		failed = append(failed, canonicalUser(session, p.JID, p.PhoneNumber)+": "+why)
	}
	return failed
}

// rosterOf shapes a group's participants the way the consumer reads people:
// canonical digits, the name the wire has for them, the admin seats marked.
func rosterOf(session *Session, info *types.GroupInfo) []groupMember {
	members := make([]groupMember, 0, len(info.Participants))
	for _, p := range info.Participants {
		member := personOf(session, p.JID, p.PhoneNumber, p.DisplayName)
		member.Admin = p.IsAdmin || p.IsSuperAdmin
		members = append(members, member)
	}
	return members
}

// personOf is one person as the consumer reads people: canonical digits and
// the name the account has for them, none when the session keeps no book.
func personOf(session *Session, jid, alt types.JID, live string) groupMember {
	address := canonicalUser(session, jid, alt)
	name := ""
	if session.Client != nil && session.Client.Store != nil && session.Client.Store.Contacts != nil {
		name, _ = contactName(session, address, live)
	}
	return groupMember{Address: address, Name: name}
}

// groupChange shapes a change WhatsApp announced for a group, every field it
// moved. ok is false when the notification moved nothing the room's line says,
// which is a notification about participant versions alone.
func groupChange(session *Session, v *events.GroupInfo) (change WebhookRoom, ok bool) {
	change = WebhookRoom{Address: v.JID.String()}
	if v.Name != nil && v.Name.Name != "" {
		change.Name = v.Name.Name
		change.Renamed = true
	}
	people := func(jids []types.JID) []groupMember {
		var out []groupMember
		for _, jid := range jids {
			out = append(out, personOf(session, jid, types.JID{}, ""))
		}
		return out
	}
	change.Joined = people(v.Join)
	change.Left = people(v.Leave)
	change.Promoted = people(v.Promote)
	change.Demoted = people(v.Demote)
	if v.Topic != nil {
		about := v.Topic.Topic
		if v.Topic.TopicDeleted {
			about = ""
		}
		change.Description = &about
	}
	if v.Announce != nil {
		change.Announce = new(v.Announce.IsAnnounce)
	}
	if v.Locked != nil {
		change.Locked = new(v.Locked.IsLocked)
	}
	if v.Ephemeral != nil {
		timer := uint32(0)
		if v.Ephemeral.IsEphemeral {
			timer = v.Ephemeral.DisappearingTimer
		}
		change.Disappearing = &timer
	}
	if v.MembershipApprovalMode != nil {
		change.Approval = new(v.MembershipApprovalMode.IsJoinApprovalRequired)
	}
	change.InviteLink = v.NewInviteLink
	change.Deleted = v.Delete != nil && v.Delete.Deleted
	change.Suspended = v.Suspended
	change.Unsuspended = v.Unsuspended
	if v.Link != nil {
		change.Linked = []roomLink{linkOf(v.Link)}
	}
	if v.Unlink != nil {
		change.Unlinked = []roomLink{linkOf(v.Unlink)}
	}
	for _, node := range v.UnknownChanges {
		if node != nil {
			change.Unknown = append(change.Unknown, node.Tag)
		}
	}
	if !saysSomething(change) {
		return change, false
	}
	change.By = changedBy(session, v.Sender, v.SenderPN)
	if len(change.Joined) > 0 && v.JoinReason == "invite" {
		change.Reason = "invite"
	}
	change.Timestamp = changedAt(v.Timestamp).Format(time.RFC3339)
	return change, true
}

// saysSomething is whether a group's change moved anything its line would say.
func saysSomething(c WebhookRoom) bool {
	return c.Renamed || len(c.Joined) > 0 || len(c.Left) > 0 || len(c.Promoted) > 0 ||
		len(c.Demoted) > 0 || c.Description != nil || c.Announce != nil || c.Locked != nil ||
		c.Disappearing != nil || c.Approval != nil || c.InviteLink != nil || c.Deleted ||
		c.Suspended || c.Unsuspended || len(c.Linked) > 0 || len(c.Unlinked) > 0 ||
		len(c.Unknown) > 0
}

// linkOf is the group a community change names, and how it stands to this one.
func linkOf(change *types.GroupLinkChange) roomLink {
	return roomLink{
		Address: change.Group.JID.String(),
		Name:    change.Group.Name,
		Type:    string(change.Type),
	}
}

// joinedGroup shapes the account's own arrival in a group. A group made with
// the account in it arrives whole — created, its founding roster joined, its
// creator the one who did it; otherwise the account alone joined, added by the
// sender, or by nobody when it came in by the group's link.
func joinedGroup(session *Session, v *events.JoinedGroup) WebhookRoom {
	change := WebhookRoom{Address: v.JID.String(), Name: v.Name}
	at := time.Time{}
	if v.Type == "new" {
		change.Created = true
		change.Joined = rosterOf(session, &v.GroupInfo)
		at = v.GroupCreated
	}
	if len(change.Joined) == 0 {
		change.Joined = []groupMember{{Address: session.Address}}
	}
	change.By = changedBy(session, v.Sender, v.SenderPN)
	if v.Reason == "invite" {
		change.Reason = "invite"
	}
	change.Timestamp = changedAt(at).Format(time.RFC3339)
	return change
}

// changedBy is whoever made a change, when WhatsApp says.
func changedBy(session *Session, sender, senderPN *types.JID) *groupMember {
	if sender == nil || sender.IsEmpty() {
		return nil
	}
	alt := types.JID{}
	if senderPN != nil {
		alt = *senderPN
	}
	by := personOf(session, *sender, alt, "")
	return &by
}

// changedAt is when a change happened; a notification that carries no time
// happened as it arrived.
func changedAt(at time.Time) time.Time {
	if at.IsZero() {
		return time.Now().UTC()
	}
	return at.UTC()
}

func viewOf(session *Session, info *types.GroupInfo) groupView {
	return groupView{
		Address: info.JID.String(),
		Name:    info.Name,
		Members: rosterOf(session, info),
	}
}

// handleCreateGroup makes a group under a subject with these people in it; the
// account is in it by the wire's own rule. The answer is the group as it stands:
// a seat the server would not fill is left out of the roster, and named in
// `not_added` with the code — the group exists either way, and the caller can
// see who is in it.
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	session := s.groupSession(w, r)
	if session == nil {
		return
	}
	var req groupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "a group needs a name", http.StatusUnprocessableEntity)
		return
	}
	members, err := memberJIDs(req.Members)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	info, err := session.Client.CreateGroup(r.Context(), whatsmeow.ReqCreateGroup{
		Name:         name,
		Participants: members,
	})
	if err != nil {
		http.Error(w, "create group: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	session.noteGroupName(info.JID.String(), info.Name)
	failed := participantFailures(session, info.Participants)
	seated := make([]types.GroupParticipant, 0, len(info.Participants))
	for _, p := range info.Participants {
		if p.Error == 0 {
			seated = append(seated, p)
		}
	}
	info.Participants = seated
	out := map[string]any{
		"address": info.JID.String(),
		"name":    info.Name,
		"members": rosterOf(session, info),
	}
	if len(failed) > 0 {
		out["not_added"] = failed
	}
	writeJSON(w, out)
}

// handleGroup reads a group as WhatsApp holds it: subject and roster.
func (s *Server) handleGroup(w http.ResponseWriter, r *http.Request) {
	session := s.groupSession(w, r)
	if session == nil {
		return
	}
	jid, err := groupJID(r.PathValue("group"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	info, err := session.Client.GetGroupInfo(r.Context(), jid)
	if err != nil {
		http.Error(w, "group info: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	session.noteGroupName(info.JID.String(), info.Name)
	session.noteAddressingMode(info.JID.String(), info.AddressingMode)
	writeJSON(w, viewOf(session, info))
}

// handleRenameGroup sets the subject. WhatsApp's own ceiling on a subject comes
// back as its 406, passed through.
func (s *Server) handleRenameGroup(w http.ResponseWriter, r *http.Request) {
	session := s.groupSession(w, r)
	if session == nil {
		return
	}
	jid, err := groupJID(r.PathValue("group"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	var req groupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		http.Error(w, "a group needs a name", http.StatusUnprocessableEntity)
		return
	}
	if err := session.Client.SetGroupName(r.Context(), jid, name); err != nil {
		http.Error(w, "rename group: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	session.noteGroupName(jid.String(), name)
	writeJSON(w, map[string]any{})
}

// handleLeaveGroup takes the account out of the group.
func (s *Server) handleLeaveGroup(w http.ResponseWriter, r *http.Request) {
	session := s.groupSession(w, r)
	if session == nil {
		return
	}
	jid, err := groupJID(r.PathValue("group"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err := session.Client.LeaveGroup(r.Context(), jid); err != nil {
		http.Error(w, "leave group: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	writeJSON(w, map[string]any{})
}

// handleAddMembers seats these people in the group. A seat the server would not
// fill fails the call as a whole (422), naming each with WhatsApp's code: the
// caller asked for all of them, and the ones that made it are on the roster.
func (s *Server) handleAddMembers(w http.ResponseWriter, r *http.Request) {
	session := s.groupSession(w, r)
	if session == nil {
		return
	}
	jid, err := groupJID(r.PathValue("group"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	var req groupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	members, err := memberJIDs(req.Members)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	seats, err := session.Client.UpdateGroupParticipants(
		r.Context(), jid, members, whatsmeow.ParticipantChangeAdd,
	)
	if err != nil {
		http.Error(w, "add members: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	if failed := participantFailures(session, seats); len(failed) > 0 {
		http.Error(w, "not added — "+strings.Join(failed, "; "), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, map[string]any{})
}

// handleRemoveMembers takes these people out. Each is found on the roster
// first, so the removal names them the way the group does — a LID-addressed
// group does not know its people by number — and somebody not in the group is
// a 404 before anything is sent.
func (s *Server) handleRemoveMembers(w http.ResponseWriter, r *http.Request) {
	session := s.groupSession(w, r)
	if session == nil {
		return
	}
	jid, err := groupJID(r.PathValue("group"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	var req groupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wanted, err := memberJIDs(req.Members)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	info, err := session.Client.GetGroupInfo(r.Context(), jid)
	if err != nil {
		http.Error(w, "group info: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	session.noteAddressingMode(info.JID.String(), info.AddressingMode)
	seats, missing := seatsOf(session, info, wanted)
	if len(missing) > 0 {
		http.Error(w, strings.Join(missing, ", ")+" not in the group", http.StatusNotFound)
		return
	}
	if _, err := session.Client.UpdateGroupParticipants(
		r.Context(), jid, seats, whatsmeow.ParticipantChangeRemove,
	); err != nil {
		http.Error(w, "remove members: "+groupErrorText(err), groupErrorStatus(err))
		return
	}
	writeJSON(w, map[string]any{})
}

// seatsOf finds each wanted person on the roster and answers the JID the group
// holds them under, plus whoever is not there. A person is matched by canonical
// digits, or by either of their JIDs when the caller gave one.
func seatsOf(session *Session, info *types.GroupInfo, wanted []types.JID) (seats []types.JID, missing []string) {
	for _, want := range wanted {
		found := false
		for _, p := range info.Participants {
			if p.JID.ToNonAD() == want.ToNonAD() || p.PhoneNumber.ToNonAD() == want.ToNonAD() ||
				p.LID.ToNonAD() == want.ToNonAD() ||
				(want.Server == types.DefaultUserServer &&
					canonicalUser(session, p.JID, p.PhoneNumber) == want.User) {
				seats = append(seats, p.JID)
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, want.User)
		}
	}
	return seats, missing
}
