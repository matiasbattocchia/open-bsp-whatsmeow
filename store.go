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
	_ "modernc.org/sqlite"
)

const schemaName = "whatsmeow"

// Store wraps the whatsmeow session container plus the bridge's own
// session-mapping table (device JID ↔ OpenBSP organization/address). On
// Postgres both live in the lent `whatsmeow` schema; OpenBSP never reads it.
// On SQLite the file IS the schema — one bridge, one file.
type Store struct {
	DB        *sql.DB
	Container *sqlstore.Container
}

// isSQLite reports whether the DSN asks for the embedded engine. Explicit
// schemes only — a bare path is ambiguous and stays an error, so a typo'd
// Postgres DSN never silently becomes a private local database.
func isSQLite(dsn string) bool {
	for _, prefix := range []string{"file:", "sqlite:", "sqlite3:"} {
		if strings.HasPrefix(dsn, prefix) {
			return true
		}
	}
	return false
}

// OpenStore opens the durable store the bridge runs on. The ENGINE follows the
// DSN scheme: `postgres://…` (the deployed tier — one database for many
// services, the whatsmeow schema lent to us) or `file:…` / `sqlite:…` (the
// local/single-tenant tier — no server to run; the file rides the data volume,
// which is also where the Signal keys belong). Both are first-class in
// whatsmeow's sqlstore; only the plumbing below differs.
func OpenStore(ctx context.Context, databaseURL string, log waLog.Logger) (*Store, error) {
	if isSQLite(databaseURL) {
		return openSQLite(ctx, databaseURL, log)
	}
	return openPostgres(ctx, databaseURL, log)
}

func openPostgres(ctx context.Context, databaseURL string, log waLog.Logger) (*Store, error) {
	dsn := databaseURL
	appendParam := func(param string) {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + param
	}
	if !strings.Contains(dsn, "search_path") {
		appendParam("search_path=" + schemaName)
	}
	// Transaction-mode poolers (Supavisor/pgbouncer port 6543) route each
	// transaction to a different backend, which breaks pgx's default
	// prepared-statement cache ("prepared statement stmtcache_... does not
	// exist/already exists"). Simple protocol works everywhere; override in
	// the DSN if you are on a direct connection and want extended protocol.
	if !strings.Contains(dsn, "default_query_exec_mode") {
		appendParam("default_query_exec_mode=simple_protocol")
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

	return finishOpen(ctx, db, "postgres", log)
}

func openSQLite(ctx context.Context, databaseURL string, log waLog.Logger) (*Store, error) {
	// modernc.org/sqlite is PURE GO on purpose: the image builds with
	// CGO_ENABLED=0 into distroless/static, so a cgo driver (mattn) would
	// break the deployment, not just the build.
	dsn := strings.TrimPrefix(strings.TrimPrefix(databaseURL, "sqlite3:"), "sqlite:")
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	appendParam := func(param string) {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + param
	}
	// whatsmeow's schema is relational and relies on cascades; SQLite enforces
	// foreign keys only when asked, per connection.
	if !strings.Contains(dsn, "foreign_keys") {
		appendParam("_pragma=foreign_keys(1)")
	}
	// WAL lets the session's writes proceed while a dispatch call reads.
	if !strings.Contains(dsn, "journal_mode") {
		appendParam("_pragma=journal_mode(WAL)")
	}
	if !strings.Contains(dsn, "busy_timeout") {
		appendParam("_pragma=busy_timeout(5000)")
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite takes ONE writer. The Postgres pool above is a server assumption;
	// here concurrency belongs in the queue, not the connection count.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0) // a file handle does not go stale

	return finishOpen(ctx, db, "sqlite3", log)
}

// finishOpen runs the parts that are identical on both engines: the whatsmeow
// store migrations and the bridge's own mapping table (portable DDL — SQLite
// has supported UPSERT since 3.24).
func finishOpen(ctx context.Context, db *sql.DB, dialect string, log waLog.Logger) (*Store, error) {
	container := sqlstore.NewWithDB(db, dialect, log)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade whatsmeow store: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		create table if not exists bridge_sessions (
			device_jid      text primary key,
			organization_id text not null,
			address         text not null,
			agent_id        text not null default '',
			webhook_url     text not null default ''
		)`); err != nil {
		return nil, fmt.Errorf("create bridge_sessions: %w", err)
	}

	// Existing databases predate these columns; neither engine has a portable
	// IF NOT EXISTS for one, so the duplicate-column error is the no-op path.
	for _, column := range []string{
		"agent_id text not null default ''",
		"webhook_url text not null default ''",
	} {
		if _, err := db.ExecContext(ctx, `alter table bridge_sessions add column `+column); err == nil {
			log.Infof("Added bridge_sessions.%s", strings.Fields(column)[0])
		}
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
	// Empty means the address is the org's shared inbox; set, it names the
	// member whose personal session this is (organizations_addresses.agent_id).
	AgentID string
	// Where this session's traffic is delivered — the base the webhook, media
	// and session-event routes resolve against. Named by whoever asked for
	// the pairing (POST /sessions); empty means the bridge-wide OPENBSP_URL,
	// which is the only receiver open-bsp-api ever names.
	WebhookURL string
}

func (s *Store) SaveMapping(ctx context.Context, m SessionMapping) error {
	_, err := s.DB.ExecContext(ctx, `
		insert into bridge_sessions (device_jid, organization_id, address, agent_id, webhook_url)
		values ($1, $2, $3, $4, $5)
		on conflict (device_jid) do update
		set organization_id = excluded.organization_id, address = excluded.address,
		    agent_id = excluded.agent_id, webhook_url = excluded.webhook_url`,
		m.DeviceJID, m.OrganizationID, m.Address, m.AgentID, m.WebhookURL)
	return err
}

func (s *Store) GetMapping(ctx context.Context, deviceJID string) (*SessionMapping, error) {
	m := SessionMapping{DeviceJID: deviceJID}
	err := s.DB.QueryRowContext(ctx, `
		select organization_id, address, agent_id, webhook_url
		from bridge_sessions where device_jid = $1`,
		deviceJID).Scan(&m.OrganizationID, &m.Address, &m.AgentID, &m.WebhookURL)
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
