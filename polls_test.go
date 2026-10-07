package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// A poll reads the same whichever field WhatsApp put it in: question, options in
// the author's order, and how many of them one voter may pick.
func TestPollReadsAsADataPart(t *testing.T) {
	poll := &waE2E.PollCreationMessage{
		Name: proto.String("¿Cuándo nos juntamos?"),
		Options: []*waE2E.PollCreationMessage_Option{
			{OptionName: proto.String("Sábado")},
			{OptionName: proto.String("Domingo")},
		},
		SelectableOptionsCount: proto.Uint32(1),
	}
	carriers := map[string]*waE2E.Message{
		"v1": {PollCreationMessage: poll},
		"v2": {PollCreationMessageV2: poll},
		"v3": {PollCreationMessageV3: poll},
		"v5": {PollCreationMessageV5: poll},
		"v6": {PollCreationMessageV6: poll},
	}

	m := &Manager{}
	for name, message := range carriers {
		content, mediaErr := m.buildContent(&Session{}, &events.Message{Message: message}, true)
		if mediaErr != nil || content == nil {
			t.Fatalf("%s: content %v, err %v", name, content, mediaErr)
		}
		if content.Type != "data" || content.Kind != "poll" {
			t.Fatalf("%s: got %s/%s, want data/poll", name, content.Type, content.Kind)
		}
		var data PollData
		if err := json.Unmarshal(content.Data, &data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := PollData{
			Question:        "¿Cuándo nos juntamos?",
			Options:         []string{"Sábado", "Domingo"},
			SelectableCount: 1,
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("%s: got %+v, want %+v", name, data, want)
		}
	}
}

// A poll sent as a reply names the message it answers, like any other reply.
func TestPollReplyIsQuoted(t *testing.T) {
	message := &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{
		Name:        proto.String("?"),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("ABC"), Participant: proto.String("1@s.whatsapp.net")},
	}}
	if stanza, participant := quotedRef(message); stanza != "ABC" || participant != "1@s.whatsapp.net" {
		t.Fatalf("quotedRef = %q, %q", stanza, participant)
	}
}
