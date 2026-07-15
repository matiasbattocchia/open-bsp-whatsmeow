package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
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
		message, status, err := buildOutgoingMessage(r, session, chat, req)
		if err != nil {
			http.Error(w, err.Error(), status)
			return
		}

		resp, err := session.Client.SendMessage(r.Context(), chat, message)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		writeJSON(w, map[string]any{
			"external_id": externalID(session.Address, chat.User, session.Address, resp.ID),
			"status":      "sent",
		})

	case "status":
		// Read receipt and/or typing indicator for an incoming message; the
		// dispatcher forwards the row when either status key changed
		// recently, mirroring whatsapp-dispatcher.
		recent := func(key string) bool {
			value, _ := req.Record.Status[key].(string)
			if value == "" {
				return false
			}
			ts, err := time.Parse(time.RFC3339, value)
			return err == nil && time.Since(ts) <= time.Minute
		}

		if recent("typing") {
			if err := session.Client.SendChatPresence(
				r.Context(), chat, types.ChatPresenceComposing, types.ChatPresenceMediaText,
			); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		}

		if recent("read") {
			_, _, senderSegment, id, err := parseExternalID(req.Record.ExternalID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusUnprocessableEntity)
				return
			}

			sender := chat
			if senderSegment != "" && senderSegment != session.Address {
				sender = types.NewJID(senderSegment, types.DefaultUserServer)
			}

			if err := session.Client.MarkRead(
				r.Context(), []types.MessageID{id}, time.Now(), chat, sender,
			); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		}

		writeJSON(w, map[string]any{})

	default:
		http.Error(w, "unknown dispatch type "+req.Type, http.StatusUnprocessableEntity)
	}
}

// referencedKey reconstructs the WhatsApp MessageKey pieces of a referenced
// message from its external id: the sender segment yields both direction
// (sender == own) and the participant JID.
func referencedKey(session *Session, content MessageContent) (id string, sender types.JID, ok bool) {
	_, _, senderSegment, id, err := parseExternalID(content.ReMessageID)
	if err != nil || senderSegment == "" {
		return "", types.JID{}, false
	}

	if senderSegment == session.Address {
		if session.Client.Store.ID == nil {
			return "", types.JID{}, false
		}
		return id, session.Client.Store.ID.ToNonAD(), true
	}
	return id, types.NewJID(senderSegment, types.DefaultUserServer), true
}

// replyContext turns re_message_id into a quote (ContextInfo), matching the
// Cloud API dispatcher: no context on forwards.
func replyContext(session *Session, content MessageContent) *waE2E.ContextInfo {
	if content.ReMessageID == "" || content.Forwarded {
		return nil
	}
	id, sender, ok := referencedKey(session, content)
	if !ok {
		return nil
	}
	return &waE2E.ContextInfo{
		StanzaID:      proto.String(id),
		Participant:   proto.String(sender.String()),
		QuotedMessage: &waE2E.Message{Conversation: proto.String("")},
	}
}

func optString(value string) *string {
	if value == "" {
		return nil
	}
	return proto.String(value)
}

// buildOutgoingMessage converts an OpenBSP content Part into a WhatsApp
// message. Feature parity with the 'whatsapp' (Cloud API) dispatcher: text,
// reaction, media kinds, location, contacts. Templates are the one
// protocol-level impossibility on WhatsApp Web and fail permanently.
// Returns an HTTP status alongside the error: 4xx = permanent, 5xx =
// transient.
func buildOutgoingMessage(
	r *http.Request, session *Session, chat types.JID, req dispatchRequest,
) (*waE2E.Message, int, error) {
	content := req.Record.Content

	switch content.Type {
	case "text":
		if content.Kind == "reaction" {
			id, sender, ok := referencedKey(session, content)
			if !ok {
				return nil, http.StatusUnprocessableEntity,
					fmt.Errorf("reaction without a valid re_message_id: %s", content.ReMessageID)
			}
			return session.Client.BuildReaction(chat, sender, id, content.Text),
				0, nil
		}

		text := markdownToWhatsApp(content.Text)
		if ctx := replyContext(session, content); ctx != nil {
			return &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text:        proto.String(text),
					ContextInfo: ctx,
				},
			}, 0, nil
		}
		return &waE2E.Message{Conversation: proto.String(text)}, 0, nil

	case "file":
		return buildMediaMessage(r, session, chat, req)

	case "data":
		switch content.Kind {
		case "location":
			var location LocationData
			if err := json.Unmarshal(content.Data, &location); err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("invalid location data: %w", err)
			}
			return &waE2E.Message{
				LocationMessage: &waE2E.LocationMessage{
					DegreesLatitude:  &location.Latitude,
					DegreesLongitude: &location.Longitude,
					Name:             optString(location.Name),
					Address:          optString(location.Address),
					ContextInfo:      replyContext(session, content),
				},
			}, 0, nil

		case "contacts":
			var contacts []ContactData
			if err := json.Unmarshal(content.Data, &contacts); err != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("invalid contacts data: %w", err)
			}
			if len(contacts) == 0 {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("empty contacts data")
			}

			cards := make([]*waE2E.ContactMessage, 0, len(contacts))
			for _, contact := range contacts {
				cards = append(cards, contactToVcard(contact))
			}
			if len(cards) == 1 {
				cards[0].ContextInfo = replyContext(session, content)
				return &waE2E.Message{ContactMessage: cards[0]}, 0, nil
			}
			return &waE2E.Message{
				ContactsArrayMessage: &waE2E.ContactsArrayMessage{
					DisplayName: proto.String(fmt.Sprintf("%d contacts", len(cards))),
					Contacts:    cards,
					ContextInfo: replyContext(session, content),
				},
			}, 0, nil

		case "template":
			return nil, http.StatusUnprocessableEntity,
				fmt.Errorf("templates are not supported on the whatsapp-web service")

		default:
			return nil, http.StatusUnprocessableEntity,
				fmt.Errorf("unsupported data kind %s", content.Kind)
		}

	default:
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("unsupported content type %s", content.Type)
	}
}

