package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func readInteractive(t *testing.T, message *waE2E.Message) (string, InteractiveData) {
	t.Helper()
	content, err := (&Manager{}).buildContent(&Session{}, &events.Message{Message: message}, false)
	if err != nil || content == nil {
		t.Fatalf("content %v, err %v", content, err)
	}
	if content.Type != "data" || content.Kind != "interactive" {
		t.Fatalf("got %s/%s, want data/interactive", content.Type, content.Kind)
	}
	var data InteractiveData
	if err := json.Unmarshal(content.Data, &data); err != nil {
		t.Fatal(err)
	}
	return content.Text, data
}

// Each shape a bot offers choices in reads as the body it wrote and the options
// a reply can press, links apart.
func TestInteractiveReadsAsADataPart(t *testing.T) {
	text, data := readInteractive(t, &waE2E.Message{ButtonsMessage: &waE2E.ButtonsMessage{
		ContentText: proto.String("Entonces… ¿Seguimos chateando?"),
		Buttons: []*waE2E.ButtonsMessage_Button{
			{ButtonID: proto.String("si"), ButtonText: &waE2E.ButtonsMessage_Button_ButtonText{DisplayText: proto.String("Sí")}},
			{ButtonID: proto.String("no"), ButtonText: &waE2E.ButtonsMessage_Button_ButtonText{DisplayText: proto.String("No")}},
		},
	}})
	if text != "Entonces… ¿Seguimos chateando?" {
		t.Fatalf("body %q", text)
	}
	want := InteractiveData{Form: "buttons", Options: []InteractiveOption{{ID: "si", Title: "Sí"}, {ID: "no", Title: "No"}}}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("buttons: got %+v", data)
	}

	_, data = readInteractive(t, &waE2E.Message{ListMessage: &waE2E.ListMessage{
		Description: proto.String("¿Qué querés hacer?"),
		ButtonText:  proto.String("Ver opciones"),
		Sections: []*waE2E.ListMessage_Section{{
			Title: proto.String("Préstamos"),
			Rows: []*waE2E.ListMessage_Row{
				{RowID: proto.String("hip"), Title: proto.String("Hipotecario"), Description: proto.String("UVA")},
			},
		}},
	}})
	want = InteractiveData{Form: "list", Button: "Ver opciones", Options: []InteractiveOption{
		{ID: "hip", Title: "Hipotecario", Description: "UVA", Section: "Préstamos"},
	}}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("list: got %+v", data)
	}

	text, data = readInteractive(t, &waE2E.Message{InteractiveMessage: &waE2E.InteractiveMessage{
		Body: &waE2E.InteractiveMessage_Body{Text: proto.String("Elegí")},
		InteractiveMessage: &waE2E.InteractiveMessage_NativeFlowMessage_{
			NativeFlowMessage: &waE2E.InteractiveMessage_NativeFlowMessage{
				Buttons: []*waE2E.InteractiveMessage_NativeFlowMessage_NativeFlowButton{
					{Name: proto.String("quick_reply"), ButtonParamsJSON: proto.String(`{"display_text":"Acepto","id":"tyc_ok"}`)},
					{Name: proto.String("cta_url"), ButtonParamsJSON: proto.String(`{"display_text":"Términos","url":"https://bh.example/tyc.pdf"}`)},
				},
			},
		},
	}})
	want = InteractiveData{
		Form:    "native_flow",
		Options: []InteractiveOption{{ID: "tyc_ok", Title: "Acepto", Flow: "quick_reply"}},
		Links:   []InteractiveLink{{Title: "Términos", URL: "https://bh.example/tyc.pdf"}},
	}
	if text != "Elegí" || !reflect.DeepEqual(data, want) {
		t.Fatalf("native flow: got %q %+v", text, data)
	}

	_, data = readInteractive(t, &waE2E.Message{TemplateMessage: &waE2E.TemplateMessage{
		HydratedTemplate: &waE2E.TemplateMessage_HydratedFourRowTemplate{
			HydratedContentText: proto.String("¿Aceptás?"),
			HydratedButtons: []*waE2E.HydratedTemplateButton{{
				Index: proto.Uint32(0),
				HydratedButton: &waE2E.HydratedTemplateButton_QuickReplyButton{
					QuickReplyButton: &waE2E.HydratedTemplateButton_HydratedQuickReplyButton{
						DisplayText: proto.String("Acepto"), ID: proto.String("ok"),
					},
				},
			}},
		},
	}})
	zero := uint32(0)
	want = InteractiveData{Form: "template", Options: []InteractiveOption{{ID: "ok", Title: "Acepto", Index: &zero}}}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("template: got %+v", data)
	}
}

