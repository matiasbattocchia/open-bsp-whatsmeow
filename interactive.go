package main

// interactive.go — messages that offer their reader choices, in and out.
//
// A business's bot writes in shapes a person's chat never uses: reply buttons
// (ButtonsMessage), a list to open (ListMessage), native-flow buttons
// (InteractiveMessage, a template's interactive form included) and a template's
// quick replies (TemplateMessage). In, each reads as one DataPart kind
// "interactive": the part's text is the body the bot wrote, its data the choices.
// Out, a text that replies to one (re_message_id) and names one of its options,
// by title or by id, is the press itself: the response message that shape
// expects, which is what a bot reads as an answer — the same words typed as a
// plain message are not one.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// InteractiveData is a message's choices (DataPart kind "interactive"). Form is
// the wire shape it came in, which decides the shape its answer goes out in:
// buttons | list | native_flow | template. Options are what a reply can press;
// Links are buttons that open a URL or dial a number on the reader's phone,
// which a reply cannot press.
type InteractiveData struct {
	Form    string              `json:"form"`
	Header  string              `json:"header,omitempty"`
	Footer  string              `json:"footer,omitempty"`
	Button  string              `json:"button,omitempty"` // the label that opens a list
	Options []InteractiveOption `json:"options,omitempty"`
	Links   []InteractiveLink   `json:"links,omitempty"`
}

// InteractiveOption is one pressable choice. Flow names the native-flow button
// that carries it (quick_reply, single_select), the name its press answers
// under; Index is a template button's position, which its answer repeats.
type InteractiveOption struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description,omitempty"`
	Section     string  `json:"section,omitempty"`
	Flow        string  `json:"flow,omitempty"`
	Index       *uint32 `json:"index,omitempty"`
}

// InteractiveLink is a button that leaves the chat: a URL or a phone number.
type InteractiveLink struct {
	Title string `json:"title"`
	URL   string `json:"url,omitempty"`
	Phone string `json:"phone,omitempty"`
}

// nativeFlowParams is every field the native-flow buttons this file reads put in
// their ButtonParamsJSON: quick_reply {display_text, id}, cta_url {display_text,
// url}, cta_call {display_text, phone_number}, single_select {title, sections}.
type nativeFlowParams struct {
	DisplayText string `json:"display_text"`
	ID          string `json:"id"`
	URL         string `json:"url"`
	PhoneNumber string `json:"phone_number"`
	Title       string `json:"title"`
	Sections    []struct {
		Title string `json:"title"`
		Rows  []struct {
			ID          string `json:"id"`
			Title       string `json:"title"`
			Description string `json:"description"`
		} `json:"rows"`
	} `json:"sections"`
}

// addNativeFlowButton reads one native-flow button into data; a name this file
// does not know adds nothing.
func addNativeFlowButton(data *InteractiveData, name, paramsJSON string) {
	var p nativeFlowParams
	if err := json.Unmarshal([]byte(paramsJSON), &p); err != nil {
		return
	}
	switch name {
	case "quick_reply":
		data.Options = append(data.Options,
			InteractiveOption{ID: p.ID, Title: p.DisplayText, Flow: name})
	case "single_select":
		data.Button = p.Title
		for _, section := range p.Sections {
			for _, row := range section.Rows {
				data.Options = append(data.Options, InteractiveOption{
					ID: row.ID, Title: row.Title, Description: row.Description,
					Section: section.Title, Flow: name,
				})
			}
		}
	case "cta_url":
		data.Links = append(data.Links, InteractiveLink{Title: p.DisplayText, URL: p.URL})
	case "cta_call":
		data.Links = append(data.Links, InteractiveLink{Title: p.DisplayText, Phone: p.PhoneNumber})
	}
}

func nativeFlowOf(msg *waE2E.InteractiveMessage) (string, *InteractiveData) {
	data := &InteractiveData{
		Form:   "native_flow",
		Header: msg.GetHeader().GetTitle(),
		Footer: msg.GetFooter().GetText(),
	}
	for _, button := range msg.GetNativeFlowMessage().GetButtons() {
		addNativeFlowButton(data, button.GetName(), button.GetButtonParamsJSON())
	}
	return msg.GetBody().GetText(), data
}

