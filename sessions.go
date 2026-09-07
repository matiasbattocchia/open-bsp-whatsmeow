package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Session is one paired WhatsApp account: a whatsmeow client plus its
// OpenBSP linkage. Address is the canonical bare digits of the session's own
// number (organizations_addresses.address for the 'whatsapp-web' service).
type Session struct {
	Client         *whatsmeow.Client
	OrganizationID string
	Address        string
	// Empty for the org's shared inbox; set, the member whose personal
	// session this is (organizations_addresses.agent_id).
	AgentID string
	// Set while the session is pairing; used to surface QR rotation and
	// completion to the polling endpoint.
	Pending *PendingSession

	// Groups whose metadata (subject → conversation name) was already sent
	// this process lifetime; re-sending after a restart is harmless.
	groupsMu   sync.Mutex
	groupsSent map[string]struct{}

	// A group's subject, kept once GetGroupInfo answers, so every later
	// message can carry conversation_name without another round-trip.
	namesMu    sync.Mutex
	groupNames map[string]string

	// A group's addressing mode (phone-number or LID), learned from inbound
	// traffic and from GetGroupInfo, and kept for the process lifetime: it
	// decides which namespace an outbound mention must speak.
	addrMu   sync.Mutex
	addrMode map[string]types.AddressingMode
}

// noteAddressingMode records what namespace a chat addresses people in.
func (s *Session) noteAddressingMode(chat string, mode types.AddressingMode) {
	if chat == "" || mode == "" {
		return
	}
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	if s.addrMode == nil {
		s.addrMode = make(map[string]types.AddressingMode)
	}
	s.addrMode[chat] = mode
}

// addressingMode reports the chat's known mode; "" when never observed.
func (s *Session) addressingMode(chat string) types.AddressingMode {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.addrMode[chat]
}

// markGroupSent reports whether the group still needed its metadata sent and
// atomically marks it as sent.
func (s *Session) markGroupSent(address string) bool {
	s.groupsMu.Lock()
	defer s.groupsMu.Unlock()
	if s.groupsSent == nil {
		s.groupsSent = make(map[string]struct{})
	}
	if _, seen := s.groupsSent[address]; seen {
		return false
	}
	s.groupsSent[address] = struct{}{}
	return true
}

// noteGroupName records a group's subject; groupName reports it, "" when this
// process has never fetched it.
func (s *Session) noteGroupName(address, name string) {
	if address == "" || name == "" {
		return
	}
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	if s.groupNames == nil {
		s.groupNames = make(map[string]string)
	}
	s.groupNames[address] = name
}

func (s *Session) groupName(address string) string {
	s.namesMu.Lock()
	defer s.namesMu.Unlock()
	return s.groupNames[address]
}

// Manager owns all sessions of this bridge instance. One replica by design:
// a WhatsApp session is a single WebSocket.
type Manager struct {
	store   *Store
	openbsp *OpenBSP
	log     waLog.Logger

	mu       sync.RWMutex
	sessions map[string]*Session        // by Address
	pending  map[string]*PendingSession // by pairing session id
}

func NewManager(st *Store, openbsp *OpenBSP, log waLog.Logger) *Manager {
	return &Manager{
		store:    st,
		openbsp:  openbsp,
		log:      log,
		sessions: make(map[string]*Session),
		pending:  make(map[string]*PendingSession),
	}
}

func randomID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// Start connects every device already present in the session store.
func (m *Manager) Start(ctx context.Context) error {
	devices, err := m.store.Container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("load devices: %w", err)
	}

	for _, device := range devices {
		if device.ID == nil {
			continue
		}

		mapping, err := m.store.GetMapping(ctx, device.ID.String())
		if err != nil {
			return fmt.Errorf("load mapping for %s: %w", device.ID, err)
		}
		if mapping == nil {
			m.log.Warnf("Device %s has no OpenBSP mapping; skipping", device.ID)
			continue
		}

		session := m.register(device, mapping.OrganizationID, mapping.Address, mapping.AgentID)
		if err := session.Client.Connect(); err != nil {
			m.log.Errorf("Connect %s failed: %v", mapping.Address, err)
		}
	}

	return nil
}

func (m *Manager) register(device *store.Device, organizationID, address, agentID string) *Session {
	client := whatsmeow.NewClient(device, m.log.Sub("client/"+address))
	session := &Session{
		Client:         client,
		OrganizationID: organizationID,
		Address:        address,
		AgentID:        agentID,
	}
	client.AddEventHandler(func(evt any) { m.handleEvent(session, evt) })

	m.mu.Lock()
	m.sessions[address] = session
	m.mu.Unlock()

	return session
}

