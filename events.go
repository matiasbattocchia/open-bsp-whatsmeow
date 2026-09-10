package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// handleEvent translates whatsmeow's typed events into connector-webhook
// batches. The bridge deliberately posts EVERYTHING, including its own sends
// echoed back (IsFromMe): the webhook upserts on external_id, so bridge-sent
// messages dedupe against the row the dispatcher committed, and phone-sent
// messages land as outgoing rows — the smb_message_echoes equivalent.
func (m *Manager) handleEvent(session *Session, evt any) {
	switch v := evt.(type) {
	case *events.PairSuccess:
		if session.Address == "" {
			m.completePairing(session, v.ID)
		}

	case *events.Connected:
		if session.Address != "" {
			m.postLinkState(session, "connected")
		} else if session.Client.Store.ID != nil {
			m.completePairing(session, *session.Client.Store.ID)
		}

	case *events.Disconnected:
		// Fires only on an unexpected drop — whatsmeow reconnects by itself
		// and a logout expects its own disconnect, so a paired session's
		// `disconnected` is always answered by a `connected`.
		if session.Address != "" {
			m.postLinkState(session, "disconnected")
			return
		}
		// A pairing socket that drops is DEAD, not slow: WhatsApp ends the
		// stream ~3 minutes after issuing a phone code, and the code dies with
		// it. Fail the pending session now — otherwise the poll keeps
		// answering "pending" until the 10-minute TTL, promising a code the
		// server has already forgotten.
		m.failPending(session, "pairing window closed — request a new code")

	case *events.StreamError:
		m.failPending(session, "pairing stream error: "+v.Code)

	case *events.LoggedOut:
		m.log.Warnf("Session %s logged out (reason %d)", session.Address, v.Reason)
		if err := m.openbsp.PostSessionEvent(SessionEvent{
			Event:          "logged_out",
			OrganizationID: session.OrganizationID,
			Address:        session.Address,
			Extra:          map[string]any{"reason": int(v.Reason)},
		}); err != nil {
			m.log.Errorf("Notify logged_out for %s failed: %v", session.Address, err)
		}

	case *events.OfflineSyncPreview:
		if v.Messages > 0 {
			m.log.Infof("Draining %d queued message(s) for %s", v.Messages, session.Address)
			session.beginDrain(v.Messages)
			// The queue must never strand the backlog: if the server stops mid-drain, or
			// never says it finished, what is held still has to reach the consumer.
			go m.drainDeadline(session, v.Messages)
		}

	case *events.OfflineSyncCompleted:
		m.flushOffline(session)

	case *events.Message:
		m.handleMessage(session, v)

	case *events.Receipt:
		m.handleReceipt(session, v)

	case *events.GroupInfo:
		if v.Name != nil {
			batch := WebhookBatch{
				OrganizationAddress: session.Address,
				Groups: []WebhookGroup{
					{Address: v.JID.String(), Name: v.Name.Name},
				},
			}
			if err := m.openbsp.PostBatch(batch); err != nil {
				m.log.Errorf("Post group rename for %s failed: %v", v.JID, err)
			}
		}

	case *events.HistorySync:
		// Runs in its own goroutine: a sync can carry thousands of messages
		// and must not block the event loop.
		go m.handleHistorySync(session, v)
	}
}

// canonicalUser resolves a JID to the canonical bare phone digits used as
// contact_address. LID (hidden user) JIDs are mapped back to the phone
// number via the alt JID the event carries, falling back to the store's LID
// map, then to the LID digits themselves (rare: a LID-only peer the store
// has never seen a mapping for).
func canonicalUser(session *Session, jid, alt types.JID) string {
	if jid.Server != types.HiddenUserServer {
		return jid.User
	}
	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.User
	}
	pn, err := session.Client.Store.LIDs.GetPNForLID(context.Background(), jid.ToNonAD())
	if err == nil && !pn.IsEmpty() {
		return pn.User
	}
	return jid.User
}

