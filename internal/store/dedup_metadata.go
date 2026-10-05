package store

import (
	"encoding/hex"
	"encoding/json/v2"
	"strings"
)

// duplicateMetadataEvidence scores independent archived facts. Generic
// fallback conversation keys are omitted. Provider conversation IDs apply to
// historical copies too, without relying on metadata written by a later sync.
type duplicateMetadataEvidence struct {
	rfc822ID          string
	metadata          string
	providerThreadKey string
	hasReplyParent    bool
}

func (e duplicateMetadataEvidence) quality(sourceType, sourceMessageID string) int {
	sourceMessageID = strings.TrimSpace(sourceMessageID)
	quality := 0
	if (sourceType == "gmail" || sourceType == "imap" || sourceType == "msmail") && sourceMessageID != "" {
		quality++
	}
	if strings.TrimSpace(e.rfc822ID) != "" {
		quality++
	}
	threadKey := strings.TrimSpace(e.providerThreadKey)
	if e.hasReplyParent ||
		(sourceType == "gmail" && threadKey != "" && sourceMessageID != "" &&
			threadKey != sourceMessageID) ||
		(sourceType == "google-groups" && isGoogleGroupsProviderThread(threadKey)) ||
		hasArchivedReplyHeader(e.metadata) {
		quality++
	}
	return quality
}

func isGoogleGroupsProviderThread(key string) bool {
	hash, ok := strings.CutPrefix(key, "google-groups:")
	if !ok || len(hash) != 64 {
		return false
	}
	_, err := hex.DecodeString(hash)
	return err == nil
}

func hasArchivedReplyHeader(metadata string) bool {
	var fields struct {
		ReplyTo string `json:"email_in_reply_to"`
	}
	// Ignore unrelated values and damaged historical metadata. Other stored
	// facts still count when the metadata object is unreadable.
	return json.Unmarshal([]byte(metadata), &fields) == nil && strings.TrimSpace(fields.ReplyTo) != ""
}
