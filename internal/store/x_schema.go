package store

// X persistence lock contract:
//
// Open/initialize Store first. Then acquire source execution ownership with
// AcquireSyncExecutionContext, then the credential lifecycle lock, then short
// Store transactions. Registration without a source may take credential ownership
// but must never acquire source execution while holding it. Reauth/removal release
// earlier credential locks before acquiring source ownership and reload in order.
// Removal uses AcquireSourceRemovalLockContext followed by RemoveSourceSerialized;
// acquiring a sync execution for removal would recover live diagnostic run rows.
// No provider/network request belongs inside a database transaction.
//
// x_post_versions and x_reply_attempts are additive IF NOT EXISTS DDL in each
// backend's main schema, so InitSchema creates them for both fresh and upgraded
// archives. Post aliases outlive local content via ON DELETE SET NULL. Receipts
// have no draft/conversation/message FK: draft deletion and conversation repair
// must not erase send authority. Source deletion cascades both tables.
//
// AcquireSyncExecutionContext already fences across processes. Acquiring it must
// never reset an admitted/unknown reply attempt: an abandoned admission may have
// reached X. Only a new caller that creates an attempt may submit it. All future
// attempt transitions must qualify source_id, draft_id and attempt_id together.
//
// Before deleting a losing provisional conversation, X consolidation must repoint
// chat_drafts.conversation_id to the survivor in the same transaction. Its native
// source_conversation_id/reply_to_source_message_id, revision and body stay pinned;
// a changed native destination requires review. No second draft pin is needed.