// A person's own press, echoed back, reads as the option it names, replying to
// the message it answers.
func TestAPressReadsAsItsOption(t *testing.T) {
	content, err := (&Manager{}).buildContent(&Session{}, &events.Message{Message: &waE2E.Message{
		ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{
			SelectedButtonID: proto.String("si"),
			Response:         &waE2E.ButtonsResponseMessage_SelectedDisplayText{SelectedDisplayText: "Sí"},
		},
	}}, false)
	if err != nil || content == nil || content.Type != "text" || content.Text != "Sí" {
		t.Fatalf("got %+v, err %v", content, err)
	}
}

// A reply that names an option presses it, in the shape its message came in; one
// that names none is refused with the options; a reply to anything else is text.
func TestAReplyNamingAnOptionPressesIt(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	session := &Session{Address: "5491100000000"}
	keep := func(id string, data InteractiveData) string {
		raw, _ := json.Marshal(data)
		external := "wmw.5491100000000.5491144117777.5491144117777." + id
		if err := st.SaveInteractive(ctx, external, raw); err != nil {
			t.Fatal(err)
		}
		return external
	}
	options := []InteractiveOption{{ID: "si", Title: "Sí", Flow: "quick_reply"}, {ID: "no", Title: "No", Flow: "quick_reply"}}
	buttons := keep("B1", InteractiveData{Form: "buttons", Options: options})
	flow := keep("F1", InteractiveData{Form: "native_flow", Options: options})
	list := keep("L1", InteractiveData{Form: "list", Options: []InteractiveOption{{ID: "hip", Title: "Hipotecario"}}})
	links := keep("U1", InteractiveData{Form: "native_flow", Links: []InteractiveLink{{Title: "Web", URL: "https://x"}}})

	press := func(re, text string) (*waE2E.Message, int, error) {
		return pressFor(st, session, MessageContent{Type: "text", Text: text, ReMessageID: re})
	}

	msg, _, err := press(buttons, " sí ")
	if err != nil || msg.GetButtonsResponseMessage().GetSelectedButtonID() != "si" ||
		msg.GetButtonsResponseMessage().GetSelectedDisplayText() != "Sí" ||
		msg.GetButtonsResponseMessage().GetContextInfo().GetStanzaID() != "B1" {
		t.Fatalf("buttons: %v %v", msg, err)
	}

	msg, _, err = press(flow, "no")
	r := msg.GetInteractiveResponseMessage()
	if err != nil || r.GetBody().GetText() != "No" ||
		r.GetNativeFlowResponseMessage().GetName() != "quick_reply" ||
		r.GetNativeFlowResponseMessage().GetParamsJSON() != `{"id":"no"}` {
		t.Fatalf("native flow: %v %v", msg, err)
	}

	msg, _, err = press(list, "hip") // an option's id names it too
	if err != nil || msg.GetListResponseMessage().GetSingleSelectReply().GetSelectedRowID() != "hip" {
		t.Fatalf("list: %v %v", msg, err)
	}

	_, status, err := press(buttons, "Acepto")
	if status != 422 || err == nil || !strings.Contains(err.Error(), `"Sí", "No"`) {
		t.Fatalf("refusal: %d %v", status, err)
	}

	for _, re := range []string{links, "wmw.5491100000000.5491144117777.5491144117777.X9", ""} {
		if msg, _, err := press(re, "hola"); msg != nil || err != nil {
			t.Fatalf("plain text to %q: %v %v", re, msg, err)
		}
	}
}
