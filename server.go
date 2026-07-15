package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Server accepts server-to-server calls from whatsapp-web-dispatcher
// (/dispatch) and whatsapp-web-management (/sessions...). The UI never talks
// to the bridge directly.
type Server struct {
	cfg     *Config
	manager *Manager
	log     waLog.Logger
}

func NewServer(cfg *Config, manager *Manager, log waLog.Logger) *Server {
	return &Server{cfg: cfg, manager: manager, log: log}
}

func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /dispatch", s.auth(s.handleDispatch))
	mux.HandleFunc("POST /sessions", s.auth(s.handleCreateSession))
	mux.HandleFunc("GET /sessions/pending/{id}", s.auth(s.handlePendingState))
	mux.HandleFunc("GET /sessions/{address}", s.auth(s.handleSessionStatus))
	mux.HandleFunc("DELETE /sessions/{address}", s.auth(s.handleLogout))

	s.log.Infof("Listening on %s", s.cfg.ListenAddr)
	return http.ListenAndServe(s.cfg.ListenAddr, mux)
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get("Authorization")
		if token != "Bearer "+s.cfg.BridgeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// dispatchRequest mirrors the connector dispatcher contract (see
// supabase/functions/_shared/connector_dispatcher.ts in open-bsp-api):
// a "message" forwards an outgoing MessageRow verbatim; a "status" forwards
// an incoming row whose read/typing status changed.
type dispatchRequest struct {
	Type   string `json:"type"` // message | status
	Record struct {
		ID                  string         `json:"id"`
		ExternalID          string         `json:"external_id"`
		OrganizationAddress string         `json:"organization_address"`
		ContactAddress      string         `json:"contact_address"`
		GroupAddress        string         `json:"group_address"`
		Content             MessageContent `json:"content"`
		Status              map[string]any `json:"status"`
	} `json:"record"`
	// Signed download URL for content.file.uri (media TODO).
	MediaURL string `json:"media_url"`
}

// Response codes follow the dispatcher's transient/permanent split:
// 4xx marks the message failed (no retry), 5xx keeps it pending for the
// dispatch cron.
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	var req dispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	session := s.manager.Get(req.Record.OrganizationAddress)
	if session == nil {
		// Unknown session is permanent from this bridge's point of view.
		http.Error(w, "unknown session "+req.Record.OrganizationAddress, http.StatusNotFound)
		return
	}
	if !session.Client.IsConnected() {
		http.Error(w, "session not connected", http.StatusServiceUnavailable)
		return
	}

	chat, err := dispatchChatJID(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	switch req.Type {
	case "message":
		if req.Record.Content.Type != "text" {
			// TODO(v1): FilePart via MediaURL download + Client.Upload;
			// DataParts (location, contacts). Permanent until implemented.
			http.Error(w, "unsupported content type "+req.Record.Content.Type,
				http.StatusUnprocessableEntity)
			return
		}

		resp, err := session.Client.SendMessage(r.Context(), chat, &waE2E.Message{
			Conversation: proto.String(req.Record.Content.Text),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeJSON(w, map[string]any{
			"external_id": externalID(session.Address, chat.User, resp.ID),
			"status":      "sent",
		})

	case "status":
		// Read receipt for an incoming message.
		_, _, id, err := parseExternalID(req.Record.ExternalID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}

		sender := chat
		if req.Record.GroupAddress != "" && req.Record.ContactAddress != "" {
			sender = types.NewJID(req.Record.ContactAddress, types.DefaultUserServer)
		}

		if err := session.Client.MarkRead(
			r.Context(), []types.MessageID{id}, time.Now(), chat, sender,
		); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeJSON(w, map[string]any{})

	default:
		http.Error(w, "unknown dispatch type "+req.Type, http.StatusUnprocessableEntity)
	}
}

func dispatchChatJID(req dispatchRequest) (types.JID, error) {
	if req.Record.GroupAddress != "" {
		return types.ParseJID(req.Record.GroupAddress)
	}
	if req.Record.ContactAddress != "" {
		return types.NewJID(req.Record.ContactAddress, types.DefaultUserServer), nil
	}
	return types.JID{}, fmt.Errorf("record has neither contact_address nor group_address")
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrganizationID string `json:"organization_id"`
		PhoneNumber    string `json:"phone_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.OrganizationID == "" {
		http.Error(w, "organization_id is required", http.StatusBadRequest)
		return
	}

	result, err := s.manager.CreateSession(r.Context(), req.OrganizationID, req.PhoneNumber)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, result)
}

// Polled by the UI (through whatsapp-web-management) during pairing: QR
// codes rotate every ~20s, so the latest one is always available here, and
// status flips to paired/error on completion.
func (s *Server) handlePendingState(w http.ResponseWriter, r *http.Request) {
	state := s.manager.PendingState(r.PathValue("id"))
	if state == nil {
		http.Error(w, "unknown pairing session", http.StatusNotFound)
		return
	}
	writeJSON(w, state)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	status := s.manager.Status(r.PathValue("address"))
	if status == nil {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}
	writeJSON(w, status)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.manager.Logout(r.Context(), r.PathValue("address")); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
