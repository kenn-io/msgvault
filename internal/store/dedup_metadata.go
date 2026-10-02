package store

import (
	"encoding/hex"
	"encoding/json/v2"
	"strings"
)

// duplicateMetadataEvidence scores independent archived facts. Generic
// conversation keys are omitted; Groups' scoped provider-thread marker is real
// evidence preserved by the export importer.
type duplicateMetadataEvidence struct {
	rfc822ID          string
	metadata          string
	providerThreadKey string
	hasReplyParent    bool
}

func (e duplicateMetadataEvidence) quality(sourceType, sourceMessageID string) int {
	quality := 0
	if (sourceType == "gmail" || sourceType == "imap" || sourceType == "msmail") && strings.TrimSpace(sourceMessageID) != "" {
		quality++
	}
	if strings.TrimSpace(e.rfc822ID) != "" {
		quality++
	}
	var fields map[string]any
	// Damaged historical metadata must not prevent deduplication. Other stored
	// facts still count when the metadata object is unreadable.
	if err := json.Unmarshal([]byte(e.metadata), &fields); err != nil {
		fields = nil
	}
	if e.hasReplyParent || nonemptyMetadataString(fields, emailReplyMetadataKey) ||
		(sourceType == "gmail" && nonemptyMetadataString(fields, "gmail_thread_id")) ||
		(sourceType == "google-groups" && isGoogleGroupsProviderThread(e.providerThreadKey)) {
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

func nonemptyMetadataString(fields map[string]any, key string) bool {
	value, ok := fields[key].(string)
	return ok && strings.TrimSpace(value) != ""
}
