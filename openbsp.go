package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// OpenBSP is the HTTP client for the two edge functions the bridge talks to:
// whatsapp-web-webhook (message traffic) and whatsapp-web-management
// (session lifecycle events). The bridge holds no Supabase credentials —
// just the shared bridge token.
type OpenBSP struct {
	baseURL string
	token   string
	http    *http.Client
	// Media uploads are sized in megabytes where the calls above are sized in
	// kilobytes, so they get their own client and a deadline per request
	// instead of one flat Timeout that has to suit both.
	mediaHTTP *http.Client
	// The receiver takes link-state events (Config.LinkEvents).
	LinkEvents bool
}

func NewOpenBSP(cfg *Config) *OpenBSP {
	return &OpenBSP{
		baseURL:    cfg.OpenBSPURL,
		token:      cfg.BridgeToken,
		http:       &http.Client{Timeout: 30 * time.Second},
		mediaHTTP:  &http.Client{},
		LinkEvents: cfg.LinkEvents,
	}
}

// How long to wait for an upload of size bytes. whatsmeow calls event
// handlers synchronously, so this deadline is also how long one medium can
// hold up the rest of the session's events — hence a budget that grows with
// the payload rather than a flat ceiling sized for the worst case. The floor
// covers a cold start of the edge function; the cap bounds the stall for a
// 50 MB upload (MAX_STORAGE_UPLOAD_SIZE) on a slow link.
func mediaUploadTimeout(size int) time.Duration {
	const (
		floor   = 30 * time.Second
		perMB   = 10 * time.Second
		ceiling = 5 * time.Minute
	)

	budget := floor + time.Duration(size/(1000*1000))*perMB
	if budget > ceiling {
		return ceiling
	}
	return budget
}

// WebhookBatch mirrors the connector webhook contract (see
// supabase/functions/_shared/connector_webhook.ts in open-bsp-api).
type WebhookBatch struct {
	OrganizationAddress string `json:"organization_address"`
	// True on history-import batches (pairing backfill) — the connector flags
	// those rows so automation skips them; never set on live traffic.
	History  bool             `json:"history,omitempty"`
	Messages []WebhookMessage `json:"messages,omitempty"`
	Statuses []WebhookStatus  `json:"statuses,omitempty"`
	Contacts []WebhookContact `json:"contacts,omitempty"`
	Groups   []WebhookGroup   `json:"groups,omitempty"`
	Edits    []WebhookEdit    `json:"edits,omitempty"`
	Revokes  []WebhookRevoke  `json:"revokes,omitempty"`
}

// WebhookGroup carries group metadata; the webhook applies Name to the
// matching conversation.
type WebhookGroup struct {
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
}

type WebhookMessage struct {
	ExternalID string `json:"external_id"`
	// The chat itself: group JID, or the peer's canonical number for direct
	// chats. Maps straight to messages.conversation_address.
	ConversationAddress string `json:"conversation_address"`
	// The contact who authored the message (group participant, or the DM
	// peer). Empty when the account itself spoke (echoes, history) — the
	// authorship marker OpenBSP branches on.
	SenderAddress string `json:"sender_address,omitempty"`
	// Who the addresses are, denormalized onto every message: SenderName is
	// what this account calls the author (address book first, pushname
	// otherwise), ConversationName the group's subject or — for a direct
	// chat — the peer's name. Both empty when nobody has ever named them,
	// and both absent for the account's own messages, which need no name.
	// A consumer keeping its own contact entity (the contacts/groups feeds
	// below) can ignore these; one without a directory needs them, since
	// the pushname is otherwise knowable only while the sender is speaking.
	SenderName       string         `json:"sender_name,omitempty"`
	ConversationName string         `json:"conversation_name,omitempty"`
	Content          MessageContent `json:"content"`
	// Omitted for live messages (arms OpenBSP automation via the pending
	// default); explicit for history/echoes so they stay inert.
	Status    map[string]any `json:"status,omitempty"`
	Timestamp string         `json:"timestamp"`
	// The chat's state when this message arrived, from the phone-synced
	// settings store (whatsmeow app state — the account's own mute/archive).
	// Stamped per message, the same denormalization as names: an unmute
	// reaches messages from the next one on, never retroactively. Live
	// traffic only — history rides the batch's History flag, which already
	// silences the whole import.
	Muted    bool `json:"muted,omitempty"`
	Archived bool `json:"archived,omitempty"`
}

type WebhookStatus struct {
	ExternalID          string         `json:"external_id"`
	ConversationAddress string         `json:"conversation_address"`
	Status              map[string]any `json:"status"`
}

type WebhookContact struct {
	Address string         `json:"address"`
	Extra   map[string]any `json:"extra,omitempty"`
}

type WebhookEdit struct {
	// The edit's OWN protocol-message id (namespaced like any message id), so the
	// consumer can log the edit as a first-class event; the original keeps its row.
	ExternalID          string `json:"external_id,omitempty"`
	OriginalMessageID   string `json:"original_message_id"`
	ConversationAddress string `json:"conversation_address,omitempty"`
	SenderAddress       string `json:"sender_address,omitempty"`
	Text                string `json:"text"`
	Timestamp           string `json:"timestamp"`
	// The chat's state on arrival (see WebhookMessage) — an edit is its own
	// event, so it carries the state too: else an edit in a muted chat would
	// wake what the chat cannot.
	Muted    bool `json:"muted,omitempty"`
	Archived bool `json:"archived,omitempty"`
}

