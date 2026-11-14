package main

import (
	"context"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// A channel (a newsletter, <id>@newsletter) is a conversation here when the
// account runs it, as its owner or one of its admins: a broadcast whose posts
// are the account's own, sent to followers who stay anonymous and whose
// reactions are counts, so no line in it names a reactor. A channel the
// account only follows is dropped wherever it appears. Whether the account
// runs one is WhatsApp's word, the viewer's role in the channel's metadata,
// read once a channel and kept until WhatsApp says the account left it.

// channel is what the bridge keeps of a channel: whether the account runs it,
// and its name.
type channel struct {
	runs bool
	name string
}

// isChannel is whether a JID is a channel's.
func isChannel(jid types.JID) bool {
	return jid.Server == types.NewsletterServer
}

// channelOf reads the account's standing in a channel off its metadata.
func channelOf(meta *types.NewsletterMetadata) channel {
	c := channel{name: meta.ThreadMeta.Name.Text}
	if meta.ViewerMeta != nil {
		switch meta.ViewerMeta.Role {
		case types.NewsletterRoleOwner, types.NewsletterRoleAdmin:
			c.runs = true
		}
	}
	return c
}

// channelReader reads a channel's metadata off the wire, the viewer's role
// included: Client.GetNewsletterInfo.
type channelReader func(ctx context.Context, jid types.JID) (*types.NewsletterMetadata, error)

// readChannel answers the account's standing in a channel: kept, else read now
// and kept. ok is false when the channel cannot be read; nothing is kept then,
// so the next line asks again.
func readChannel(session *Session, jid types.JID, read channelReader) (c channel, ok bool) {
	address := jid.ToNonAD().String()
	if c, ok := session.cachedChannel(address); ok {
		return c, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), GROUP_READ)
	defer cancel()
	meta, err := read(ctx, jid.ToNonAD())
	if err != nil || meta == nil {
		return channel{}, false
	}
	return session.noteChannel(address, meta), true
}

// channelFor is readChannel over the session's own connection.
func channelFor(session *Session, jid types.JID) (channel, bool) {
	if c, ok := session.cachedChannel(jid.ToNonAD().String()); ok || session.Client == nil {
		return c, ok
	}
	return readChannel(session, jid, session.Client.GetNewsletterInfo)
}

// runsChannel is whether the account runs a channel, as far as WhatsApp says.
func runsChannel(session *Session, jid types.JID) bool {
	c, _ := channelFor(session, jid)
	return c.runs
}

// channelPost answers a channel's post as the account's own message, or nil:
// for a channel the account only follows, or cannot learn its role in, and for
// a reaction, which in a channel names nobody.
func (m *Manager) channelPost(session *Session, evt *events.Message) *events.Message {
	c, ok := channelFor(session, evt.Info.Chat)
	if !ok {
		m.log.Warnf("Channel %s: the account's role could not be read — post %s dropped",
			evt.Info.Chat, evt.Info.ID)
		return nil
	}
	if !c.runs || evt.Message.GetReactionMessage() != nil || evt.Message.GetEncReactionMessage() != nil {
		return nil
	}
	post := *evt
	post.Info.IsFromMe = true
	return &post
}

// noteChannel keeps what a channel's metadata says of the account's standing
// in it, and answers it.
func (s *Session) noteChannel(address string, meta *types.NewsletterMetadata) channel {
	c := channelOf(meta)
	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()
	if s.channels == nil {
		s.channels = make(map[string]channel)
	}
	s.channels[address] = c
	return c
}

func (s *Session) cachedChannel(address string) (channel, bool) {
	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()
	c, ok := s.channels[address]
	return c, ok
}

// forgetChannel drops a channel the account left: its next line asks again.
func (s *Session) forgetChannel(address string) {
	s.channelsMu.Lock()
	defer s.channelsMu.Unlock()
	delete(s.channels, address)
}

// channelName is a channel's name as last read, "" when never read.
func (s *Session) channelName(address string) string {
	c, _ := s.cachedChannel(address)
	return c.name
}
