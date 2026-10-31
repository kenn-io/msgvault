package store

// FullConversationWhere admits absent staged policy state and rejects only
// conversations with an explicitly non-full effective tier.
func FullConversationWhere(alias string) string {
	return `NOT EXISTS (SELECT 1 FROM conversation_policy policy_full WHERE policy_full.conversation_id=` + alias + `.id AND policy_full.effective_tier<>'full')`
}

// FullMessageWhere admits messages with no conversation or a full-policy one.
func FullMessageWhere(alias string) string {
	return `(` + alias + `.conversation_id IS NULL OR EXISTS (SELECT 1 FROM conversations policy_conversation WHERE policy_conversation.id=` + alias + `.conversation_id AND ` + FullConversationWhere("policy_conversation") + `))`
}

// FullActivityEventWhere hides a projected event as soon as its message's
// conversation becomes cold or otherwise ineligible, before the projection
// queue retracts the stored event.
func FullActivityEventWhere(alias string) string {
	return `EXISTS (SELECT 1 FROM messages policy_activity_message WHERE policy_activity_message.id=` + alias + `.message_id AND ` + FullMessageWhere("policy_activity_message") + `)`
}
