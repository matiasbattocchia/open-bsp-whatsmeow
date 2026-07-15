package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
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
}

func NewOpenBSP(cfg *Config) *OpenBSP {
	return &OpenBSP{
		baseURL: cfg.OpenBSPURL,
		token:   cfg.BridgeToken,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// WebhookBatch mirrors the connector webhook contract (see
// supabase/functions/_shared/connector_webhook.ts in open-bsp-api).
type WebhookBatch struct {
	OrganizationAddress string           `json:"organization_address"`
	Messages            []WebhookMessage `json:"messages,omitempty"`
	Statuses            []WebhookStatus  `json:"statuses,omitempty"`
	Contacts            []WebhookContact `json:"contacts,omitempty"`
	Edits               []WebhookEdit    `json:"edits,omitempty"`
	Revokes             []WebhookRevoke  `json:"revokes,omitempty"`
}

type WebhookMessage struct {
	ExternalID     string         `json:"external_id"`
	Direction      string         `json:"direction"` // incoming | outgoing
	ContactAddress string         `json:"contact_address,omitempty"`
	GroupAddress   string         `json:"group_address,omitempty"`
	Content        MessageContent `json:"content"`
	// Omitted for live messages (arms OpenBSP automation via the pending
	// default); explicit for history/echoes so they stay inert.
	Status    map[string]any `json:"status,omitempty"`
	Timestamp string         `json:"timestamp"`
}

type WebhookStatus struct {
	ExternalID     string         `json:"external_id"`
	ContactAddress string         `json:"contact_address,omitempty"`
	GroupAddress   string         `json:"group_address,omitempty"`
	Status         map[string]any `json:"status"`
}

type WebhookContact struct {
	Address string         `json:"address"`
	Extra   map[string]any `json:"extra,omitempty"`
}

type WebhookEdit struct {
	OriginalMessageID string `json:"original_message_id"`
	Text              string `json:"text"`
	Timestamp         string `json:"timestamp"`
}

type WebhookRevoke struct {
	OriginalMessageID string `json:"original_message_id"`
	Timestamp         string `json:"timestamp"`
}

// MessageContent is a v1 content Part: TextPart (type "text") or FilePart
// (type "file", where Text carries the caption). DataParts TODO.
type MessageContent struct {
	Version string       `json:"version"`
	Type    string       `json:"type"`
	Kind    string       `json:"kind"`
	Text    string       `json:"text,omitempty"`
	File    *FilePayload `json:"file,omitempty"`
}

type FilePayload struct {
	MimeType string `json:"mime_type"`
	URI      string `json:"uri"`
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

	req, err := http.NewRequest(
		http.MethodPost, o.baseURL+"/whatsapp-web-webhook/media", &body,
	)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+o.token)
	req.Header.Set("Content-Type", form.FormDataContentType())

	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("media upload responded %d", resp.StatusCode)
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
// management function owns all onboarding-related DB writes.
type SessionEvent struct {
	Event          string         `json:"event"` // connected | logged_out
	OrganizationID string         `json:"organization_id"`
	Address        string         `json:"address"`
	Extra          map[string]any `json:"extra,omitempty"`
}

func (o *OpenBSP) PostSessionEvent(event SessionEvent) error {
	return o.post("/whatsapp-web-management/sessions/events", event)
}
