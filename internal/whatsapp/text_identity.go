package whatsapp

import (
	"strconv"

	"go.kenn.io/msgvault/internal/imazingcsv"
)

func textHash(parts ...string) string  { return imazingcsv.StableHash(parts...) }
func textChatKey(chatID string) string { return "whatsapp_txt:" + textHash(chatID) }
func textMessageKey(chatID string, row textRecord, occurrence int) string {
	payload := row.Body
	if row.Kind == "message" {
		payload = row.Sender + ": " + row.Body
	}
	return "whatsapp_txt:" + textHash(chatID, row.WallTime, row.Precision, payload) + ":" + strconv.Itoa(occurrence)
}
