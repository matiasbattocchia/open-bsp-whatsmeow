package main

import (
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

// groupChange shapes a change WhatsApp announced for a group: the subject when
// it moved, the roster when it moved. ok is false when neither did — a
// description, a setting or an admin seat is not a line the consumer keeps.
func groupChange(session *Session, v *events.GroupInfo) (change WebhookGroup, ok bool) {
	change = WebhookGroup{Address: v.JID.String()}
	if v.Name != nil {
		change.Name = v.Name.Name
	}
	for _, jid := range v.Join {
		change.Joined = append(change.Joined, personOf(session, jid, types.JID{}, ""))
	}
	for _, jid := range v.Leave {
		change.Left = append(change.Left, personOf(session, jid, types.JID{}, ""))
	}
	if change.Name == "" && len(change.Joined) == 0 && len(change.Left) == 0 {
		return change, false
	}
	if len(change.Joined) > 0 || len(change.Left) > 0 {
		change.By = changedBy(session, v.Sender, v.SenderPN)
		if v.JoinReason == "invite" {
			change.Reason = "invite"
		}
	}
	change.Timestamp = changedAt(v.Timestamp).Format(time.RFC3339)
	return change, true
}

// joinedGroup shapes the account's own arrival in a group. A group made with
// the account in it arrives whole — its founding roster joined, its creator
// the one who did it; otherwise the account alone joined, added by the sender,
// or by nobody when it came in by the group's link.
func joinedGroup(session *Session, v *events.JoinedGroup) WebhookGroup {
	change := WebhookGroup{Address: v.JID.String(), Name: v.Name}
	at := time.Time{}
	if v.Type == "new" {
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