// conversationAddressFor is the chat's address: the group JID for groups,
// the peer's canonical bare number for direct chats — the value of
// messages.conversation_address. senderAddressFor is the message author —
// the group participant, the DM peer, or the session's own number when the
// account itself spoke (IsFromMe): we know our address, so we stamp it.
func conversationAddressFor(session *Session, source types.MessageSource) string {
	if source.IsGroup {
		return source.Chat.String()
	}
	if source.IsFromMe {
		return canonicalUser(session, source.Chat, source.RecipientAlt)
	}
	return canonicalUser(session, source.Chat, source.SenderAlt)
}

// chatSegment is the CHAT half of an external id, in the same canonical namespace as
// conversationAddressFor (bare, since ids carry users not JIDs). A LID-addressed chat
// speaks lids on the wire, and an id minted from one matches nothing the consumer stored
// from the other side: our own sends address the peer by phone number, so a reply-to, a
// reaction and a receipt all came back naming a chat that did not exist here (live,
// 2026-08-18 — the quoted approval that answered nothing).
func chatSegment(session *Session, source types.MessageSource) string {
	if source.IsGroup {
		return source.Chat.User
	}
	if source.IsFromMe {
		return canonicalUser(session, source.Chat, source.RecipientAlt)
	}
	return canonicalUser(session, source.Chat, source.SenderAlt)
}

func senderAddressFor(session *Session, source types.MessageSource) string {
	if source.IsFromMe {
		return session.Address
	}
	if source.IsGroup {
		return canonicalUser(session, source.Sender, source.SenderAlt)
	}
	return canonicalUser(session, source.Chat, source.SenderAlt)
}

// pickName chooses what to call somebody, address book first: FullName and
// FirstName are what the ACCOUNT named this contact, PushName only what the
// contact calls itself, and BusinessName the storefront it trades under. The
// live event's own pushname (`live`) beats the stored one — same fact, fresher
// — but never the account's own naming.
func pickName(contact types.ContactInfo, live string) string {
	for _, candidate := range []string{
		contact.FullName, contact.FirstName, live, contact.PushName, contact.BusinessName,
	} {
		if name := strings.TrimSpace(candidate); name != "" {
			return name
		}
	}
	return ""
}

// contactName is pickName over the contact store, keyed by canonical digits.
// A miss is not an error: plenty of numbers are in no address book, and the
// consumer's fallback is the address itself.
func contactName(session *Session, user, live string) string {
	if user == "" {
		return strings.TrimSpace(live)
	}
	contact, err := session.Client.Store.Contacts.GetContact(
		context.Background(), types.NewJID(user, types.DefaultUserServer),
	)
	if err != nil {
		return strings.TrimSpace(live)
	}
	return pickName(contact, live)
}

// marksOf reads a chat's mute/archive off the settings whatsmeow's app state
// sync keeps phone-synced. Muted is a deadline, not a flag: "8 hours" stores a
// timestamp, "always" stores the far-future sentinel, an unmute zeroes it — so
// the question is simply whether the deadline is still ahead of this message.
func marksOf(settings types.LocalChatSettings, now time.Time) (muted, archived bool) {
	if !settings.Found {
		return false, false
	}
	return settings.MutedUntil.After(now), settings.Archived
}

// chatMarks is marksOf against the session's settings store. A miss is an
// unmarked chat, never an error: most chats have no settings row at all.
func chatMarks(session *Session, chat types.JID, now time.Time) (muted, archived bool) {
	store := session.Client.Store.ChatSettings
	if store == nil {
		return false, false
	}
	settings, err := store.GetChatSettings(context.Background(), chat)
	if err != nil {
		return false, false
	}
	return marksOf(settings, now)
}

// mediaKinds maps a detected media message to the FilePart metadata OpenBSP
// expects; the actual bytes are resolved separately via DownloadAny.
type inboundMedia struct {
	kind    string
	mime    string
	name    string
	caption string
}

