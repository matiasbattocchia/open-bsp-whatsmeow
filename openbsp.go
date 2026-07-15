package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// MessageContent is a v1 content Part (TextPart for now; FilePart/DataPart
// TODO along with media support).
type MessageContent struct {
	Version string `json:"version"`
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Text    string `json:"text,omitempty"`
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
