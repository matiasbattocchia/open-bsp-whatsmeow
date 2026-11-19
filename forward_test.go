package main

import (
	"encoding/json"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// A forward goes out marked forwarded, one hop from what it copies, and quotes
// nothing even when the record names a message: plain text becomes an extended
// text so it can wear the mark, and a pin wears it the same way.
func TestAForwardIsMarkedAndQuotesNothing(t *testing.T) {
	session := &Session{Address: "5491100000000"}
	chat := types.NewJID("5491144117777", types.DefaultUserServer)
	build := func(content MessageContent) *waE2E.Message {
		t.Helper()
		var req dispatchRequest
		req.Record.Content = content
		// no store: a forward never asks it what a reply would press
		msg, status, err := buildOutgoingMessage(nil, nil, session, chat, req)
		if err != nil || status != 0 {
			t.Fatalf("build %+v: %d %v", content, status, err)
		}
		return msg
	}
	marked := func(name string, ctx *waE2E.ContextInfo) {
		t.Helper()
		if !ctx.GetIsForwarded() || ctx.GetForwardingScore() != 1 || ctx.GetStanzaID() != "" {
			t.Fatalf("%s: context %v", name, ctx)
		}
	}

	text := build(MessageContent{Type: "text", Kind: "text", Text: "hola", Forwarded: true})
	if text.GetConversation() != "" || text.GetExtendedTextMessage().GetText() != "hola" {
		t.Fatalf("text: %v", text)
	}
	marked("text", text.GetExtendedTextMessage().GetContextInfo())

	re := "wmw.5491100000000.5491144117777.5491144117777.B1"
	quoted := build(MessageContent{Type: "text", Kind: "text", Text: "hola", ReMessageID: re, Forwarded: true})
	marked("text naming a message", quoted.GetExtendedTextMessage().GetContextInfo())

	pin, _ := json.Marshal(LocationData{Latitude: -32.89, Longitude: -68.83, Name: "Casa"})
	location := build(MessageContent{Type: "data", Kind: "location", Data: pin, Forwarded: true})
	marked("location", location.GetLocationMessage().GetContextInfo())

	plain := build(MessageContent{Type: "text", Kind: "text", Text: "hola"})
	if plain.GetConversation() != "hola" {
		t.Fatalf("a message of our own stays plain: %v", plain)
	}
}

// What the sender forwarded says so, whichever message type carries the mark.
func TestIsForwardedReadsTheMark(t *testing.T) {
	mark := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(2)}
	for name, msg := range map[string]*waE2E.Message{
		"text":     {ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("x"), ContextInfo: mark}},
		"image":    {ImageMessage: &waE2E.ImageMessage{ContextInfo: mark}},
		"audio":    {AudioMessage: &waE2E.AudioMessage{ContextInfo: mark}},
		"location": {LocationMessage: &waE2E.LocationMessage{ContextInfo: mark}},
	} {
		if !isForwarded(msg) {
			t.Fatalf("%s: not read as forwarded", name)
		}
	}
	for name, msg := range map[string]*waE2E.Message{
		"plain":  {Conversation: proto.String("x")},
		"quoted": {ExtendedTextMessage: &waE2E.ExtendedTextMessage{ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("A")}}},
	} {
		if isForwarded(msg) {
			t.Fatalf("%s: read as forwarded", name)
		}
	}
}
