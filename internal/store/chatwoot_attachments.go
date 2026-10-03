package store

// MessageChatwootAttachments returns media occurrences keyed by stable provider
// IDs, independent of signed URLs.
func (s *Store) MessageChatwootAttachments(messageID int64) (map[string]AttachmentRef, error) {
	return s.messageProviderAttachments(messageID, "chatwoot:")
}

func (s *Store) ReplaceMessageChatwootAttachments(messageID int64, refs []AttachmentRef) error {
	return s.replaceMessageProviderAttachments(messageID, "chatwoot:", refs)
}
