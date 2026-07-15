package main

import (
	"time"

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

func (m *Manager) handleMessage(session *Session, evt *events.Message) {
	if session.Address == "" {
		return // still pairing
	}

	// TODO(v1): media messages — DownloadAny + POST /media, then send a
	// FilePart with the returned internal:// URI. Text only for now.
	text := evt.Message.GetConversation()
	if text == "" {
		text = evt.Message.GetExtendedTextMessage().GetText()
	}
	if text == "" {
		m.log.Debugf("Skipping non-text message %s (type %s)", evt.Info.ID, evt.Info.Type)
		return
	}

	chat := evt.Info.Chat
	sender := evt.Info.Sender

	message := WebhookMessage{
		ExternalID: externalID(session.Address, chat.User, evt.Info.ID),
		Content: MessageContent{
			Version: "1",
			Type:    "text",
			Kind:    "text",
			Text:    text,
		},
		Timestamp: evt.Info.Timestamp.Format(time.RFC3339),
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
		message.Status = map[string]any{
			"sent": evt.Info.Timestamp.Format(time.RFC3339),
		}
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
