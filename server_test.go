package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/socket"
)

// Only what clears on its own comes back 503 for the dispatch cron to retry; a
// failure the same request would meet again is 422, so the row is marked failed
// instead of being re-sent for 12 hours.
func TestSendErrorStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{whatsmeow.ErrNotConnected, http.StatusServiceUnavailable},
		{fmt.Errorf("failed to get device list: %w", whatsmeow.ErrIQTimedOut), http.StatusServiceUnavailable},
		{whatsmeow.ErrMessageTimedOut, http.StatusServiceUnavailable},
		{socket.ErrSocketClosed, http.StatusServiceUnavailable},
		{context.DeadlineExceeded, http.StatusServiceUnavailable},
		{&whatsmeow.DisconnectedError{Action: "info query"}, http.StatusServiceUnavailable},
		{whatsmeow.ErrIQRateOverLimit, http.StatusServiceUnavailable},
		{&whatsmeow.IQError{Code: 500, Text: "internal-server-error"}, http.StatusServiceUnavailable},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, http.StatusServiceUnavailable},
		{fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError), http.StatusUnprocessableEntity},
		{whatsmeow.ErrUnknownServer, http.StatusUnprocessableEntity},
		{whatsmeow.ErrIQBadRequest, http.StatusUnprocessableEntity},
		{errors.New("something whatsmeow did not name"), http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		if got := sendErrorStatus(c.err); got != c.want {
			t.Errorf("%v: got %d, want %d", c.err, got, c.want)
		}
	}
}

// A consumer that serves its own media has no hostname to hand us: it sends a
// relative media_url and we resolve it against the address we already deliver
// to. An absolute one (open-bsp-api's signed storage links) is left alone.
func TestResolveMediaURL(t *testing.T) {
	cases := []struct {
		name  string
		base  string
		media string
		want  string
	}{
		{
			name:  "relative resolves against an origin base",
			base:  "http://localhost:8793",
			media: "/m/eyJ9.mac",
			want:  "http://localhost:8793/m/eyJ9.mac",
		},
		{
			name:  "relative ignores the base's path — it is an origin-absolute ref",
			base:  "http://kong:8000/functions/v1",
			media: "/m/eyJ9.mac",
			want:  "http://kong:8000/m/eyJ9.mac",
		},
		{
			name:  "absolute passes through untouched",
			base:  "http://localhost:8793",
			media: "https://project.supabase.co/storage/v1/object/sign/media/x?token=y",
			want:  "https://project.supabase.co/storage/v1/object/sign/media/x?token=y",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveMediaURL(c.base, c.media)
			if err != nil {
				t.Fatalf("resolveMediaURL: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}

	if _, err := resolveMediaURL("://nonsense", "/m/x"); err == nil {
		t.Fatal("a malformed base should be reported, not dialed")
	}
}
