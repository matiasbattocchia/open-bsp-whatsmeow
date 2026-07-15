package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const schemaName = "whatsmeow"

// Store wraps the whatsmeow session container plus the bridge's own
// session-mapping table (device JID ↔ OpenBSP organization/address). Both
// live in the lent `whatsmeow` schema; OpenBSP never reads it.
type Store struct {
	DB        *sql.DB
	Container *sqlstore.Container
}

func OpenStore(ctx context.Context, databaseURL string, log waLog.Logger) (*Store, error) {
	dsn := databaseURL
	if !strings.Contains(dsn, "search_path") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "search_path=" + schemaName
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// One small bounded pool for everything; never connect-per-message.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(30 * time.Minute)

	if _, err := db.ExecContext(ctx, "create schema if not exists "+schemaName); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}

	container := sqlstore.NewWithDB(db, "postgres", log)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		create table if not exists bridge_sessions (
			device_jid      text primary key,
			organization_id text not null,
			address         text not null
		)`); err != nil {
		return nil, fmt.Errorf("create bridge_sessions: %w", err)
	}

	return &Store{DB: db, Container: container}, nil
}

// SessionMapping links a whatsmeow device to the OpenBSP organization and
// organizations_addresses.address ('whatsapp-web' service, canonical bare
// digits) it serves.
type SessionMapping struct {
	DeviceJID      string
	OrganizationID string
	Address        string
}

func (s *Store) SaveMapping(ctx context.Context, m SessionMapping) error {
	_, err := s.DB.ExecContext(ctx, `
		insert into bridge_sessions (device_jid, organization_id, address)
		values ($1, $2, $3)
		on conflict (device_jid) do update
		set organization_id = excluded.organization_id, address = excluded.address`,
		m.DeviceJID, m.OrganizationID, m.Address)
	return err
}

func (s *Store) GetMapping(ctx context.Context, deviceJID string) (*SessionMapping, error) {
	m := SessionMapping{DeviceJID: deviceJID}
	err := s.DB.QueryRowContext(ctx, `
		select organization_id, address from bridge_sessions where device_jid = $1`,
		deviceJID).Scan(&m.OrganizationID, &m.Address)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) DeleteMapping(ctx context.Context, deviceJID string) error {
	_, err := s.DB.ExecContext(ctx,
		`delete from bridge_sessions where device_jid = $1`, deviceJID)
	return err
}
