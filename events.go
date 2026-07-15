package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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
		if session.Address == "" && session.Client.Store.ID != nil {
			m.completePairing(session, *session.Client.Store.ID)
		}

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

	case *events.Message:
		m.handleMessage(session, v)

	case *events.Receipt:
		m.handleReceipt(session, v)

		// TODO(v1): *events.HistorySync — import with explicit final statuses
		// (see the connector webhook contract; never status.pending).
	}
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

// quotedStanzaID pulls the reply target (quoted message id) out of whichever
// message type carries the ContextInfo.
func quotedStanzaID(msg *waE2E.Message) string {
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
			return ctx.GetStanzaID()
		}
	}
	return ""
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

// buildContent extracts a v1 content Part from the event. Media is
// downloaded+decrypted (DownloadAny) and stored via the webhook's /media
// route; on failure the FilePart is preserved without a URI and mediaErr is
// returned so the message can carry an error status instead of silently
// dropping (mirrors whatsapp-webhook's oversized-media path).
func (m *Manager) buildContent(session *Session, evt *events.Message) (content *MessageContent, mediaErr error) {
	text := evt.Message.GetConversation()
	if text == "" {
		text = evt.Message.GetExtendedTextMessage().GetText()
	}
	if text != "" {
		return &MessageContent{Version: "1", Type: "text", Kind: "text", Text: text}, nil
	}

	if reaction := evt.Message.GetReactionMessage(); reaction != nil {
		return &MessageContent{
			Version: "1",
			Type:    "text",
			Kind:    "reaction",
			Text:    reaction.GetText(), // empty text = reaction removed
			ReMessageID: externalID(
				session.Address, evt.Info.Chat.User, reaction.GetKey().GetID(),
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

	content = &MessageContent{
		Version: "1",
		Type:    "file",
		Kind:    media.kind,
		Text:    media.caption,
		File:    &FilePayload{MimeType: media.mime, Name: media.name},
	}

	data, err := session.Client.DownloadAny(context.Background(), evt.Message)
	if err != nil {
		return content, fmt.Errorf("download media: %w", err)
	}

	uri, err := m.openbsp.UploadMedia(session.Address, media.name, data)
	if err != nil {
		return content, fmt.Errorf("store media: %w", err)
	}

	content.File.URI = uri
	content.File.Size = int64(len(data))
	return content, nil
}

// handleProtocolMessage translates edits and revokes into the webhook's
// edits/revokes arrays (applied as in-place updates keyed by the ORIGINAL
// external id). Other protocol messages (app state, key distribution, ...)
// are internal noise and ignored.
func (m *Manager) handleProtocolMessage(session *Session, evt *events.Message, pm *waE2E.ProtocolMessage) {
	batch := WebhookBatch{OrganizationAddress: session.Address}
	original := externalID(session.Address, evt.Info.Chat.User, pm.GetKey().GetID())
	timestamp := evt.Info.Timestamp.Format(time.RFC3339)

	switch pm.GetType() {
	case waE2E.ProtocolMessage_REVOKE:
		batch.Revokes = append(batch.Revokes, WebhookRevoke{
			OriginalMessageID: original,
			Timestamp:         timestamp,
		})
	case waE2E.ProtocolMessage_MESSAGE_EDIT:
		edited := pm.GetEditedMessage()
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
			m.log.Debugf("Unsupported edit content on %s", original)
			return
		}
		batch.Edits = append(batch.Edits, WebhookEdit{
			OriginalMessageID: original,
			Text:              text,
			Timestamp:         timestamp,
		})
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

	if pm := evt.Message.GetProtocolMessage(); pm != nil {
		m.handleProtocolMessage(session, evt, pm)
		return
	}

	content, mediaErr := m.buildContent(session, evt)
	if content == nil {
		m.log.Debugf("Skipping unsupported message %s (type %s)", evt.Info.ID, evt.Info.Type)
		return
	}

	chat := evt.Info.Chat
	sender := evt.Info.Sender

	// Replies: surface the quoted message as re_message_id (reactions
	// already carry their target there).
	if content.ReMessageID == "" {
		if stanza := quotedStanzaID(evt.Message); stanza != "" {
			content.ReMessageID = externalID(session.Address, chat.User, stanza)
		}
	}

	message := WebhookMessage{
		ExternalID: externalID(session.Address, chat.User, evt.Info.ID),
		Content:    *content,
		Timestamp:  evt.Info.Timestamp.Format(time.RFC3339),
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

	if evt.Info.IsGroup {
		message.GroupAddress = chat.String()
		message.ContactAddress = sender.User
	} else {
		message.ContactAddress = chat.User
	}

	batch := WebhookBatch{OrganizationAddress: session.Address}

	if evt.Info.IsFromMe {
		// Echo (bridge- or phone-sent): explicit status keeps it inert.
		message.Direction = "outgoing"
		if message.Status == nil {
			message.Status = map[string]any{}
		}
		message.Status["sent"] = evt.Info.Timestamp.Format(time.RFC3339)
	} else {
		// Live incoming: no status, so the pending default arms automation.
		message.Direction = "incoming"
		if evt.Info.PushName != "" {
			batch.Contacts = append(batch.Contacts, WebhookContact{
				Address: sender.User,
				Extra:   map[string]any{"name": evt.Info.PushName},
			})
		}
	}

	batch.Messages = append(batch.Messages, message)

	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post message %s failed: %v", message.ExternalID, err)
	}
}

func (m *Manager) handleReceipt(session *Session, evt *events.Receipt) {
	if session.Address == "" {
		return
	}

	var key string
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		key = "delivered"
	case types.ReceiptTypeRead:
		key = "read"
	default:
		return
	}

	batch := WebhookBatch{OrganizationAddress: session.Address}

	for _, id := range evt.MessageIDs {
		status := WebhookStatus{
			ExternalID: externalID(session.Address, evt.Chat.User, id),
			Status: map[string]any{
				key: evt.Timestamp.Format(time.RFC3339),
			},
		}
		if evt.IsGroup {
			status.GroupAddress = evt.Chat.String()
			status.ContactAddress = evt.Sender.User
		} else {
			status.ContactAddress = evt.Chat.User
		}
		batch.Statuses = append(batch.Statuses, status)
	}

	if err := m.openbsp.PostBatch(batch); err != nil {
		m.log.Errorf("Post receipts for %s failed: %v", evt.Chat, err)
	}
}