// interactiveOf reads the choices msg offers, with the body they come under; nil
// when msg offers none.
func interactiveOf(msg *waE2E.Message) (string, *InteractiveData) {
	if b := msg.GetButtonsMessage(); b != nil {
		data := &InteractiveData{Form: "buttons", Header: b.GetText(), Footer: b.GetFooterText()}
		for _, button := range b.GetButtons() {
			if button.GetType() == waE2E.ButtonsMessage_Button_NATIVE_FLOW {
				info := button.GetNativeFlowInfo()
				addNativeFlowButton(data, info.GetName(), info.GetParamsJSON())
				continue
			}
			data.Options = append(data.Options, InteractiveOption{
				ID: button.GetButtonID(), Title: button.GetButtonText().GetDisplayText(),
			})
		}
		return b.GetContentText(), data
	}

	if l := msg.GetListMessage(); l != nil {
		data := &InteractiveData{
			Form: "list", Header: l.GetTitle(), Footer: l.GetFooterText(), Button: l.GetButtonText(),
		}
		for _, section := range l.GetSections() {
			for _, row := range section.GetRows() {
				data.Options = append(data.Options, InteractiveOption{
					ID: row.GetRowID(), Title: row.GetTitle(), Description: row.GetDescription(),
					Section: section.GetTitle(),
				})
			}
		}
		return l.GetDescription(), data
	}

	if i := msg.GetInteractiveMessage(); i != nil {
		return nativeFlowOf(i)
	}

	if t := msg.GetTemplateMessage(); t != nil {
		if i := t.GetInteractiveMessageTemplate(); i != nil {
			return nativeFlowOf(i)
		}
		h := t.GetHydratedTemplate()
		if h == nil {
			h = t.GetHydratedFourRowTemplate()
		}
		if h == nil {
			return "", nil
		}
		data := &InteractiveData{
			Form: "template", Header: h.GetHydratedTitleText(), Footer: h.GetHydratedFooterText(),
		}
		for _, button := range h.GetHydratedButtons() {
			index := button.GetIndex()
			switch {
			case button.GetQuickReplyButton() != nil:
				q := button.GetQuickReplyButton()
				data.Options = append(data.Options, InteractiveOption{
					ID: q.GetID(), Title: q.GetDisplayText(), Index: &index,
				})
			case button.GetUrlButton() != nil:
				u := button.GetUrlButton()
				data.Links = append(data.Links, InteractiveLink{Title: u.GetDisplayText(), URL: u.GetURL()})
			case button.GetCallButton() != nil:
				c := button.GetCallButton()
				data.Links = append(data.Links, InteractiveLink{Title: c.GetDisplayText(), Phone: c.GetPhoneNumber()})
			}
		}
		return h.GetHydratedContentText(), data
	}

	return "", nil
}

// responseText is the choice an answer to an interactive message names, as the
// chat shows it — what a person's own press reads as, echoed back to us.
func responseText(msg *waE2E.Message) (text string, ctx *waE2E.ContextInfo) {
	switch {
	case msg.GetButtonsResponseMessage() != nil:
		r := msg.GetButtonsResponseMessage()
		return r.GetSelectedDisplayText(), r.GetContextInfo()
	case msg.GetListResponseMessage() != nil:
		r := msg.GetListResponseMessage()
		return r.GetTitle(), r.GetContextInfo()
	case msg.GetInteractiveResponseMessage() != nil:
		r := msg.GetInteractiveResponseMessage()
		return r.GetBody().GetText(), r.GetContextInfo()
	case msg.GetTemplateButtonReplyMessage() != nil:
		r := msg.GetTemplateButtonReplyMessage()
		return r.GetSelectedDisplayText(), r.GetContextInfo()
	}
	return "", nil
}

