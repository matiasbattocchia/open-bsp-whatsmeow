package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
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

// A vote names its choices by hash and is sealed under the poll's secret: it reads back
// to option names only when the bridge saw the poll, and lands on the poll's own id —
// in a DM, where the voter's key says "not mine" about OUR poll, and in a group, where
// the key names the poll's author outright.
func TestVoteReadsBackToThePollItAnswers(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()

	device := func(user string) *whatsmeow.Client {
		d := st.Container.NewDevice()
		id := types.JID{User: user, Device: 1, Server: types.DefaultUserServer}
		d.ID = &id
		d.Account = &waAdv.ADVSignedDeviceIdentity{
			Details:             []byte{0x01},
			AccountSignature:    make([]byte, 64),
			AccountSignatureKey: make([]byte, 32),
			DeviceSignature:     make([]byte, 64),
		}
		if err := st.Container.PutDevice(ctx, d); err != nil {
			t.Fatalf("PutDevice %s: %v", user, err)
		}
		return whatsmeow.NewClient(d, waLog.Noop)
	}
	us := types.NewJID("5491133585694", types.DefaultUserServer)
	ourClient := device(us.User)
	session := &Session{Client: ourClient, Address: us.User}
	m := &Manager{store: st, log: waLog.Noop}

	options := []string{"Sábado", "Domingo", "Lunes"}
	secret := make([]byte, 32)
	secret[0] = 7

	// poll arrives as `pollEvt` on our side; the voter holds the same poll as `voterInfo`
	vote := func(t *testing.T, voter *whatsmeow.Client, voterInfo types.MessageInfo, pollEvt *events.Message, voteSource types.MessageSource, picks ...string) *MessageContent {
		t.Helper()
		content, _ := m.buildContent(session, pollEvt, false)
		pollID := externalID(session.Address, chatSegment(session, pollEvt.Info.MessageSource),
			authorSegment(session, pollEvt.Info), pollEvt.Info.ID)
		m.rememberPoll(pollID, content)
		if err := ourClient.Store.MsgSecrets.PutMessageSecret(ctx, pollEvt.Info.Chat, pollEvt.Info.Sender, pollEvt.Info.ID, secret); err != nil {
			t.Fatalf("our secret: %v", err)
		}
		if err := voter.Store.MsgSecrets.PutMessageSecret(ctx, voterInfo.Chat, voterInfo.Sender, voterInfo.ID, secret); err != nil {
			t.Fatalf("voter's secret: %v", err)
		}
		update, err := voter.EncryptPollVote(ctx, &voterInfo, &waE2E.PollVoteMessage{
			SelectedOptions: whatsmeow.HashPollOptions(picks),
		})
		if err != nil {
			t.Fatalf("EncryptPollVote: %v", err)
		}
		got, _ := m.buildContent(session, &events.Message{
			Info:    types.MessageInfo{MessageSource: voteSource, ID: "VOTE-" + pollEvt.Info.ID},
			Message: &waE2E.Message{PollUpdateMessage: update},
		}, false)
		if got == nil {
			t.Fatal("vote did not read")
		}
		if got.Kind != "poll_vote" || got.ReMessageID != pollID {
			t.Fatalf("vote = %s on %q, want poll_vote on %q", got.Kind, got.ReMessageID, pollID)
		}
		return got
	}
	pollMessage := &waE2E.Message{PollCreationMessage: &waE2E.PollCreationMessage{
		Name:    proto.String("¿Cuándo?"),
		Options: []*waE2E.PollCreationMessage_Option{{OptionName: proto.String(options[0])}, {OptionName: proto.String(options[1])}, {OptionName: proto.String(options[2])}},
	}}

	t.Run("dm: the peer votes on our poll", func(t *testing.T) {
		peer := types.NewJID("5492616514662", types.DefaultUserServer)
		peerClient := device(peer.User)
		ourPoll := &events.Message{
			Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: peer, Sender: us, IsFromMe: true}, ID: "DMPOLL"},
			Message: pollMessage,
		}
		theirView := types.MessageInfo{MessageSource: types.MessageSource{Chat: us, Sender: us}, ID: "DMPOLL"}

		got := vote(t, peerClient, theirView, ourPoll, types.MessageSource{Chat: peer, Sender: peer}, "Lunes", "Sábado")
		var data PollVoteData
		_ = json.Unmarshal(got.Data, &data)
		want := PollVoteData{
			Question: "¿Cuándo?",
			Selected: []string{"Sábado", "Lunes"},
			Results: []PollResult{
				{Option: "Sábado", Votes: 1, Voters: []string{peer.User}},
				{Option: "Domingo", Votes: 0, Voters: []string{}},
				{Option: "Lunes", Votes: 1, Voters: []string{peer.User}},
			},
		}
		if !reflect.DeepEqual(data, want) {
			t.Fatalf("vote = %+v, want %+v (poll order)", data, want)
		}
	})

	t.Run("group: we vote on someone else's poll", func(t *testing.T) {
		group := types.NewJID("120363025580475259", types.GroupServer)
		author := types.NewJID("5491199999999", types.DefaultUserServer)
		info := types.MessageInfo{MessageSource: types.MessageSource{Chat: group, Sender: author, IsGroup: true}, ID: "GPOLL"}
		theirPoll := &events.Message{Info: info, Message: pollMessage}

		got := vote(t, ourClient, info, theirPoll, types.MessageSource{Chat: group, Sender: us, IsFromMe: true, IsGroup: true})
		var data PollVoteData
		_ = json.Unmarshal(got.Data, &data)
		if data.Selected == nil || len(data.Selected) != 0 {
			t.Fatalf("a withdrawn vote = %#v, want an empty selection", data.Selected)
		}
	})
}