func (m *Manager) Get(address string) *Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[address]
}

// PendingSession tracks an in-progress pairing so the UI can poll for QR
// rotation (codes expire every ~20s) and completion.
type PendingSession struct {
	ID      string
	Session *Session

	mu          sync.Mutex
	qrCode      string
	pairingCode string
	status      string // pending | paired | error
	errMessage  string
	createdAt   time.Time
}

const pendingTTL = 10 * time.Minute

// PairingState is the poll response relayed by whatsapp-web-management.
type PairingState struct {
	SessionID   string `json:"session_id"`
	Status      string `json:"status"` // pending | paired | error
	QRCode      string `json:"qr_code,omitempty"`
	PairingCode string `json:"pairing_code,omitempty"`
	// Set once paired: the organizations_addresses.address of the new session.
	Address string `json:"address,omitempty"`
	Error   string `json:"error,omitempty"`
}

func (p *PendingSession) state() *PairingState {
	p.mu.Lock()
	defer p.mu.Unlock()

	status := p.status
	if status == "pending" && time.Since(p.createdAt) > pendingTTL {
		status = "error"
		if p.errMessage == "" {
			p.errMessage = "pairing timed out"
		}
	}

	return &PairingState{
		SessionID:   p.ID,
		Status:      status,
		QRCode:      p.qrCode,
		PairingCode: p.pairingCode,
		Address:     p.Session.Address,
		Error:       p.errMessage,
	}
}

// CreateSession starts pairing a new device and returns the initial pairing
// state: a QR code string (rotated codes are picked up via PendingState
// polling) or, when phoneNumber is given, a phone pairing code. Pairing
// completes asynchronously: on PairSuccess the event handler saves the
// mapping, notifies whatsapp-web-management, and flips the pending status.
func (m *Manager) CreateSession(ctx context.Context, organizationID, phoneNumber, agentID string) (*PairingState, error) {
	device := m.store.Container.NewDevice()
	session := &Session{
		Client:         whatsmeow.NewClient(device, m.log.Sub("client/pairing")),
		OrganizationID: organizationID,
		AgentID:        agentID,
	}

	pending := &PendingSession{
		ID:        randomID(),
		Session:   session,
		status:    "pending",
		createdAt: time.Now(),
	}
	session.Pending = pending
	session.Client.AddEventHandler(func(evt any) { m.handleEvent(session, evt) })

	m.mu.Lock()
	// Opportunistically drop long-expired pairings so the map doesn't grow.
	for id, p := range m.pending {
		if time.Since(p.createdAt) > 3*pendingTTL {
			delete(m.pending, id)
		}
	}
	m.pending[pending.ID] = pending
	m.mu.Unlock()

	// The QR channel belongs to the QR flow ONLY. Phone pairing has its own
	// out-of-band code, and the channel would still run its QR clock beside
	// it — six rotations, then a "timeout" event that says nothing about the
	// code the person is typing. Opening it there killed the pairing after
	// ~2 minutes.
	if phoneNumber == "" {
		// Use a background context for the QR channel: it must outlive the
		// HTTP request that started the pairing.
		qrChan, err := session.Client.GetQRChannel(context.Background())
		if err != nil {
			return nil, fmt.Errorf("qr channel: %w", err)
		}

		// Consume the QR channel for the whole pairing window, keeping the
		// latest code available to the polling endpoint.
		go func() {
			for item := range qrChan {
				switch item.Event {
				case "code":
					pending.mu.Lock()
					pending.qrCode = item.Code
					pending.mu.Unlock()
				case whatsmeow.QRChannelSuccess.Event:
					// completePairing (via the PairSuccess event) flips the
					// status; nothing to do here.
				default: // timeout, error, multidevice-not-enabled, ...
					pending.mu.Lock()
					if pending.status == "pending" {
						pending.status = "error"
						pending.errMessage = item.Event
						if item.Error != nil {
							pending.errMessage = item.Error.Error()
						}
					}
					pending.mu.Unlock()
				}
			}
		}()
	}

	if err := session.Client.Connect(); err != nil {
		return nil, fmt.Errorf("connect for pairing: %w", err)
	}

	if phoneNumber != "" {
		code, err := session.Client.PairPhone(
			ctx, phoneNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)",
		)
		if err != nil {
			session.Client.Disconnect()
			return nil, fmt.Errorf("pair phone: %w", err)
		}
		pending.mu.Lock()
		pending.pairingCode = code
		pending.mu.Unlock()
		return pending.state(), nil
	}

	// Wait for the first QR code so the UI has something to render right
	// away; rotations arrive via polling.
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-deadline:
			session.Client.Disconnect()
			return nil, fmt.Errorf("timed out waiting for QR code")
		case <-time.After(200 * time.Millisecond):
			state := pending.state()
			if state.QRCode != "" || state.Status == "error" {
				if state.Status == "error" {
					session.Client.Disconnect()
					return nil, fmt.Errorf("pairing failed: %s", state.Error)
				}
				return state, nil
			}
		}
	}
}

