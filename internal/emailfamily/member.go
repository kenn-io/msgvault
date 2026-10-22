package emailfamily

import "go.kenn.io/msgvault/internal/mime"

// Member is one archived copy in the authorized email population. Provider
// thread IDs are authoritative only for source-native Gmail threads.
type Member struct {
	SourceType       string            `json:"source_type"`
	MessageID        int64             `json:"message_id"`
	SourceID         int64             `json:"source_id"`
	ProviderThreadID string            `json:"provider_thread_id"`
	Linkage          mime.EmailLinkage `json:"linkage"`
}
