package inboxcontrol

const (
	DefaultContextBytes = 16384
	MaxContextBytes     = 65536
)

// ContextRequest selects one archived message without a provider read. A chat
// target additionally needs the exact archived message ID within that chat.
type ContextRequest struct {
	Target    Target `json:"target"`
	MessageID int64  `json:"message_id,omitempty"`
	MaxBytes  int    `json:"max_bytes,omitempty"`
}

func (r ContextRequest) Validate() error {
	if r.Target.Validate() != nil || r.MaxBytes < 0 || r.MaxBytes > MaxContextBytes {
		return ErrInvalid
	}
	if (r.Target.Scope == ScopeChat && r.MessageID <= 0) || (r.Target.Scope == ScopeMessage && r.MessageID != 0) {
		return ErrInvalid
	}
	return nil
}

// Context is bounded plain text from one exact archive primary key. Missing
// plain-text content is unavailable, distinct from a known empty message body.
type Context struct {
	Target      Target `json:"target"`
	MessageID   int64  `json:"message_id"`
	Text        string `json:"text"`
	Truncated   bool   `json:"truncated"`
	Unavailable bool   `json:"unavailable"`
}