func inboundMediaInfo(msg *waE2E.Message) *inboundMedia {
	switch {
	case msg.GetImageMessage() != nil:
		img := msg.GetImageMessage()
		return &inboundMedia{kind: "image", mime: img.GetMimetype(), caption: img.GetCaption()}
	case msg.GetAudioMessage() != nil:
		aud := msg.GetAudioMessage()
		return &inboundMedia{kind: "audio", mime: aud.GetMimetype()}
	case msg.GetVideoMessage() != nil:
		vid := msg.GetVideoMessage()
		return &inboundMedia{kind: "video", mime: vid.GetMimetype(), caption: vid.GetCaption()}
	case msg.GetDocumentMessage() != nil:
		doc := msg.GetDocumentMessage()
		return &inboundMedia{kind: "document", mime: doc.GetMimetype(), name: doc.GetFileName(), caption: doc.GetCaption()}
	case msg.GetStickerMessage() != nil:
		stk := msg.GetStickerMessage()
		return &inboundMedia{kind: "sticker", mime: stk.GetMimetype()}
	}
	return nil
}

// mentionsIn collects ContextInfo.MentionedJID — the participants an inbound
// message tags — as canonical digits, AND puts the message text in the same
// namespace: WhatsApp writes the mention inline as "@digits", which in a
// lid-addressed chat are the LID's, so the token is rewritten to the
// canonical digits the Mention carries. Namespace is the bridge's business
// end to end (outbound, encodeMentions maps back); no name resolution
// happens here — the directory side of a mention is OpenBSP's to enrich.
func mentionsIn(session *Session, msg *waE2E.Message, text string) (string, []Mention) {
	for _, ctx := range []*waE2E.ContextInfo{
		msg.GetExtendedTextMessage().GetContextInfo(),
		msg.GetImageMessage().GetContextInfo(),
		msg.GetVideoMessage().GetContextInfo(),
		msg.GetDocumentMessage().GetContextInfo(),
	} {
		jids := ctx.GetMentionedJID()
		if len(jids) == 0 {
			continue
		}
		mentions := make([]Mention, 0, len(jids))
		for _, raw := range jids {
			jid, err := types.ParseJID(raw)
			if err != nil {
				continue
			}
			address := canonicalUser(session, jid, types.JID{})
			if address != jid.User {
				text = strings.ReplaceAll(text, "@"+jid.User, "@"+address)
			}
			mentions = append(mentions, Mention{Address: address})
		}
		return text, mentions
	}
	return text, nil
}

// quotedRef pulls the reply target (quoted message id + its sender JID) out
// of whichever message type carries the ContextInfo.
func quotedRef(msg *waE2E.Message) (stanzaID, participant string) {
	for _, ctx := range []*waE2E.ContextInfo{
		msg.GetExtendedTextMessage().GetContextInfo(),
		msg.GetImageMessage().GetContextInfo(),
		msg.GetAudioMessage().GetContextInfo(),
		msg.GetVideoMessage().GetContextInfo(),
		msg.GetDocumentMessage().GetContextInfo(),
		msg.GetStickerMessage().GetContextInfo(),
		msg.GetLocationMessage().GetContextInfo(),
		msg.GetContactMessage().GetContextInfo(),
		msg.GetContactsArrayMessage().GetContextInfo(),
	} {
		if ctx.GetStanzaID() != "" {
			return ctx.GetStanzaID(), ctx.GetParticipant()
		}
	}
	return "", ""
}

// keySender resolves the sender segment of an external id from
// MessageKey-style references (fromMe + participant), falling back to the
// DM peer (== chat) when no participant is given.
func keySender(session *Session, chat types.JID, fromMe bool, participant string) string {
	if fromMe {
		return session.Address
	}
	if participant != "" {
		if jid, err := types.ParseJID(participant); err == nil {
			return canonicalUser(session, jid, types.JID{})
		}
	}
	return chat.User
}

// parseVcard extracts the fields OpenBSP's ContactData carries (FN and TEL
// lines) from a vCard blob.
func parseVcard(displayName, vcard string) ContactData {
	var contact ContactData
	contact.Name.FormattedName = displayName

	for line := range strings.Lines(vcard) {
		line = strings.TrimRight(line, "\r\n")
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.ToUpper(strings.SplitN(key, ";", 2)[0])
		switch key {
		case "FN":
			if contact.Name.FormattedName == "" {
				contact.Name.FormattedName = value
			}
		case "TEL":
			var phone struct {
				Phone string `json:"phone"`
				WaID  string `json:"wa_id,omitempty"`
				Type  string `json:"type,omitempty"`
			}
			phone.Phone = value
			contact.Phones = append(contact.Phones, phone)
		}
	}
	return contact
}