// PendingState returns the current pairing state for polling, or nil for an
// unknown id.
func (m *Manager) PendingState(id string) *PairingState {
	m.mu.RLock()
	pending := m.pending[id]
	m.mu.RUnlock()

	if pending == nil {
		return nil
	}
	return pending.state()
}

// failPending ends an in-flight pairing — a session that never reached an
// address — and drops its client. Sessions that already paired are left
// alone: their disconnects are whatsmeow's ordinary reconnect churn, and
// re-pairing is not what a flaky network calls for. Disconnect runs off the
// event goroutine that delivered the event, which is the one holding the
// socket.
func (m *Manager) failPending(session *Session, reason string) {
	if session.Address != "" || session.Pending == nil {
		return
	}

	session.Pending.mu.Lock()
	stale := session.Pending.status != "pending"
	if !stale {
		session.Pending.status = "error"
		session.Pending.errMessage = reason
	}
	session.Pending.mu.Unlock()
	if stale {
		return
	}

	m.log.Warnf("Pairing %s failed: %s", session.Pending.ID, reason)
	go session.Client.Disconnect()
}

// completePairing is called from the event handler on PairSuccess/Connected
// of a session that has no address yet.
func (m *Manager) completePairing(session *Session, ownJID types.JID) {
	session.Address = ownJID.User

	m.mu.Lock()
	m.sessions[session.Address] = session
	m.mu.Unlock()

	if session.Pending != nil {
		session.Pending.mu.Lock()
		session.Pending.status = "paired"
		session.Pending.mu.Unlock()
	}

	ctx := context.Background()

	if err := m.store.SaveMapping(ctx, SessionMapping{
		DeviceJID:      ownJID.String(),
		OrganizationID: session.OrganizationID,
		Address:        session.Address,
		AgentID:        session.AgentID,
	}); err != nil {
		m.log.Errorf("Save mapping for %s failed: %v", ownJID, err)
	}

	if err := m.openbsp.PostSessionEvent(SessionEvent{
		Event:          "connected",
		OrganizationID: session.OrganizationID,
		Address:        session.Address,
		AgentID:        session.AgentID,
		Extra:          map[string]any{"device_jid": ownJID.String()},
	}); err != nil {
		m.log.Errorf("Notify connected for %s failed: %v", ownJID, err)
	}
}

// postLinkState reports a paired session's socket coming or going, when the
// receiver takes such events (OpenBSP.LinkEvents).
func (m *Manager) postLinkState(session *Session, state string) {
	if !m.openbsp.LinkEvents {
		return
	}
	m.log.Infof("Session %s %s", session.Address, state)
	if err := m.openbsp.PostSessionEvent(SessionEvent{
		Event:          state,
		OrganizationID: session.OrganizationID,
		Address:        session.Address,
		AgentID:        session.AgentID,
	}); err != nil {
		m.log.Errorf("Notify %s for %s failed: %v", state, session.Address, err)
	}
}

// SessionStatus is what whatsapp-web-management's GET route relays to the UI.
type SessionStatus struct {
	Address   string `json:"address"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
}

func (m *Manager) Status(address string) *SessionStatus {
	session := m.Get(address)
	if session == nil {
		return nil
	}
	return &SessionStatus{
		Address:   address,
		Connected: session.Client.IsConnected(),
		LoggedIn:  session.Client.IsLoggedIn(),
	}
}

// Logout unpairs the device and removes it from the session store.
func (m *Manager) Logout(ctx context.Context, address string) error {
	session := m.Get(address)
	if session == nil {
		return fmt.Errorf("unknown session %s", address)
	}

	deviceJID := ""
	if session.Client.Store.ID != nil {
		deviceJID = session.Client.Store.ID.String()
	}

	if err := session.Client.Logout(ctx); err != nil {
		return fmt.Errorf("logout: %w", err)
	}

	if deviceJID != "" {
		if err := m.store.DeleteMapping(ctx, deviceJID); err != nil {
			return fmt.Errorf("delete mapping: %w", err)
		}
	}

	m.mu.Lock()
	delete(m.sessions, address)
	m.mu.Unlock()

	return nil
}
