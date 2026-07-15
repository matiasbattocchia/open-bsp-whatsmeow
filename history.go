package main

import (
	"time"

	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// History import batch size: syncs can carry thousands of messages, so they
// are flushed to the connector webhook in chunks.
const historyBatchSize = 200

// handleHistorySync imports the on-phone history WhatsApp pushes after
// pairing. Every imported row carries an explicit FINAL status (never
// status.pending): the pending default is OpenBSP's automation gate, and a
// backfill must not trigger agent replies, media preprocessing, or webhook
// fan-out for months-old messages. Media bytes are not imported (old media
// is frequently gone from the CDN) — FileParts keep their metadata but no
// URI, which the UI renders as an unavailable attachment.
func (m *Manager) handleHistorySync(session *Session, evt *events.HistorySync) {
	if session.Address == "" {
		return
	}

	syncType := evt.Data.GetSyncType().String()
	m.log.Infof("History sync (%s) for %s: %d conversations, %d pushnames",
		syncType, session.Address,
		len(evt.Data.GetConversations()), len(evt.Data.GetPushnames()))

	// PUSH_NAME syncs: contact names.
	if pushnames := evt.Data.GetPushnames(); len(pushnames) > 0 {
		batch := WebhookBatch{OrganizationAddress: session.Address}
		for _, pushname := range pushnames {
			jid, err := types.ParseJID(pushname.GetID())
			if err != nil || pushname.GetPushname() == "" {
				continue
			}
			batch.Contacts = append(batch.Contacts, WebhookContact{
				Address: canonicalUser(session, jid, types.JID{}),
				Extra:   map[string]any{"name": pushname.GetPushname()},
			})
		}
		if len(batch.Contacts) > 0 {
			if err := m.openbsp.PostBatch(batch); err != nil {
				m.log.Errorf("Post history pushnames failed: %v", err)
			}
		}
	}

	pending := WebhookBatch{OrganizationAddress: session.Address}
	flush := func() {
		if len(pending.Messages) == 0 {
			return
		}
		if err := m.openbsp.PostBatch(pending); err != nil {
			m.log.Errorf("Post history batch failed: %v", err)
		}
		pending = WebhookBatch{OrganizationAddress: session.Address}
	}

	imported, skipped := 0, 0
	for _, conversation := range evt.Data.GetConversations() {
		chat, err := types.ParseJID(conversation.GetID())
		if err != nil {
			continue
		}

		for _, historyMsg := range conversation.GetMessages() {
			webMsg := historyMsg.GetMessage()
			parsed, err := session.Client.ParseWebMessage(chat, webMsg)
			if err != nil {
				skipped++
				continue
			}

			// Protocol messages and reactions reference other rows; edits
			// and revokes are already reflected in the history content, and
			// standalone reaction rows would be noise.
			if parsed.Message.GetProtocolMessage() != nil ||
				parsed.Message.GetReactionMessage() != nil {
				skipped++
				continue
			}

			content, _ := m.buildContent(session, parsed, false)
			if content == nil {
				skipped++
				continue
			}

			if content.ReMessageID == "" {
				if stanza := quotedStanzaID(parsed.Message); stanza != "" {
					content.ReMessageID = externalID(session.Address, chat.User, stanza)
				}
			}

			message := WebhookMessage{
				ExternalID:     externalID(session.Address, chat.User, parsed.Info.ID),
				ContactAddress: contactAddressFor(session, parsed.Info.MessageSource),
				Content:        *content,
				Status:         historyStatus(webMsg, parsed),
				Timestamp:      parsed.Info.Timestamp.Format(time.RFC3339),
			}
			if parsed.Info.IsGroup {
				message.GroupAddress = chat.String()
			}
			if parsed.Info.IsFromMe {
				message.Direction = "outgoing"
			} else {
				message.Direction = "incoming"
			}

			pending.Messages = append(pending.Messages, message)
			imported++
			if len(pending.Messages) >= historyBatchSize {
				flush()
			}
		}
	}
	flush()

	m.log.Infof("History sync (%s) for %s done: %d imported, %d skipped",
		syncType, session.Address, imported, skipped)
}

// historyStatus maps a history message to its explicit final status.
// Incoming history is stamped read (it predates the pairing); outgoing
// follows the ack recorded by the phone.
func historyStatus(webMsg *waWeb.WebMessageInfo, parsed *events.Message) map[string]any {
	timestamp := parsed.Info.Timestamp.Format(time.RFC3339)

	if !parsed.Info.IsFromMe {
		return map[string]any{"read": timestamp}
	}

	switch webMsg.GetStatus() {
	case waWeb.WebMessageInfo_READ, waWeb.WebMessageInfo_PLAYED:
		return map[string]any{"read": timestamp}
	case waWeb.WebMessageInfo_DELIVERY_ACK:
		return map[string]any{"delivered": timestamp}
	default:
		return map[string]any{"sent": timestamp}
	}
}