func TestSelectedOptionsKeepsThePollsOrderAndCountsStrangers(t *testing.T) {
	options := []string{"a", "b", "c"}
	hashes := whatsmeow.HashPollOptions([]string{"c", "zzz", "a"})
	selected, unknown := selectedOptions(options, hashes)
	if !reflect.DeepEqual(selected, []string{"a", "c"}) || unknown != 1 {
		t.Fatalf("selected %v, unknown %d", selected, unknown)
	}
}

// The tally is each voter's latest pick: a change of mind moves the voter, a withdrawn
// vote removes them, and a history import arriving late cannot undo a newer live vote.
func TestPollResultsCountEachVotersLatestPick(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()

	at := func(minute int) time.Time { return time.Date(2026, 10, 8, 12, minute, 0, 0, time.UTC) }
	save := func(voter string, minute int, picks ...string) {
		if err := st.SaveVote(ctx, "poll", voter, append([]string{}, picks...), at(minute)); err != nil {
			t.Fatalf("SaveVote: %v", err)
		}
	}
	save("ana", 1, "Sábado")
	save("bea", 2, "Domingo", "Sábado")
	save("ana", 3, "Domingo") // changed her mind
	save("caro", 4, "Lunes")  //
	save("caro", 5)           // and withdrew
	save("ana", 0, "Lunes")   // a stale pick from history
	save("dani", 6, "Martes") // an option the poll does not have

	got, err := st.PollResults(ctx, "poll", []string{"Sábado", "Domingo", "Lunes"})
	if err != nil {
		t.Fatalf("PollResults: %v", err)
	}
	want := []PollResult{
		{Option: "Sábado", Votes: 1, Voters: []string{"bea"}},
		{Option: "Domingo", Votes: 2, Voters: []string{"bea", "ana"}},
		{Option: "Lunes", Votes: 0, Voters: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
}

// A history import carries a poll's votes already opened, keyed in the account's own
// frame: ours, a group member by participant, or — in a DM — the peer.
func TestSeedVotesNamesEachVoter(t *testing.T) {
	ctx := context.Background()
	st, err := OpenStore(ctx, "file:"+filepath.Join(t.TempDir(), "bridge.db"), waLog.Noop)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer st.DB.Close()
	m := &Manager{store: st, log: waLog.Noop}
	session := &Session{Address: "5491133585694"}
	poll := &PollData{Question: "?", Options: []string{"sí", "no"}}
	update := func(fromMe bool, participant string, picks ...string) *waWeb.PollUpdate {
		return &waWeb.PollUpdate{
			PollUpdateMessageKey: &waCommon.MessageKey{FromMe: proto.Bool(fromMe), Participant: proto.String(participant)},
			Vote:                 &waE2E.PollVoteMessage{SelectedOptions: whatsmeow.HashPollOptions(picks)},
			SenderTimestampMS:    proto.Int64(1),
		}
	}

	group := types.MessageSource{Chat: types.NewJID("120363025580475259", types.GroupServer), IsGroup: true}
	m.seedVotes(session, group, "gpoll", poll, []*waWeb.PollUpdate{
		update(true, "", "sí"),
		update(false, "5492616514662@s.whatsapp.net", "no"),
	})
	got, _ := st.PollResults(ctx, "gpoll", poll.Options)
	if !reflect.DeepEqual(got[0].Voters, []string{session.Address}) || !reflect.DeepEqual(got[1].Voters, []string{"5492616514662"}) {
		t.Fatalf("group results = %+v", got)
	}

	peer := types.NewJID("5491199999999", types.DefaultUserServer)
	m.seedVotes(session, types.MessageSource{Chat: peer, Sender: peer}, "dpoll", poll, []*waWeb.PollUpdate{
		update(false, "", "sí"),
	})
	got, _ = st.PollResults(ctx, "dpoll", poll.Options)
	if !reflect.DeepEqual(got[0].Voters, []string{peer.User}) {
		t.Fatalf("dm results = %+v", got)
	}
}
