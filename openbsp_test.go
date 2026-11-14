package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A group's facts are fields of their own beside the ones a consumer already
// reads, and a chat that has none leaves them out.
func TestGroupFactsRideAsFieldsOfTheirOwn(t *testing.T) {
	message, _ := json.Marshal(WebhookMessage{
		ConversationAddress:   "120363001234567890@g.us",
		ConversationMembers:   312,
		ConversationAdmin:     true,
		ConversationCommunity: "Obra",
	})
	for _, field := range []string{
		`"conversation_members":312`, `"conversation_admin":true`, `"conversation_community":"Obra"`,
	} {
		if !strings.Contains(string(message), field) {
			t.Errorf("message lacks %s: %s", field, message)
		}
	}
	room, _ := json.Marshal(WebhookRoom{Address: "120363001234567890@g.us", Members: 40, Community: "Obra"})
	if !strings.Contains(string(room), `"members":40`) || !strings.Contains(string(room), `"community":"Obra"`) ||
		strings.Contains(string(room), `"admin"`) {
		t.Errorf("room: %s", room)
	}
	direct, _ := json.Marshal(WebhookMessage{ConversationAddress: "5491100000001"})
	if strings.Contains(string(direct), "conversation_members") || strings.Contains(string(direct), "conversation_community") {
		t.Errorf("a direct chat carries no group facts: %s", direct)
	}
}

// An upload's file part says what the medium is, so the receiver files a voice
// note as audio rather than as nameless bytes; one that says nothing is bytes.
func TestUploadMediaLabelsThePartWithItsType(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(file)
		got = append(got, header.Header.Get("Content-Type")+" "+header.Filename+" "+string(data))
		w.Write([]byte(`{"uri":"internal://media/x"}`))
	}))
	defer srv.Close()
	o := NewOpenBSP(&Config{OpenBSPURL: srv.URL, BridgeToken: "t"})

	if _, err := o.UploadMedia("549", "", "audio/ogg; codecs=opus", []byte("OggS")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.UploadMedia("549", `a "b".pdf`, "", []byte("%PDF")); err != nil {
		t.Fatal(err)
	}
	want := []string{"audio/ogg; codecs=opus file OggS", `application/octet-stream a "b".pdf %PDF`}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parts = %q, want %q", got, want)
	}
}

// The upload deadline doubles as how long one medium may hold up a session's
// event stream, so it has to grow with the payload and then stop growing.
func TestMediaUploadTimeoutScalesWithSizeAndIsBounded(t *testing.T) {
	const mb = 1000 * 1000

	cases := []struct {
		name string
		size int
		want time.Duration
	}{
		{"an empty body still waits out a cold start", 0, 30 * time.Second},
		{"a 1 MB image", 1 * mb, 40 * time.Second},
		{"a 5 MB video", 5 * mb, 80 * time.Second},
		{"the largest upload the webhook accepts", 50 * mb, 5 * time.Minute},
	}
	for _, c := range cases {
		if got := mediaUploadTimeout(c.size); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}

	// Monotonic: a bigger upload never gets less time than a smaller one.
	prev := time.Duration(0)
	for size := 0; size <= 60*mb; size += mb {
		got := mediaUploadTimeout(size)
		if got < prev {
			t.Fatalf("timeout shrank at %d MB: %v after %v", size/mb, got, prev)
		}
		prev = got
	}
}

// A session delivers where its pairing said; one that said nothing delivers
// where the bridge does. The derived client is the same client but for the
// address — the token and the pools are not copied into a second life.
func TestAtAimsOneClientPerReceiver(t *testing.T) {
	shared := NewOpenBSP(&Config{OpenBSPURL: "http://kong:8000/functions/v1", BridgeToken: "t"})

	if got := shared.at(""); got != shared {
		t.Fatal("a session naming no receiver must post through the bridge-wide client itself")
	}

	own := shared.at("http://localhost:8794")
	if own.Base() != "http://localhost:8794" {
		t.Fatalf("Base = %q, want the session's own receiver", own.Base())
	}
	if shared.Base() != "http://kong:8000/functions/v1" {
		t.Fatal("aiming a copy must not move the bridge-wide client")
	}
	if own.token != shared.token || own.http != shared.http || own.mediaHTTP != shared.mediaHTTP {
		t.Fatal("the aimed client must share the token and the HTTP clients")
	}

	// A bridge with no OPENBSP_URL at all: every session brings its own.
	none := NewOpenBSP(&Config{BridgeToken: "t"})
	if none.Base() != "" || none.at("http://localhost:8793").Base() != "http://localhost:8793" {
		t.Fatal("an unset OPENBSP_URL is an empty base until a session names one")
	}
}
