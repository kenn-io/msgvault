package store

import (
	"fmt"
	"strings"
)

// MessagesContentColumns are the columns of `messages` whose modification means
// the message changed in a way a reader must see again. This list drives the
// content_changed_at triggers on both backends.
//
// The invariant tying this list to the change feed is one-directional: every
// field ChangedMessage carries must appear here, EXCEPT id, source_id, and
// content_changed_at (immutable identity and the watermark itself). Any other
// field the feed reports but the trigger ignores would be cached stale by a
// consumer forever. The converse does not hold. This list also covers columns
// the feed does not return — sender_id and metadata are tracked because
// changing either means "re-read this message", yet neither is in the response
// — and that is correct: tracking a column the feed omits costs a redundant
// re-read, while omitting a column the feed returns costs silent staleness.
//
// That direction is enforced by TestChangesResponseFieldsAreAllTracked
// (internal/api/changes_test.go), which reflects over the feed's HTTP response
// type and requires every field it carries to appear in this list. The response
// type arrives with the HTTP endpoint, later in this change, so the test is not
// in the tree at this commit; until it lands the direction is held by review
// against ChangedMessage. TestMessagesColumnClassificationIsExhaustive already
// asserts the other half — that every real column of `messages` is classified
// here or in MessagesNonContentColumns.
//
// last_modified is NOT this list. That column is a true row-level watermark
// that bumps on any change and stays that way -- the embed worker relies on it
// as an optimistic-CAS token. The two columns answer different questions.
//
// Adding a column to `messages` without classifying it here or in
// MessagesNonContentColumns fails TestMessagesColumnClassificationIsExhaustive.
var MessagesContentColumns = []string{
	// source_message_id is NOT immutable, despite reading like a natural key:
	// UpdateMessageOnDedup (messages.go) rewrites it on a cross-mailbox
	// RFC822 dedup match and MigrateSourceMessageID rewrites
	// it when a message moves between source locations. The feed returns it, so
	// leaving it untracked would strand a consumer on a stale source ID.
	"source_message_id",
	"conversation_id",        // message moved threads
	"sender_id",              // sender re-resolved or corrected
	"message_type",           // reclassified
	"sent_at",                // canonical timestamp corrected
	"received_at",            // platform timestamp corrected
	"internal_date",          // feeds the reported sent_at via COALESCE
	"subject",                // content
	"snippet",                // content
	"metadata",               // platform-specific payload
	"size_estimate",          // reported to consumers
	"has_attachments",        // attachment set changed
	"attachment_count",       // attachment set changed
	"deleted_at",             // dedup-hidden / restored
	"deleted_from_source_at", // removed at the source
}

// MessagesNonContentColumns are the remaining columns of `messages`. Changing
// one does not move content_changed_at. Each carries its reason: the cost of a
// wrong call here is a consumer that silently misses updates.
var MessagesNonContentColumns = []string{
	"id",                // immutable identity
	sourceIDColumnName,  // immutable: which account this came from
	"rfc822_message_id", // not reported by the feed (dedup.go rewrites it)
	"read_at",           // local read state, not archive content
	"delivered_at",      // platform delivery receipt
	"is_from_me",        // not reported by the feed
	// The two provenance inputs behind is_from_me: source-native ownership as
	// the provider stated it, and ownership derived from the account's own
	// identities. The feed reports neither, and it does not report the
	// is_from_me they resolve to, so a change in either is invisible to a
	// consumer. Tracking them would stamp every message in an archive on the
	// attribution provenance migration for no reader-visible difference.
	"source_is_from_me",
	"identity_is_from_me",
	"reply_to_message_id", // threading pointer; conversation_id is the routing key
	"thread_position",     // ordering within a thread, derived
	"is_read",             // local read state
	"is_delivered",        // platform delivery state
	"is_sent",             // platform send state
	"is_edited",           // flag; the edit itself lands in subject/snippet/body
	"is_forwarded",        // platform flag
	"delete_batch_id",     // deletion-run bookkeeping; deleted_at carries the fact
	"archived_at",         // when we archived it, not when it changed
	"indexing_version",    // FTS index bookkeeping
	"last_modified",       // the other watermark
	"content_changed_at",  // itself
	"embed_gen",           // embedding watermark -- the whole reason this list exists
	"search_fts",          // PostgreSQL-only tsvector, maintained by the FTS path
}

// contentChangedTriggerColumnList renders MessagesContentColumns for a
// `... UPDATE OF <cols> ON messages ...` clause. Both dialects call this, so
// their trigger definitions cannot disagree.
func contentChangedTriggerColumnList() string {
	return strings.Join(MessagesContentColumns, ", ")
}

// contentChangedValueGuard renders the "did any content column actually change
// value?" half of the trigger's WHEN clause.
//
// The column list alone is not enough. Both backends fire `UPDATE OF` on the
// columns a statement NAMES, regardless of whether the value changed (measured
// on both), and the `ON CONFLICT ... DO UPDATE SET` of UpsertMessage's
// statement (upsertMessageSQL in messages.go) unconditionally re-assigns ten
// content columns on every re-sync of a known message. Without this guard every message a sync
// touches reports as changed and the feed carries no information.
//
// The comparison is null-safe both ways, so NULL->value and value->NULL count
// as changes and NULL->NULL does not. This mirrors the idiom the upsert already
// uses to decide whether a subject really changed (the embed_gen CASE in that
// same ON CONFLICT clause).
//
// distinctOp is "IS NOT" for SQLite, "IS DISTINCT FROM" for PostgreSQL.
func contentChangedValueGuard(distinctOp string) string {
	return columnValueGuard(MessagesContentColumns, distinctOp)
}

// columnValueGuard renders `(OLD.a <op> NEW.a OR OLD.b <op> NEW.b ...)` for a
// trigger WHEN clause or plpgsql IF over the given columns.
func columnValueGuard(columns []string, distinctOp string) string {
	clauses := make([]string, 0, len(columns))
	for _, c := range columns {
		clauses = append(clauses, fmt.Sprintf("OLD.%s %s NEW.%s", c, distinctOp, c))
	}
	return "(" + strings.Join(clauses, " OR ") + ")"
}