func dataPart(kind string, data any) (*MessageContent, error) {
	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &MessageContent{Version: "1", Type: "data", Kind: kind, Data: payload}, nil
}

// errMediaNotImported is why a history row carries a placeholder: not a
// failure to report to the org as one, but the import's standing policy.
var errMediaNotImported = errors.New(
	"media not imported: history sync does not fetch media",
)

// buildContent extracts a v1 content Part from the event. A media message
// becomes a FilePart only when downloadMedia is set AND the bytes reach
// storage; every other path returns a media_placeholder and the reason in
// mediaErr, for the caller to put in the message's status.
func (m *Manager) buildContent(session *Session, evt *events.Message, downloadMedia bool) (content *MessageContent, mediaErr error) {
	text := evt.Message.GetConversation()
	if text == "" {
		text = evt.Message.GetExtendedTextMessage().GetText()
	}
	if text != "" {
		body, mentions := mentionsIn(session, evt.Message, whatsappToMarkdown(text))
		return &MessageContent{
			Version:  "1",
			Type:     "text",
			Kind:     "text",
			Text:     body,
			Mentions: mentions,
		}, nil
	}

	if reaction := evt.Message.GetReactionMessage(); reaction != nil {
		key := reaction.GetKey()
		emoji := reaction.GetText()
		// Cross-service ReactionPart, data-only (rendering is the UI's job):
		// {action, name, unicode}, name = the emoji itself for WhatsApp;
		// removals carry no emoji on this service.
		reactionData := map[string]any{"action": "removed"}
		if emoji != "" {
			reactionData = map[string]any{
				"action":  "added",
				"name":    emoji,
				"unicode": emoji,
			}
		}
		payload, err := json.Marshal(reactionData)
		if err != nil {
			return nil, err
		}
		return &MessageContent{
			Version: "1",
			Type:    "data",
			Kind:    "reaction",
			Data:    payload,
			ReMessageID: externalID(
				session.Address, chatSegment(session, evt.Info.MessageSource),
				keySender(session, evt.Info.Chat, key.GetFromMe(), key.GetParticipant()),
				key.GetID(),
			),
		}, nil
	}

	if loc := evt.Message.GetLocationMessage(); loc != nil {
		return dataPart("location", LocationData{
			Latitude:  loc.GetDegreesLatitude(),
			Longitude: loc.GetDegreesLongitude(),
			Name:      loc.GetName(),
			Address:   loc.GetAddress(),
		})
	}

	if contact := evt.Message.GetContactMessage(); contact != nil {
		return dataPart("contacts", []ContactData{
			parseVcard(contact.GetDisplayName(), contact.GetVcard()),
		})
	}

	if contactsArray := evt.Message.GetContactsArrayMessage(); contactsArray != nil {
		contacts := make([]ContactData, 0, len(contactsArray.GetContacts()))
		for _, c := range contactsArray.GetContacts() {
			contacts = append(contacts, parseVcard(c.GetDisplayName(), c.GetVcard()))
		}
		return dataPart("contacts", contacts)
	}

	media := inboundMediaInfo(evt.Message)
	if media == nil {
		return nil, nil
	}

	captionText, captionMentions := mentionsIn(
		session, evt.Message, whatsappToMarkdown(media.caption),
	)
	// A FilePart is a promise of bytes, and a consumer takes it as one: it
	// renders an attachment and offers to open it. With no URI to open there
	// is nothing to render, so an unstored medium is a media_placeholder
	// instead — what the Cloud API sends when it cannot hand the media over,
	// so consumers already know the shape.
	//
	// Data stays empty, as the Cloud API leaves it; what whatsmeow knows that
	// the Cloud API does not — the type and name of the medium — rides in
	// File, minus the URI. The caption survives either way; it is the part
	// the peer actually wrote.
	placeholder := &MessageContent{
		Version:  "1",
		Type:     "data",
		Kind:     "media_placeholder",
		Text:     captionText,
		Data:     json.RawMessage("{}"),
		File:     &FilePayload{MimeType: media.mime, Name: media.name},
		Mentions: captionMentions,
	}

	// Media in history is usually already gone from the CDN, and re-fetching
	// a year of it would spend the org's whole storage quota on one sweep.
	if !downloadMedia {
		return placeholder, errMediaNotImported
	}

	data, err := session.Client.DownloadAny(context.Background(), evt.Message)
	if err != nil {
		return placeholder, fmt.Errorf("download media: %w", err)
	}

	uri, err := m.openbsp.UploadMedia(session.Address, media.name, data)
	if err != nil {
		return placeholder, fmt.Errorf("store media: %w", err)
	}

	return &MessageContent{
		Version: "1",
		Type:    "file",
		Kind:    media.kind,
		Text:    captionText,
		File: &FilePayload{
			MimeType: media.mime,
			Name:     media.name,
			URI:      uri,
			Size:     int64(len(data)),
		},
		Mentions: captionMentions,
	}, nil
}