type WebhookRevoke struct {
	ExternalID          string `json:"external_id,omitempty"` // the revoke's own id (see WebhookEdit)
	OriginalMessageID   string `json:"original_message_id"`
	ConversationAddress string `json:"conversation_address,omitempty"`
	SenderAddress       string `json:"sender_address,omitempty"`
	Timestamp           string `json:"timestamp"`
}

// MessageContent is a v1 content Part: TextPart (type "text", kinds
// text/reaction), FilePart (type "file", Text carries the caption), or
// DataPart (type "data", kinds location/contacts, payload in Data using the
// same shapes as the Cloud API service).
//
// Outbound, three data kinds act on a message already sent rather than carrying
// content of their own, each naming its referent in ReMessageID: "reaction"
// (Data {action, unicode} — a removal is the empty reaction), "edit" (the
// replacement body in Text) and "revoke".
type MessageContent struct {
	Version     string          `json:"version"`
	Type        string          `json:"type"`
	Kind        string          `json:"kind"`
	Text        string          `json:"text,omitempty"`
	File        *FilePayload    `json:"file,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	ReMessageID string          `json:"re_message_id,omitempty"`
	Forwarded   bool            `json:"forwarded,omitempty"`
	Mentions    []Mention       `json:"mentions,omitempty"`
}

// Mention is a soft reference to who the text calls out. Address is the
// participant's canonical digits, and the inline "@digits" in Text is in
// that same namespace both ways — the bridge translates to and from the
// chat's wire namespace (LID or phone), so OpenBSP only ever sees canonical
// addresses. Name is the display text a composer used ("@Ana"), which
// outbound sends substitute back to @digits. AgentID is OpenBSP's
// member-space key and passes through untouched.
type Mention struct {
	Address string `json:"address,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Name    string `json:"name,omitempty"`
}

// LocationData mirrors the Cloud API location object used by the
// 'whatsapp' service, so both services share one DataPart shape.
type LocationData struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Name      string  `json:"name,omitempty"`
	Address   string  `json:"address,omitempty"`
}

// ContactData mirrors the Cloud API contacts object (subset).
type ContactData struct {
	Name struct {
		FormattedName string `json:"formatted_name"`
		FirstName     string `json:"first_name,omitempty"`
	} `json:"name"`
	Phones []struct {
		Phone string `json:"phone"`
		WaID  string `json:"wa_id,omitempty"`
		Type  string `json:"type,omitempty"`
	} `json:"phones,omitempty"`
}

// FilePayload describes a medium. A FilePart always carries a URI; a
// media_placeholder carries the same struct with the URI omitted, which is
// the whole of what it is saying.
type FilePayload struct {
	MimeType string `json:"mime_type"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

func (o *OpenBSP) post(path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, o.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("openbsp %s responded %d", path, resp.StatusCode)
	}
	return nil
}

func (o *OpenBSP) PostBatch(batch WebhookBatch) error {
	return o.post("/whatsapp-web-webhook", batch)
}

// UploadMedia stores decrypted media bytes in OpenBSP storage via the
// connector webhook's /media route and returns the internal://media/... URI
// to reference in a FilePart. The bridge holds no storage credentials; the
// webhook (which has the service key) does the actual upload and enforces
// the size cap.
func (o *OpenBSP) UploadMedia(organizationAddress, name string, data []byte) (string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)

	filename := name
	if filename == "" {
		filename = "file"
	}
	part, err := form.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if name != "" {
		if err := form.WriteField("name", name); err != nil {
			return "", err
		}
	}
	if err := form.WriteField("organization_address", organizationAddress); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(
		context.Background(), mediaUploadTimeout(len(data)),
	)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, o.baseURL+"/whatsapp-web-webhook/media", &body,
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := o.mediaHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Include the body: a 402 carries the billing-cap reason (e.g.
		// "Usage limit reached for storage"), which ends up verbatim in the
		// message's status.errors for the org to see.
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", fmt.Errorf(
			"media upload responded %d: %s", resp.StatusCode,
			strings.TrimSpace(string(reason)),
		)
	}

	var result struct {
		URI string `json:"uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.URI, nil
}

// SessionEvent notifies whatsapp-web-management of a lifecycle change; the
// management function owns all onboarding-related DB writes. `connected` is
// posted on pairing and, with LinkEvents, on every reconnect; `disconnected`
// only with LinkEvents.
type SessionEvent struct {
	Event          string `json:"event"` // connected | disconnected | logged_out
	OrganizationID string `json:"organization_id"`
	Address        string `json:"address"`
	// Empty for the org's shared inbox; set for a personal session.
	AgentID string         `json:"agent_id,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

func (o *OpenBSP) PostSessionEvent(event SessionEvent) error {
	return o.post("/whatsapp-web-management/sessions/events", event)
}
