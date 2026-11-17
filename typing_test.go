package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func TestTypingNamesTheChatAndTheWriter(t *testing.T) {
	session := &Session{Address: "5491133585694"}
	at := time.Date(2026, 11, 16, 19, 37, 0, 0, time.UTC)
	peer := types.NewJID("5492614191189", types.DefaultUserServer)

	direct, ok := typingOf(session, &events.ChatPresence{
		MessageSource: types.MessageSource{Chat: peer, Sender: peer},
		State:         types.ChatPresenceComposing,
		Media:         types.ChatPresenceMediaAudio,
	}, at)
	want := WebhookTyping{
		ConversationAddress: "5492614191189",
		SenderAddress:       "5492614191189",
		State:               "composing",
		Media:               "audio",
		Timestamp:           "2026-11-16T19:37:00Z",
	}
	if !ok || direct != want {
		t.Fatalf("direct = %+v, %v; want %+v", direct, ok, want)
	}

	group := types.NewJID("120363423981194331", types.GroupServer)
	member := types.NewJID("5492616825604", types.DefaultUserServer)
	paused, ok := typingOf(session, &events.ChatPresence{
		MessageSource: types.MessageSource{Chat: group, Sender: member, IsGroup: true},
		State:         types.ChatPresencePaused,
		Media:         types.ChatPresenceMediaAudio,
	}, at)
	if !ok || paused.ConversationAddress != group.String() || paused.SenderAddress != member.User ||
		paused.State != "paused" || paused.Media != "" {
		t.Fatalf("group paused = %+v, %v", paused, ok)
	}

	for _, chat := range []types.JID{types.StatusBroadcastJID, types.NewJID("120363", types.NewsletterServer)} {
		if _, ok := typingOf(session, &events.ChatPresence{
			MessageSource: types.MessageSource{Chat: chat, Sender: member},
			State:         types.ChatPresenceComposing,
		}, at); ok {
			t.Errorf("%s: a chat with no typing to tell was forwarded", chat)
		}
	}
}
