// open-bsp-whatsmeow: self-hosted WhatsApp Web bridge for OpenBSP
// ('whatsapp-web' service). A thin, stateless wrapper around
// go.mau.fi/whatsmeow that adapts to OpenBSP's native connector contract:
// inbound events → whatsapp-web-webhook, outbound via /dispatch called by
// whatsapp-web-dispatcher, lifecycle via whatsapp-web-management.
package main

import (
	"context"
	"os"

	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// The name the phone lists under Linked devices — whatsmeow's default is its
// own. It rides DeviceProps in the registration payload, so it is fixed at
// PAIRING time: changing it renames nothing that is already linked, only what
// pairs next. DEVICE_NAME overrides it for a deployment that wants its own
// branding on the phone.
func init() {
	store.SetOSInfo(envOr("DEVICE_NAME", "OpenBSP"), [3]uint32{0, 1, 0})
}

// askHistory sets the history a pairing asks the phone for: a full sync,
// reaching back this many days. It rides DeviceProps beside the device name,
// so it too is fixed at pairing time.
func askHistory(days uint32) {
	store.DeviceProps.RequireFullSync = proto.Bool(true)
	store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(days)
}

func main() {
	log := waLog.Stdout("bridge", envOr("LOG_LEVEL", "INFO"), true)

	cfg, err := ConfigFromEnv()
	if err != nil {
		log.Errorf("Config: %v", err)
		os.Exit(1)
	}
	askHistory(cfg.HistoryDays)

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
