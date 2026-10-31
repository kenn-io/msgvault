package store

// FullConversationWhere rejects absent policy state for supported chat sources.
func FullConversationWhere(alias string) string {
	return `(EXISTS (SELECT 1 FROM conversation_policy policy_full WHERE policy_full.conversation_id=` + alias + `.id AND policy_full.effective_tier='full') OR (NOT EXISTS (SELECT 1 FROM conversation_policy policy_missing WHERE policy_missing.conversation_id=` + alias + `.id) AND NOT EXISTS (SELECT 1 FROM sources policy_source WHERE policy_source.id=` + alias + `.source_id AND policy_source.source_type IN ('beeper','slack','slackdump','discord','teams'))))`
}

// FullMessageWhere admits messages whose conversation is still full-policy, or
// non-chat messages with no conversation.
func FullMessageWhere(alias string) string {
	return `(EXISTS (SELECT 1 FROM conversations policy_conversation WHERE policy_conversation.id=` + alias + `.conversation_id AND ` + FullConversationWhere("policy_conversation") + `) OR (` + alias + `.conversation_id IS NULL AND NOT EXISTS (SELECT 1 FROM sources policy_message_source WHERE policy_message_source.id=` + alias + `.source_id AND policy_message_source.source_type IN ('beeper','slack','slackdump','discord','teams'))))`
}

// FullActivityEventWhere hides a projected event as soon as its message's
// conversation becomes cold or otherwise ineligible, before the projection
// queue retracts the stored event.
func FullActivityEventWhere(alias string) string {
	return `EXISTS (SELECT 1 FROM messages policy_activity_message WHERE policy_activity_message.id=` + alias + `.message_id AND ` + FullMessageWhere("policy_activity_message") + `)`
}