func contactToVcard(contact ContactData) *waE2E.ContactMessage {
	name := contact.Name.FormattedName
	if name == "" {
		name = contact.Name.FirstName
	}

	var vcard strings.Builder
	vcard.WriteString("BEGIN:VCARD\nVERSION:3.0\nFN:" + name + "\n")
	for _, phone := range contact.Phones {
		if phone.WaID != "" {
			fmt.Fprintf(&vcard, "TEL;type=CELL;waid=%s:%s\n", phone.WaID, phone.Phone)
		} else {
			fmt.Fprintf(&vcard, "TEL;type=CELL:%s\n", phone.Phone)
		}
	}
	vcard.WriteString("END:VCARD")

	return &waE2E.ContactMessage{
		DisplayName: proto.String(name),
		Vcard:       proto.String(vcard.String()),
	}
}

// WhatsApp per-type upload limits, mirroring whatsapp-dispatcher's table.
// Exceeding them is a permanent (4xx) failure.
var whatsappMaxFileSize = map[string]int64{
	"audio":    16 * 1000 * 1000,
	"document": 100 * 1000 * 1000,
	"image":    5 * 1000 * 1000,
	"sticker":  500 * 1000,
	"video":    16 * 1000 * 1000,
}

var kindToMediaType = map[string]whatsmeow.MediaType{
	"image":    whatsmeow.MediaImage,
	"sticker":  whatsmeow.MediaImage,
	"audio":    whatsmeow.MediaAudio,
	"video":    whatsmeow.MediaVideo,
	"document": whatsmeow.MediaDocument,
}

// buildMediaMessage turns an outgoing FilePart into a WhatsApp media
// message: fetch the bytes from the signed media_url the dispatcher
// embedded, Upload() (encrypts + pushes to WhatsApp's CDN), and copy the
// resulting keys/hashes into the per-kind protobuf. Returns an HTTP status
// alongside the error: 4xx = permanent, 5xx = transient.
func buildMediaMessage(r *http.Request, session *Session, chat types.JID, req dispatchRequest) (*waE2E.Message, int, error) {
	file := req.Record.Content.File
	kind := req.Record.Content.Kind
	caption := req.Record.Content.Text

	if file == nil {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("file part without file payload")
	}
	if req.MediaURL == "" {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("file part without media_url")
	}

	mediaType, ok := kindToMediaType[kind]
	if !ok {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("unsupported file kind %s", kind)
	}

	maxSize := whatsappMaxFileSize[kind]
	if file.Size > maxSize {
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("file too large for WhatsApp: %d bytes (limit %d for %s)", file.Size, maxSize, kind)
	}

	resp, err := http.Get(req.MediaURL)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("fetch media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, http.StatusBadGateway, fmt.Errorf("fetch media: storage responded %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("read media: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("file too large for WhatsApp: >%d bytes for %s", maxSize, kind)
	}

	upload, err := session.Client.Upload(r.Context(), data, mediaType)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Errorf("upload to WhatsApp: %w", err)
	}

	mimetype := proto.String(file.MimeType)
	captionPtr := optString(markdownToWhatsApp(caption))
	contextInfo := replyContext(session, req.Record.Content)

	message := &waE2E.Message{}
	switch kind {
	case "image":
		message.ImageMessage = &waE2E.ImageMessage{
			ContextInfo:   contextInfo,
			Caption:       captionPtr,
			Mimetype:      mimetype,
			URL:           &upload.URL,
			DirectPath:    &upload.DirectPath,
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    &upload.FileLength,
		}
	case "sticker":
		message.StickerMessage = &waE2E.StickerMessage{
			ContextInfo:   contextInfo,
			Mimetype:      mimetype,
			URL:           &upload.URL,
			DirectPath:    &upload.DirectPath,
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    &upload.FileLength,
		}
	case "audio":
		message.AudioMessage = &waE2E.AudioMessage{
			ContextInfo:   contextInfo,
			Mimetype:      mimetype,
			URL:           &upload.URL,
			DirectPath:    &upload.DirectPath,
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    &upload.FileLength,
		}
	case "video":
		message.VideoMessage = &waE2E.VideoMessage{
			ContextInfo:   contextInfo,
			Caption:       captionPtr,
			Mimetype:      mimetype,
			URL:           &upload.URL,
			DirectPath:    &upload.DirectPath,
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    &upload.FileLength,
		}
	case "document":
		var namePtr *string
		if file.Name != "" {
			namePtr = proto.String(file.Name)
		}
		message.DocumentMessage = &waE2E.DocumentMessage{
			ContextInfo:   contextInfo,
			Caption:       captionPtr,
			FileName:      namePtr,
			Mimetype:      mimetype,
			URL:           &upload.URL,
			DirectPath:    &upload.DirectPath,
			MediaKey:      upload.MediaKey,
			FileEncSHA256: upload.FileEncSHA256,
			FileSHA256:    upload.FileSHA256,
			FileLength:    &upload.FileLength,
		}
	}

	return message, 0, nil
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
