package whatsapp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
)

func textHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}
func textChatKey(chatID string) string { return "whatsapp_txt:" + textHash(chatID) }
func textMessageKey(chatID string, row textRecord, occurrence int) string {
	return "whatsapp_txt:" + textHash(chatID, row.WallTime, row.Precision, row.Kind, row.Sender, row.Body) + ":" + strconv.Itoa(occurrence)
}
