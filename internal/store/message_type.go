package store

// MessageTypeEmail is the canonical messages.message_type value for email rows.
const MessageTypeEmail = "email"

// IsEmailMessageType reports whether a messages.message_type value denotes an
// email row. Rows imported before message_type existed carry a NULL/empty
// value and are treated as email everywhere, matching the SQL email filters
// in internal/query and the migration default in the store dialects.
func IsEmailMessageType(messageType string) bool {
	return messageType == "" || messageType == MessageTypeEmail
}