// editBody reads the new content in the original's part types, carrying its mentions so
// an edit that adds an @name says who. No readable text means nothing to publish.
func editBody(session *Session, edited *waE2E.Message) (body string, mentions []Mention, ok bool) {
	text := edited.GetConversation()
	if text == "" {
		text = edited.GetExtendedTextMessage().GetText()
	}
	if text == "" {
		text = edited.GetImageMessage().GetCaption()
	}
	if text == "" {
		text = edited.GetVideoMessage().GetCaption()
	}
	if text == "" {
		text = edited.GetDocumentMessage().GetCaption()
	}
	if text == "" {
		return "", nil, false
	}
	body, mentions = mentionsIn(session, edited, whatsappToMarkdown(text))
	return body, mentions, true
}

// DRAIN_GRACE bounds the hold. A queue the server never finishes sending must not keep
// the consumer from hearing what did arrive, so the hold gives up after this and posts.
const DRAIN_GRACE = 90 * time.Second

// flushOffline posts everything the hold kept, as one batch: the whole queue is one
// arrival. A drain that held nothing posts nothing — the consumer stays asleep.
func (m *Manager) flushOffline(session *Session) {
	held := session.endDrain()
	if len(held) == 0 {
		return
	}
	m.log.Infof("Queue drained for %s — posting %d message(s) as one", session.Address, len(held))
	batch := WebhookBatch{OrganizationAddress: session.Address, Messages: held}
	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post drained queue for %s failed: %v", session.Address, err)
	}
}

// drainDeadline is the hold's backstop — see DRAIN_GRACE.
func (m *Manager) drainDeadline(session *Session, expected int) {
	time.Sleep(DRAIN_GRACE)
	session.offlineMu.Lock()
	stranded := session.draining
	session.offlineMu.Unlock()
	if !stranded {
		return
	}
	m.log.Warnf("Queue for %s never finished (%d announced) — posting what arrived",
		session.Address, expected)
	m.flushOffline(session)
}

// fieldsOf names the fields a message carries, values omitted — enough to say what a
// shape we cannot read is made of, without putting anyone's words in the log.
func fieldsOf(msg *waE2E.Message) string {
	var names []string
	msg.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		names = append(names, string(fd.Name()))
		return true
	})
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// editFor builds the webhook edit both wire shapes agree on, stamped with the chat's
// state like any other event that can wake the consumer.
func (m *Manager) editFor(
	session *Session, evt *events.Message, edited *waE2E.Message,
	ownID, original, conversation, sender string,
) *WebhookEdit {
	body, mentions, ok := editBody(session, edited)
	if !ok {
		// Loud, and specific about the shape: a correction the consumer never hears is
		// the same loss as a message it never hears, and the field names say what to read.
		m.log.Warnf("Unreadable edit on %s (fields: %s)", original, fieldsOf(edited))
		return nil
	}
	muted, archived := chatMarks(session, evt.Info.Chat, evt.Info.Timestamp)
	return &WebhookEdit{
		ExternalID:          ownID,
		OriginalMessageID:   original,
		ConversationAddress: conversation,
		SenderAddress:       sender,
		Text:                body,
		Mentions:            mentions,
		Timestamp:           evt.Info.Timestamp.Format(time.RFC3339),
		Muted:               muted,
		Archived:            archived,
	}
}

