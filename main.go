// open-bsp-whatsmeow: self-hosted WhatsApp Web bridge for OpenBSP
// ('whatsapp-web' service). A thin, stateless wrapper around
// go.mau.fi/whatsmeow that adapts to OpenBSP's native connector contract:
// inbound events → whatsapp-web-webhook, outbound via /dispatch called by
// whatsapp-web-dispatcher, lifecycle via whatsapp-web-management.
package main

import (
	"context"
	"os"

	waLog "go.mau.fi/whatsmeow/util/log"
)

func main() {
	log := waLog.Stdout("bridge", envOr("LOG_LEVEL", "INFO"), true)

	cfg, err := ConfigFromEnv()
	if err != nil {
		log.Errorf("Config: %v", err)
		os.Exit(1)
	}

	ctx := context.Background()

	st, err := OpenStore(ctx, cfg.DatabaseURL, log.Sub("store"))
	if err != nil {
		log.Errorf("Store: %v", err)
		os.Exit(1)
	}

	manager := NewManager(st, NewOpenBSP(cfg), log)
	if err := manager.Start(ctx); err != nil {
		log.Errorf("Sessions: %v", err)
		os.Exit(1)
	}

	server := NewServer(cfg, manager, log.Sub("http"))
	if err := server.ListenAndServe(); err != nil {
		log.Errorf("HTTP server: %v", err)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
