package main

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// rememberPoll keeps the poll a message carries, if it carries one, under the
// message's external id — the id its votes will point at — and answers it.
func (m *Manager) rememberPoll(externalID string, content *MessageContent) *PollData {
	if content.Type != "data" || content.Kind != "poll" {
		return nil
	}
	var poll PollData
	if err := json.Unmarshal(content.Data, &poll); err != nil {
		m.log.Errorf("Read poll %s failed: %v", externalID, err)
		return nil
	}
	if err := m.store.SavePoll(context.Background(), externalID, poll); err != nil {
		m.log.Errorf("Save poll %s failed: %v", externalID, err)
	}
	return &poll
}

// seedVotes keeps the votes a history import carries on a poll, already opened:
// the import brings no vote rows, but the tally a later live vote reports starts
// from these. The keys are in the account's own frame — it is our copy of the chat.
func (m *Manager) seedVotes(session *Session, source types.MessageSource, pollID string, poll *PollData, updates []*waWeb.PollUpdate) {
	ctx := context.Background()
	for _, update := range updates {
		key := update.GetPollUpdateMessageKey()
		var voter string
		switch {
		case key.GetFromMe():
			voter = session.Address
		case key.GetParticipant() != "":
			jid, err := types.ParseJID(key.GetParticipant())
			if err != nil {
				continue
			}
			voter = canonicalUser(session, jid, types.JID{})
		case !source.IsGroup:
			voter = chatSegment(session, source)
		default:
			continue
		}
		selected, _ := selectedOptions(poll.Options, update.GetVote().GetSelectedOptions())
		at := time.UnixMilli(update.GetSenderTimestampMS())
		if err := m.store.SaveVote(ctx, pollID, voter, selected, at); err != nil {
			m.log.Errorf("Save vote on poll %s failed: %v", pollID, err)
		}
	}
}

// pollVote reads a vote: opened with the poll's message secret, its hashes matched
// back to the options the poll was seen with. Nil when either is missing — a poll
// from before the bridge was there, or one it was never sent.
func (m *Manager) pollVote(session *Session, evt *events.Message, update *waE2E.PollUpdateMessage) *MessageContent {
	key := update.GetPollCreationMessageKey()
	pollID := externalID(
		session.Address, chatSegment(session, evt.Info.MessageSource),
		keySender(
			session, evt.Info.MessageSource, authorSegment(session, evt.Info),
			key.GetFromMe(), key.GetParticipant(),
		),
		key.GetID(),
	)

	ctx := context.Background()
	poll, err := m.store.GetPoll(ctx, pollID)
	if err != nil {
		m.log.Errorf("Load poll %s failed: %v", pollID, err)
		return nil
	}
	if poll == nil {
		m.log.Warnf("Vote %s is on poll %s, which the bridge never saw", evt.Info.ID, pollID)
		return nil
	}

	vote, err := session.Client.DecryptPollVote(ctx, evt)
	if err != nil {
		m.log.Warnf("Vote %s on poll %s: %v", evt.Info.ID, pollID, err)
		return nil
	}

	selected, unknown := selectedOptions(poll.Options, vote.GetSelectedOptions())
	if unknown > 0 {
		m.log.Warnf("Vote %s on poll %s picks %d option(s) the poll was not seen with",
			evt.Info.ID, pollID, unknown)
	}

	data := PollVoteData{Question: poll.Question, Selected: selected}
	at := evt.Info.Timestamp
	if sent := update.GetSenderTimestampMS(); sent > 0 {
		at = time.UnixMilli(sent)
	}
	if err := m.store.SaveVote(ctx, pollID, authorSegment(session, evt.Info), selected, at); err != nil {
		m.log.Errorf("Save vote %s on poll %s failed: %v", evt.Info.ID, pollID, err)
	} else if data.Results, err = m.store.PollResults(ctx, pollID, poll.Options); err != nil {
		m.log.Errorf("Tally poll %s failed: %v", pollID, err)
	}

	content, err := dataPart("poll_vote", data)
	if err != nil {
		m.log.Errorf("Encode vote %s failed: %v", evt.Info.ID, err)
		return nil
	}
	content.ReMessageID = pollID
	return content
}

// selectedOptions names the options a vote's hashes stand for, in the poll's order,
// and counts the hashes no option answers to.
func selectedOptions(options []string, hashes [][]byte) (selected []string, unknown int) {
	selected = []string{}
	optionHashes := whatsmeow.HashPollOptions(options)
	for _, hash := range hashes {
		matched := false
		for _, optionHash := range optionHashes {
			if bytes.Equal(hash, optionHash) {
				matched = true
				break
			}
		}
		if !matched {
			unknown++
		}
	}
	for i, optionHash := range optionHashes {
		for _, hash := range hashes {
			if bytes.Equal(hash, optionHash) {
				selected = append(selected, options[i])
				break
			}
		}
	}
	return selected, unknown
}
