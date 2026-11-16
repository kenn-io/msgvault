package store

// Canonical message types shared across import and migration paths.
const (
	MessageTypeEmail      = "email"
	MessageTypeGoogleChat = "google_chat"
)

// IsEmailMessageType reports whether a messages.message_type value denotes an
// email row. Rows imported before message_type existed carry a NULL/empty
// value and are treated as email everywhere, matching the SQL email filters
// in internal/query and the migration default in the store dialects.
func IsEmailMessageType(messageType string) bool {
	return messageType == "" || messageType == MessageTypeEmail
}

// IsWellFormedMessageType reports whether messageType has the shape of every
// msgvault message type: lowercase ASCII letters, digits and underscores,
// starting with a letter. Message import callers choose their own types, so a
// filter can name a type msgvault's own sources never write.
func IsWellFormedMessageType(messageType string) bool {
	if messageType == "" {
		return false
	}
	for i, r := range messageType {
		switch {
		case r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_'):
		default:
			return false
		}
	}
	return true
}