// rememberInteractive keeps the choices a message carries, if it carries any,
// under the message's external id — the id an answer will reply to.
func (m *Manager) rememberInteractive(externalID string, content *MessageContent) {
	if content.Type != "data" || content.Kind != "interactive" {
		return
	}
	if err := m.store.SaveInteractive(context.Background(), externalID, content.Data); err != nil {
		m.log.Errorf("Save interactive %s failed: %v", externalID, err)
	}
}

// pressFor is the answer a text makes to the interactive message it replies to,
// when it names one of that message's options; nil when the text replies to no
// interactive message the bridge kept, or to one with nothing to press. Naming
// none of the options of one that has some is a refusal that lists them.
func pressFor(store *Store, session *Session, content MessageContent) (*waE2E.Message, int, error) {
	if content.ReMessageID == "" {
		return nil, 0, nil
	}
	data, err := store.GetInteractive(context.Background(), content.ReMessageID)
	if err != nil {
		return nil, http.StatusInternalServerError, fmt.Errorf("read interactive: %w", err)
	}
	if data == nil || len(data.Options) == 0 {
		return nil, 0, nil
	}
	said := strings.TrimSpace(content.Text)
	var picked *InteractiveOption
	for i := range data.Options {
		o := &data.Options[i]
		if strings.EqualFold(said, strings.TrimSpace(o.Title)) || (o.ID != "" && said == o.ID) {
			picked = o
			break
		}
	}
	if picked == nil {
		titles := make([]string, 0, len(data.Options))
		for _, o := range data.Options {
			titles = append(titles, fmt.Sprintf("%q", o.Title))
		}
		return nil, http.StatusUnprocessableEntity, fmt.Errorf(
			"%q is none of that message's options — answer with one of %s, "+
				"or write without replying to it",
			said, strings.Join(titles, ", "))
	}

	ctx := replyContext(session, content)
	switch data.Form {
	case "buttons":
		return &waE2E.Message{ButtonsResponseMessage: &waE2E.ButtonsResponseMessage{
			SelectedButtonID: proto.String(picked.ID),
			Response: &waE2E.ButtonsResponseMessage_SelectedDisplayText{
				SelectedDisplayText: picked.Title,
			},
			Type:        waE2E.ButtonsResponseMessage_DISPLAY_TEXT.Enum(),
			ContextInfo: ctx,
		}}, 0, nil
	case "list":
		return &waE2E.Message{ListResponseMessage: &waE2E.ListResponseMessage{
			Title:             proto.String(picked.Title),
			Description:       optString(picked.Description),
			ListType:          waE2E.ListResponseMessage_SINGLE_SELECT.Enum(),
			SingleSelectReply: &waE2E.ListResponseMessage_SingleSelectReply{SelectedRowID: proto.String(picked.ID)},
			ContextInfo:       ctx,
		}}, 0, nil
	case "template":
		return &waE2E.Message{TemplateButtonReplyMessage: &waE2E.TemplateButtonReplyMessage{
			SelectedID:          proto.String(picked.ID),
			SelectedDisplayText: proto.String(picked.Title),
			SelectedIndex:       picked.Index,
			ContextInfo:         ctx,
		}}, 0, nil
	default: // native_flow
		params, err := json.Marshal(map[string]string{"id": picked.ID})
		if err != nil {
			return nil, http.StatusInternalServerError, err
		}
		flow := picked.Flow
		if flow == "" {
			flow = "quick_reply"
		}
		return &waE2E.Message{InteractiveResponseMessage: &waE2E.InteractiveResponseMessage{
			Body: &waE2E.InteractiveResponseMessage_Body{
				Text:   proto.String(picked.Title),
				Format: waE2E.InteractiveResponseMessage_Body_DEFAULT.Enum(),
			},
			InteractiveResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage_{
				NativeFlowResponseMessage: &waE2E.InteractiveResponseMessage_NativeFlowResponseMessage{
					Name:       proto.String(flow),
					ParamsJSON: proto.String(string(params)),
					Version:    proto.Int32(3),
				},
			},
			ContextInfo: ctx,
		}}, 0, nil
	}
}
