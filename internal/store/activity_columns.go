package store

import "strings"

// MessagesActivityColumns are the columns of `messages` the dated-activity
// projector reads: the candidate row (activityCandidateColumns in
// activity_queries.go) plus the sender/ownership inputs of its person
// derivation. Only a change to one of these can move a message's activity
// event, so only these fire trg_activity_queue_messages_update on both backends.
//
// Every other column stays out on purpose. Bookkeeping such as embed_gen,
// search_fts, indexing_version, and last_modified is rewritten archive-wide by
// embedding and FTS backfills; content the projector never reads (subject,
// snippet, attachment counters, read/delivery flags) is re-stamped by every
// sync. A blanket trigger turned each of those sweeps into a full re-projection
// — and because the stored event carries the row's last_modified, the
// projector could not even short-circuit them as unchanged — while the pending
// queue held the contact-state freshness barrier open until it drained.
//
// last_modified is excluded even though the projector records it as
// ProjectedLastModified: that value is "last_modified as of the inputs that
// were projected" and is only ever compared against a candidate token loaded
// in the same batch (validateActivityEventToken), never against the live row.
//
// TestMessagesActivityColumnsAreRealColumns keeps this list honest against the
// live table.
var MessagesActivityColumns = []string{
	sourceIDColumnName,       // owner source; immutable in production, listed as read
	"conversation_id",        // routing key; co-presence and conversation_type
	"sender_id",              // direct-counterpart and direction derivation
	"message_type",           // channel classification (email/chat/meeting)
	"sent_at",                // occurrence timestamp candidates
	"received_at",            //
	"internal_date",          //
	"deleted_at",             // retraction
	"deleted_from_source_at", // retraction
	"metadata",               // meeting all_day/status inputs
	"source_is_from_me",      // source-native ownership
}

// activityTriggerColumnList renders MessagesActivityColumns for a
// `... UPDATE OF <cols> ON messages ...` clause. Both dialects call this, so
// their trigger definitions cannot disagree.
func activityTriggerColumnList() string {
	return strings.Join(MessagesActivityColumns, ", ")
}

// activityValueGuard renders the "did any activity input actually change
// value?" half of the trigger's guard, null-safe both ways. `UPDATE OF` alone
// fires on the columns a statement NAMES, and UpsertMessage's ON CONFLICT
// clause re-assigns most of these on every re-sync of a known message.
//
// distinctOp is "IS NOT" for SQLite, "IS DISTINCT FROM" for PostgreSQL.
func activityValueGuard(distinctOp string) string {
	return columnValueGuard(MessagesActivityColumns, distinctOp)
}
