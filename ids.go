package main

import (
	"fmt"
	"strings"
)

// externalID builds the globally-unique messages.external_id. whatsmeow
// message IDs are only unique per chat/sender, while OpenBSP has a global
// unique constraint, so the ID is namespaced by the session's own number and
// the chat: wmw.<own>.<chat>.<id>
func externalID(own, chat, id string) string {
	return fmt.Sprintf("wmw.%s.%s.%s", own, chat, id)
}

// parseExternalID reverses externalID. The message ID may itself contain
// dots, so only the first three segments are split off.
func parseExternalID(ext string) (own, chat, id string, err error) {
	parts := strings.SplitN(ext, ".", 4)
	if len(parts) != 4 || parts[0] != "wmw" {
		return "", "", "", fmt.Errorf("not a whatsapp-web external_id: %s", ext)
	}
	return parts[1], parts[2], parts[3], nil
}