// handleSecretEncrypted translates the message-secret envelope — the shape WhatsApp
// wraps an edit in, keyed to the message it edits and encrypted under that message's
// secret. Only the client holding the original's secret can read it.
func (m *Manager) handleSecretEncrypted(
	session *Session, evt *events.Message, enc *waE2E.SecretEncryptedMessage,
) {
	if enc.GetSecretEncType() != waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
		m.log.Warnf("Skipping secret-encrypted %s (%s)", evt.Info.ID, enc.GetSecretEncType())
		return
	}
	edited, err := session.Client.DecryptSecretEncryptedMessage(context.Background(), evt)
	if err != nil {
		m.log.Errorf("Decrypt edit %s failed: %v", evt.Info.ID, err)
		return
	}
	m.log.Debugf("Decrypted edit envelope on %s", evt.Info.ID)

	// The envelope holds the edit proper: the protocol message the old wire shape sent
	// in the clear, now sealed under the target's secret. Its editedMessage is the new
	// content, and its key names the message being replaced.
	key := enc.GetTargetMessageKey()
	if pm := edited.GetProtocolMessage(); pm.GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT {
		edited = pm.GetEditedMessage()
		if pm.GetKey().GetID() != "" {
			key = pm.GetKey()
		}
	}
	original := externalID(
		session.Address, chatSegment(session, evt.Info.MessageSource),
		keySender(session, evt.Info.Chat, key.GetFromMe(), key.GetParticipant()),
		key.GetID(),
	)
	ownSegment := session.Address
	if !evt.Info.IsFromMe {
		ownSegment = canonicalUser(session, evt.Info.Sender, evt.Info.SenderAlt)
	}
	ownID := externalID(
		session.Address, chatSegment(session, evt.Info.MessageSource), ownSegment, evt.Info.ID,
	)

	edit := m.editFor(
		session, evt, edited, ownID, original,
		conversationAddressFor(session, evt.Info.MessageSource),
		senderAddressFor(session, evt.Info.MessageSource),
	)
	if edit == nil {
		return
	}
	batch := WebhookBatch{OrganizationAddress: session.Address, Edits: []WebhookEdit{*edit}}
	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post edit for %s failed: %v", original, err)
		return
	}
	m.log.Infof("Published edit of %s (%d mentions)", original, len(edit.Mentions))
}

// handleProtocolMessage translates edits and revokes into the webhook's
// edits/revokes arrays (applied as in-place updates keyed by the ORIGINAL
// external id). Other protocol messages (app state, key distribution, ...)
// are internal noise and ignored.
func (m *Manager) handleProtocolMessage(session *Session, evt *events.Message, pm *waE2E.ProtocolMessage) {
	batch := WebhookBatch{OrganizationAddress: session.Address}
	key := pm.GetKey()
	original := externalID(
		session.Address, chatSegment(session, evt.Info.MessageSource),
		keySender(session, evt.Info.Chat, key.GetFromMe(), key.GetParticipant()),
		key.GetID(),
	)
	timestamp := evt.Info.Timestamp.Format(time.RFC3339)

	// the edit/revoke's OWN identity: the carrier protocol message, addressed like any
	// message — so the consumer can log it as a first-class event beside the original
	ownSegment := session.Address
	if !evt.Info.IsFromMe {
		ownSegment = canonicalUser(session, evt.Info.Sender, evt.Info.SenderAlt)
	}
	ownID := externalID(
		session.Address, chatSegment(session, evt.Info.MessageSource), ownSegment, evt.Info.ID,
	)
	conversation := conversationAddressFor(session, evt.Info.MessageSource)
	sender := senderAddressFor(session, evt.Info.MessageSource)

	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		batch.Revokes = append(batch.Revokes, WebhookRevoke{
			ExternalID:          ownID,
			OriginalMessageID:   original,
			ConversationAddress: conversation,
			SenderAddress:       sender,
			Timestamp:           timestamp,
		})
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		edit := m.editFor(session, evt, pm.GetEditedMessage(), ownID, original, conversation, sender)
		if edit == nil {
			return
		}
		batch.Edits = append(batch.Edits, *edit)
	default:
		return
	}

	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post edit/revoke for %s failed: %v", original, err)
	}
}

