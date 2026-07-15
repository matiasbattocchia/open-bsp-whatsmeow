package main

import (
	"fmt"
	"strings"
)

// externalID builds the globally-unique messages.external_id:
//
//	wmw.<own>.<chat>.<sender>.<id>
//
// whatsmeow message IDs are only unique per chat, while OpenBSP has a global
// unique constraint, so the ID is namespaced by the session's own number and
// the chat. The sender segment (canonical digits of who sent the message;
// equals <own> for own messages) exists because quoting/reacting on the
// WhatsApp protocol needs the full MessageKey {chat, id, fromMe,
// participant}: direction falls out of sender == own, and sender doubles as
// the group participant — no OpenBSP lookup required.
func externalID(own, chat, sender, id string) string {
	return fmt.Sprintf("wmw.%s.%s.%s.%s", own, chat, sender, id)
}

// parseExternalID reverses externalID. The message ID may itself contain
// dots, so only the first four segments are split off.
func parseExternalID(ext string) (own, chat, sender, id string, err error) {
	parts := strings.SplitN(ext, ".", 5)
	if len(parts) != 5 || parts[0] != "wmw" {
		return "", "", "", "", fmt.Errorf("not a whatsapp-web external_id: %s", ext)
	}
	return parts[1], parts[2], parts[3], parts[4], nil
}
