package whatsapp

import (
	"strconv"

	"go.kenn.io/msgvault/internal/imazingcsv"
)

func textHash(parts ...string) string  { return imazingcsv.StableHash(parts...) }
func textChatKey(chatID string) string { return "whatsapp_txt:" + textHash(chatID) }
func textMessageKey(chatID string, row textRecord, occurrence int) string {
	return "whatsapp_txt:" + textHash(chatID, row.WallTime, row.Precision, row.Kind, row.Sender, row.Body) + ":" + strconv.Itoa(occurrence)
}