func (m *Manager) handleMessage(session *Session, evt *events.Message) {
	if session.Address == "" {
		return // still pairing
	}

	// WhatsApp Status (stories) and newsletters are not conversations —
	// drop them. status@broadcast notably is NOT a group: GetGroupInfo on
	// it times out and would stall the event loop.
	if evt.Info.Chat == types.StatusBroadcastJID ||
		evt.Info.Chat.Server == types.NewsletterServer {
		return
	}

	if pm := evt.Message.GetProtocolMessage(); pm != nil {
		m.handleProtocolMessage(session, evt, pm)
		return
	}

	if enc := evt.Message.GetSecretEncryptedMessage(); enc != nil {
		m.handleSecretEncrypted(session, evt, enc)
		return
	}

	content, mediaErr := m.buildContent(session, evt, true)
	if content == nil {
		// A group's sender key rides its own enc block beside the words, and that leg
		// carries no content by design — nothing is lost when it says nothing.
		if evt.Message.GetSenderKeyDistributionMessage() != nil {
			return
		}
		// Everything else is loud on purpose: an inbound message the bridge cannot read
		// is one the consumer will never hear about, and a wire shape that changes under
		// us looks exactly like silence until someone goes looking.
		m.log.Warnf("Skipping unreadable message %s (type %s)", evt.Info.ID, evt.Info.Type)
		return
	}

	chat := evt.Info.Chat
	// Free knowledge: the namespace this chat speaks, for outbound mentions.
	session.noteAddressingMode(chat.String(), evt.Info.AddressingMode)

	senderSegment := session.Address
	if !evt.Info.IsFromMe {
		senderSegment = canonicalUser(session, evt.Info.Sender, evt.Info.SenderAlt)
	}

	// Replies: surface the quoted message as re_message_id (reactions
	// already carry their target there).
	if content.ReMessageID == "" {
		if stanza, participant := quotedRef(evt.Message); stanza != "" {
			content.ReMessageID = externalID(
				session.Address, chatSegment(session, evt.Info.MessageSource),
				keySender(session, chat, false, participant), stanza,
			)
		}
	}

	muted, archived := chatMarks(session, chat, evt.Info.Timestamp)
	message := WebhookMessage{
		ExternalID: externalID(
			session.Address, chatSegment(session, evt.Info.MessageSource),
			senderSegment, evt.Info.ID,
		),
		ConversationAddress: conversationAddressFor(session, evt.Info.MessageSource),
		SenderAddress:       senderAddressFor(session, evt.Info.MessageSource),
		Content:             *content,
		Timestamp:           evt.Info.Timestamp.Format(time.RFC3339),
		Muted:               muted,
		Archived:            archived,
	}
	// The event's pushname is the AUTHOR's, so it only speaks for the author:
	// on an echo it is our own name, and lending it to the peer would name the
	// chat after ourselves.
	live := ""
	if !evt.Info.IsFromMe {
		live = evt.Info.PushName
		message.SenderName = contactName(session, message.SenderAddress, live)
	}
	if evt.Info.IsGroup {
		message.ConversationName = session.groupName(chat.String())
	} else {
		// A direct chat IS its peer, so the peer's name names the room — the
		// only name a DM will ever have (WhatsApp has no subject for one).
		message.ConversationName = contactName(session, message.ConversationAddress, live)
	}

	if mediaErr != nil {
		// Keep the message (metadata + caption) but mark it errored; the
		// explicit status also keeps automation from processing a FilePart
		// that has no stored bytes.
		m.log.Errorf("Media handling failed for %s: %v", message.ExternalID, mediaErr)
		message.Status = map[string]any{
			"errors": []string{mediaErr.Error()},
		}
	}

	batch := WebhookBatch{OrganizationAddress: session.Address}

	if evt.Info.IsGroup {
		// First message from this group this process lifetime: send the
		// subject so the webhook can name the conversation. Off the event
		// loop — GetGroupInfo is a server round-trip.
		if session.markGroupSent(chat.String()) {
			go func() {
				info, err := session.Client.GetGroupInfo(context.Background(), chat)
				if err != nil {
					m.log.Warnf("GetGroupInfo %s failed: %v", chat, err)
					return
				}
				session.noteGroupName(chat.String(), info.Name)
				if err := m.openbsp.PostBatch(WebhookBatch{
					OrganizationAddress: session.Address,
					Groups: []WebhookGroup{
						{Address: chat.String(), Name: info.Name},
					},
				}); err != nil {
					m.log.Errorf("Post group subject for %s failed: %v", chat, err)
				}
			}()
		}
	}

	if evt.Info.IsFromMe {
		// Echo (bridge- or phone-sent): explicit status keeps it inert.
		if message.Status == nil {
			message.Status = map[string]any{}
		}
		message.Status["sent"] = evt.Info.Timestamp.Format(time.RFC3339)
	} else {
		// Live incoming: no status, so the pending default arms automation.
		if evt.Info.PushName != "" {
			batch.Contacts = append(batch.Contacts, WebhookContact{
				Address: message.SenderAddress,
				Extra:   map[string]any{"name": evt.Info.PushName},
			})
		}
	}

	batch.Messages = append(batch.Messages, message)

	// An offline queue is ONE arrival, however many messages it holds: everything said
	// while the host slept lands at once, and posted one by one it reads to the consumer
	// as a conversation happening now — a wake per message, each turn answering the
	// backlog as it grows. Held until the sync completes, it is a single batch: one
	// commit, one wake, one window with all of it.
	if session.holdOffline(batch) {
		return
	}

	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post message %s failed: %v", message.ExternalID, err)
	}
}

