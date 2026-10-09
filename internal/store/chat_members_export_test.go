package store

// SetChatMembersAfterClaimHookForTest pauses a chat member refresh after it
// claims queued chats, before it rewrites their member rows.
func (s *Store) SetChatMembersAfterClaimHookForTest(fn func()) func() {
	s.chatMembersAfterClaimHook = fn
	return func() { s.chatMembersAfterClaimHook = nil }
}
