package main

import (
	"fmt"
	"os"
	"strconv"
)

// Config is read once from the environment; the bridge is stateless and
// disposable — all durable state lives in the database DATABASE_URL points at.
type Config struct {
	// Database DSN; the engine follows the scheme.
	//
	//	postgres://…  the whatsmeow session store and the bridge's own
	//	              session-mapping table live in the `whatsmeow` schema;
	//	              a search_path option is appended if the DSN has none.
	//	file:… │ sqlite:…
	//	              embedded SQLite, no server to run — the file holds both.
	//	              Put it on a persistent volume: it IS the session.
	DatabaseURL string
	// Base URL of the OpenBSP edge functions, e.g.
	// http://kong:8000/functions/v1
	OpenBSPURL string
	// Shared bearer token, used in both directions: the bridge authenticates
	// its posts to whatsapp-web-webhook / whatsapp-web-management with it,
	// and requires it on calls to its own HTTP endpoints.
	BridgeToken string
	// HTTP listen address for dispatcher/management calls.
	ListenAddr string
	// Report a paired session's link state to the management function:
	// `disconnected` when its socket drops, `connected` when whatsmeow has it
	// back (the pair brackets the outage). Off ⇒ only the pairing
	// `connected` and `logged_out` are posted, which is all OpenBSP's
	// management function accepts.
	LinkEvents bool
}

func ConfigFromEnv() (*Config, error) {
	cfg := &Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		OpenBSPURL:  os.Getenv("OPENBSP_URL"),
		BridgeToken: os.Getenv("BRIDGE_TOKEN"),
		ListenAddr:  os.Getenv("LISTEN_ADDR"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.OpenBSPURL == "" {
		return nil, fmt.Errorf("OPENBSP_URL is required")
	}
	if cfg.BridgeToken == "" {
		return nil, fmt.Errorf("BRIDGE_TOKEN is required")
	}
	if raw := os.Getenv("LINK_EVENTS"); raw != "" {
		on, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("LINK_EVENTS: %w", err)
		}
		cfg.LinkEvents = on
	}
	if cfg.ListenAddr == "" {
		// PaaS convention (Zeabur/Heroku/...): the platform announces the
		// port it routes traffic to via PORT.
		if port := os.Getenv("PORT"); port != "" {
			cfg.ListenAddr = ":" + port
		} else {
			// local default: loopback only — the bearer token authenticates callers,
			// it does not do LAN-perimeter duty. LISTEN_ADDR widens deliberately.
			cfg.ListenAddr = "127.0.0.1:8081"
		}
	}

	return cfg, nil
}