func (m *Manager) handleReceipt(session *Session, evt *events.Receipt) {
	if session.Address == "" {
		return
	}

	if evt.Chat == types.StatusBroadcastJID ||
		evt.Chat.Server == types.NewsletterServer {
		return
	}

	var key string
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		key = "delivered"
	case types.ReceiptTypeRead, types.ReceiptTypeReadSelf:
		key = "read"
	default:
		return
	}

	// The message AUTHOR segment of the external ID. Receipts from others are
	// always about our own sends — but our own devices echo receipts too (the
	// phone read the peer's message), and those are about the OTHER side's
	// messages: MessageSender names the author in groups, and in a direct chat
	// the author is the peer. Hardcoding session.Address here minted IDs no
	// message row carries, so no self receipt ever merged.
	author := session.Address
	if evt.IsFromMe {
		if evt.IsGroup {
			if evt.MessageSender.IsEmpty() {
				return // unattributable: don't mint IDs no row can match
			}
			author = canonicalUser(session, evt.MessageSender, types.JID{})
		} else {
			author = conversationAddressFor(session, evt.MessageSource)
		}
	}

	batch := WebhookBatch{OrganizationAddress: session.Address}

	// Group receipts are per participant, so the value is a map keyed by the
	// reader's canonical digits — OpenBSP's status merge is recursive, so
	// readers accumulate key by key. Direct chats keep the scalar: one peer,
	// one fact.
	var value any = evt.Timestamp.Format(time.RFC3339)
	if evt.IsGroup {
		value = map[string]any{
			canonicalUser(session, evt.Sender, evt.SenderAlt): evt.Timestamp.Format(time.RFC3339),
		}
	}

	for _, id := range evt.MessageIDs {
		status := WebhookStatus{
			ExternalID:          externalID(session.Address, chatSegment(session, evt.MessageSource), author, id),
			ConversationAddress: conversationAddressFor(session, evt.MessageSource),
			Status:              map[string]any{key: value},
		}
		batch.Statuses = append(batch.Statuses, status)
	}

	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post receipts for %s failed: %v", evt.Chat, err)
	}
}
