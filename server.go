package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime/debug"
	"sort"
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
		// A panic would otherwise kill the connection with no response —
		// the dispatcher reads that as a network (transient) error. Answer
		// 500 instead: still transient, but logged and well-formed.
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Errorf("panic in %s %s: %v\n%s", r.Method, r.URL.Path, rec, debug.Stack())
				http.Error(w, fmt.Sprintf("panic: %v", rec), http.StatusInternalServerError)
			}
		}()

		token := r.Header.Get("Authorization")
		if token != "Bearer "+s.cfg.BridgeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// sendErrorStatus maps a SendMessage failure onto the dispatcher's
// transient/permanent split. Connectivity and timeouts stay 5xx — the
// dispatch cron retries; what the message or its recipient makes impossible
// is 4xx — the dispatcher stamps the row failed instead of re-sending it
// every minute for 12 hours. ErrServerReturnedError is on the permanent
// side deliberately: the server actively rejected this message, and the
// same bytes will be rejected again (mirrors whatsapp-dispatcher, where
// unknown Meta codes default to permanent).
func sendErrorStatus(err error) int {
	for _, permanent := range []error{
		whatsmeow.ErrBroadcastListUnsupported,
		whatsmeow.ErrUnknownServer,
		whatsmeow.ErrRecipientADJID,
		whatsmeow.ErrInvalidInlineBotID,
		whatsmeow.ErrServerReturnedError,
	} {
		if errors.Is(err, permanent) {
			return http.StatusUnprocessableEntity
		}
	}
	return http.StatusBadGateway
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
		ConversationAddress string         `json:"conversation_address"`
		Content             MessageContent `json:"content"`
		Status              map[string]any `json:"status"`
	} `json:"record"`
	// Where the bytes are fetched from for a file part: a signed download URL
	// for content.file.uri. May be RELATIVE ("/m/<signed>"), in which case it
	// resolves against the session's receiver — the consumer's own address,
	// the one this bridge already delivers to, so it never has to state its
	// host to us twice.
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

	if req.MediaURL != "" {
		resolved, err := resolveMediaURL(session.receiver.Base(), req.MediaURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		req.MediaURL = resolved
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
			http.Error(w, err.Error(), sendErrorStatus(err))
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

// encodeMentions rewrites @Name tokens to WhatsApp's inline @digits form and
// returns the full JIDs for ContextInfo.MentionedJID. Entries without an
// address are skipped; without a name the text is left as the composer wrote
// it (it may already carry @digits).
//
// The digits are the CHAT's namespace, not ours: a lid-addressed group knows
// its members by LID, so a phone-number mention neither binds nor belongs —
// it would publish a number into a chat whose addressing exists to hide it.
// Callers pass the canonical (phone) address; wireMention maps it across.
func encodeMentions(session *Session, chat types.JID, content MessageContent, text string) (string, []string) {
	if len(content.Mentions) == 0 {
		return text, nil
	}

	// Longest name first, so "@Ana María" is not half-claimed by "@Ana".
	mentions := append([]Mention(nil), content.Mentions...)
	sort.Slice(mentions, func(i, j int) bool {
		return len(mentions[i].Name) > len(mentions[j].Name)
	})

	lidChat := session.chatSpeaksLID(chat)
	jids := make([]string, 0, len(mentions))
	for _, m := range mentions {
		if m.Address == "" {
			continue
		}
		jid := wireMention(session, m.Address, lidChat)
		jids = append(jids, jid.String())
		if m.Name != "" {
			text = strings.ReplaceAll(text, "@"+m.Name, "@"+jid.User)
		}
	}
	return text, jids
}

// wireMention is a canonical address in the namespace the chat speaks: the
// LID when the chat is lid-addressed and the mapping is known, the phone
// number otherwise (including the honest fallback — an unmapped peer keeps
// the form we have rather than inventing one).
func wireMention(session *Session, address string, lidChat bool) types.JID {
	pn := types.NewJID(address, types.DefaultUserServer)
	if !lidChat {
		return pn
	}
	lid, err := session.Client.Store.LIDs.GetLIDForPN(context.Background(), pn)
	if err != nil || lid.IsEmpty() {
		return pn
	}
	return lid.ToNonAD()
}

// chatSpeaksLID reports whether the chat addresses people by LID. Inbound
// traffic teaches this for free (Session.noteAddressingMode); for a group we
// have never received from, GetGroupInfo answers — a round trip paid only on
// a mention-bearing send to an unseen group.
func (s *Session) chatSpeaksLID(chat types.JID) bool {
	if mode := s.addressingMode(chat.String()); mode != "" {
		return mode == types.AddressingModeLID
	}
	if chat.Server == types.HiddenUserServer {
		return true
	}
	if chat.Server != types.GroupServer {
		return false
	}
	info, err := s.Client.GetGroupInfo(context.Background(), chat)
	if err != nil {
		return false
	}
	s.noteAddressingMode(chat.String(), info.AddressingMode)
	return info.AddressingMode == types.AddressingModeLID
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
		if strings.TrimSpace(content.Text) == "" {
			return nil, http.StatusUnprocessableEntity, fmt.Errorf("text message with empty text")
		}

		text := markdownToWhatsApp(content.Text)
		text, mentioned := encodeMentions(session, chat, content, text)
		ctx := replyContext(session, content)
		if ctx != nil || len(mentioned) > 0 {
			if ctx == nil {
				ctx = &waE2E.ContextInfo{}
			}
			ctx.MentionedJID = mentioned
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
		case "reaction":
			return buildReaction(session, chat, content)
		case "edit":
			return buildEdit(session, chat, content)
		case "revoke":
			return buildRevoke(session, chat, content)
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

// resolveMediaURL makes a media_url absolute. An absolute one is taken as it
// stands (open-bsp-api sends signed storage links); a relative one is resolved
// against the session's receiver, which is where this bridge already posts its
// webhooks — so a consumer serving its own media states its address once.
func resolveMediaURL(base, media string) (string, error) {
	ref, err := url.Parse(media)
	if err != nil {
		return "", fmt.Errorf("media_url: %w", err)
	}
	if ref.IsAbs() {
		return media, nil
	}
	b, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("receiver base: %w", err)
	}
	return b.ResolveReference(ref).String(), nil
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
	// captions carry mentions the same way text messages do: @Name → @digits inline,
	// the JIDs on the media message's ContextInfo
	captionText, captionMentioned := encodeMentions(
		session, chat, req.Record.Content, markdownToWhatsApp(caption),
	)
	captionPtr := optString(captionText)
	contextInfo := replyContext(session, req.Record.Content)
	if len(captionMentioned) > 0 {
		if contextInfo == nil {
			contextInfo = &waE2E.ContextInfo{}
		}
		contextInfo.MentionedJID = captionMentioned
	}

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

// buildReaction sends a cross-service ReactionPart (type data, data-only:
// {action, unicode}). The emoji sent to WhatsApp is the Unicode form; empty
// removes (the wire convention on this service).
func buildReaction(
	session *Session, chat types.JID, content MessageContent,
) (*waE2E.Message, int, error) {
	id, sender, ok := referencedKey(session, content)
	if !ok {
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("reaction without a valid re_message_id: %s", content.ReMessageID)
	}

	var data struct {
		Action  string `json:"action"`
		Unicode string `json:"unicode"`
	}
	if err := json.Unmarshal(content.Data, &data); err != nil {
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("invalid reaction data: %w", err)
	}

	emoji := data.Unicode
	if data.Action == "removed" {
		emoji = ""
	}

	return session.Client.BuildReaction(chat, sender, id, emoji), 0, nil
}

// buildEdit replaces the text of a message we sent: the referent by id, the new body
// through the same markdown and mention encoders a fresh send goes through, so an edit
// reads on the wire exactly like the message it replaces. WhatsApp only honours an edit
// within EditWindow (20 minutes) — past it the stanza is accepted and ignored, which is
// the one failure this path cannot report.
func buildEdit(
	session *Session, chat types.JID, content MessageContent,
) (*waE2E.Message, int, error) {
	id, _, ok := referencedKey(session, content)
	if !ok {
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("edit without a valid re_message_id: %s", content.ReMessageID)
	}
	if strings.TrimSpace(content.Text) == "" {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("edit with empty text")
	}

	text := markdownToWhatsApp(content.Text)
	text, mentioned := encodeMentions(session, chat, content, text)
	newContent := &waE2E.Message{Conversation: proto.String(text)}
	if len(mentioned) > 0 {
		newContent = &waE2E.Message{
			ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text:        proto.String(text),
				ContextInfo: &waE2E.ContextInfo{MentionedJID: mentioned},
			},
		}
	}
	return session.Client.BuildEdit(chat, id, newContent), 0, nil
}

// buildRevoke takes a message back for everyone. referencedKey yields our own JID for a
// message we sent — which is what whatsmeow wants for the ordinary case; a group admin
// revoking someone else's passes that participant, and the server decides.
func buildRevoke(
	session *Session, chat types.JID, content MessageContent,
) (*waE2E.Message, int, error) {
	id, sender, ok := referencedKey(session, content)
	if !ok {
		return nil, http.StatusUnprocessableEntity,
			fmt.Errorf("revoke without a valid re_message_id: %s", content.ReMessageID)
	}
	return session.Client.BuildRevoke(chat, sender, id), 0, nil
}

func dispatchChatJID(req dispatchRequest) (types.JID, error) {
	// conversation_address is the chat: a full JID for groups (contains "@"),
	// the peer's bare number for direct chats.
	if addr := req.Record.ConversationAddress; addr != "" {
		if strings.Contains(addr, "@") {
			return types.ParseJID(addr)
		}
		return types.NewJID(addr, types.DefaultUserServer), nil
	}
	return types.JID{}, fmt.Errorf("record has no conversation_address")
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OrganizationID string `json:"organization_id"`
		PhoneNumber    string `json:"phone_number"`
		// Optional: pairs a personal session for this member instead of the
		// org's shared inbox. Authorization happened in the management
		// function; the bridge just carries it through to the mapping and
		// the connected event.
		AgentID string `json:"agent_id"`
		// Optional: where THIS session delivers (webhook, media, lifecycle),
		// kept with its mapping. A consumer that runs its own receiver per
		// tenant names it here; one receiver for the whole bridge is
		// OPENBSP_URL, the fallback.
		WebhookURL string `json:"webhook_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.OrganizationID == "" {
		http.Error(w, "organization_id is required", http.StatusBadRequest)
		return
	}
	if req.WebhookURL == "" && s.cfg.OpenBSPURL == "" {
		http.Error(w, "webhook_url is required — this bridge has no OPENBSP_URL to fall back to", http.StatusBadRequest)
		return
	}
	if req.WebhookURL != "" {
		if u, err := url.Parse(req.WebhookURL); err != nil || !u.IsAbs() {
			http.Error(w, "webhook_url must be an absolute http(s) URL", http.StatusBadRequest)
			return
		}
	}

	result, err := s.manager.CreateSession(
		r.Context(), req.OrganizationID, req.PhoneNumber, req.AgentID, req.WebhookURL,
	)
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
