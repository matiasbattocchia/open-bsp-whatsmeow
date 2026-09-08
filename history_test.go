package main

import (
	"errors"
	"testing"
	"time"

	waWeb "go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A history row that carries no file has to say so. The delivery status is
// what the phone acked; the error is why the timeline shows a placeholder,
// and the two have to coexist — an imported row is both read AND missing
// its media, and dropping either half is what made 59k rows look ordinary.
func TestHistoryStatusKeepsBothDeliveryAndMediaError(t *testing.T) {
	ts := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	incoming := &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{IsFromMe: false},
		Timestamp:     ts,
	}}
	webMsg := &waWeb.WebMessageInfo{}

	plain := historyStatus(webMsg, incoming, nil)
	if _, ok := plain["errors"]; ok {
		t.Fatalf("a row with its media stored carries no errors: %v", plain)
	}
	if plain["read"] != ts.Format(time.RFC3339) {
		t.Fatalf("incoming history is read: %v", plain)
	}

	withErr := historyStatus(webMsg, incoming, errors.New("boom"))
	if withErr["read"] != ts.Format(time.RFC3339) {
		t.Fatalf("the media error must not displace the delivery status: %v", withErr)
	}
	errs, ok := withErr["errors"].([]string)
	if !ok || len(errs) != 1 || errs[0] != "boom" {
		t.Fatalf("the reason reaches status.errors verbatim: %v", withErr)
	}
}
