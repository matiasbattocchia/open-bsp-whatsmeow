package main

import (
	"context"
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
}

// Manager owns all sessions of this bridge instance. One replica by design:
// a WhatsApp session is a single WebSocket.
type Manager struct {
	store   *Store
	openbsp *OpenBSP
	log     waLog.Logger

	mu       sync.RWMutex
	sessions map[string]*Session // by Address
}

func NewManager(st *Store, openbsp *OpenBSP, log waLog.Logger) *Manager {
	return &Manager{
		store:    st,
		openbsp:  openbsp,
		log:      log,
		sessions: make(map[string]*Session),
	}
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

		session := m.register(device, mapping.OrganizationID, mapping.Address)
		if err := session.Client.Connect(); err != nil {
			m.log.Errorf("Connect %s failed: %v", mapping.Address, err)
		}
	}

	return nil
}

func (m *Manager) register(device *store.Device, organizationID, address string) *Session {
	client := whatsmeow.NewClient(device, m.log.Sub("client/"+address))
	session := &Session{
		Client:         client,
		OrganizationID: organizationID,
		Address:        address,
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

// PairingResult is returned to whatsapp-web-management for the UI to render.
type PairingResult struct {
	QRCode      string `json:"qr_code,omitempty"`
	PairingCode string `json:"pairing_code,omitempty"`
}

// CreateSession starts pairing a new device. It returns a QR code string
// (or, when phoneNumber is given, a pairing code) and completes
// asynchronously: on PairSuccess the event handler saves the mapping and
// notifies whatsapp-web-management. The pending session keeps its
// organization but has no address until pairing succeeds.
func (m *Manager) CreateSession(ctx context.Context, organizationID, phoneNumber string) (*PairingResult, error) {
	device := m.store.Container.NewDevice()
	session := &Session{
		Client:         whatsmeow.NewClient(device, m.log.Sub("client/pairing")),
		OrganizationID: organizationID,
	}
	session.Client.AddEventHandler(func(evt any) { m.handleEvent(session, evt) })

	qrChan, err := session.Client.GetQRChannel(ctx)
	if err != nil {
		return nil, fmt.Errorf("qr channel: %w", err)
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
		// Drain the QR channel in the background so pairing can complete.
		go func() {
			for range qrChan {
				// PairSuccess is handled by the event handler.
			}
		}()
		return &PairingResult{PairingCode: code}, nil
	}

	select {
	case item := <-qrChan:
		if item.Event != "code" {
			session.Client.Disconnect()
			return nil, fmt.Errorf("pairing failed: %s %v", item.Event, item.Error)
		}
		go func() {
			for range qrChan {
				// Subsequent codes/success are handled by the event handler;
				// TODO: expose code rotation to the UI (poll or SSE).
			}
		}()
		return &PairingResult{QRCode: item.Code}, nil
	case <-time.After(15 * time.Second):
		session.Client.Disconnect()
		return nil, fmt.Errorf("timed out waiting for QR code")
	}
}

// completePairing is called from the event handler on PairSuccess/Connected
// of a session that has no address yet.
func (m *Manager) completePairing(session *Session, ownJID types.JID) {
	session.Address = ownJID.User

	m.mu.Lock()
	m.sessions[session.Address] = session
	m.mu.Unlock()

	ctx := context.Background()

	if err := m.store.SaveMapping(ctx, SessionMapping{
		DeviceJID:      ownJID.String(),
		OrganizationID: session.OrganizationID,
		Address:        session.Address,
	}); err != nil {
		m.log.Errorf("Save mapping for %s failed: %v", ownJID, err)
	}

	if err := m.openbsp.PostSessionEvent(SessionEvent{
		Event:          "connected",
		OrganizationID: session.OrganizationID,
		Address:        session.Address,
		Extra:          map[string]any{"device_jid": ownJID.String()},
	}); err != nil {
		m.log.Errorf("Notify connected for %s failed: %v", ownJID, err)
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
