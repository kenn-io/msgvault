package store

import "fmt"

// liveMessages* cover the four (alias, hideDeletedFromSource)
// combinations on hot read paths; pre-computing them avoids a
// fmt.Sprintf allocation per call on list/search/count paths.
const (
	liveMessagesUnaliasedDedupOnly = "deleted_at IS NULL"
	liveMessagesUnaliasedFull      = "deleted_at IS NULL AND deleted_from_source_at IS NULL"
	liveMessagesMDedupOnly         = "m.deleted_at IS NULL"
	liveMessagesMFull              = "m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL"
)

// LiveMessagesWhere returns the SQL predicate that selects live
// messages. Dedup-hidden rows (deleted_at IS NOT NULL) are filtered
// always — they must not appear in normal user-facing reads.
// Source-deleted rows (deleted_from_source_at IS NOT NULL) are
// filtered only when hideDeletedFromSource is true; archive views
// may intentionally show source-deleted rows, but they always hide
// dedup losers.
//
// Pass the table alias used in the surrounding query (use "" if the
// query has no alias).
//
// Predicate shape:
//
//	hideDeletedFromSource=false  →  <alias>.deleted_at IS NULL
//	hideDeletedFromSource=true   →  <alias>.deleted_at IS NULL AND
//	                                <alias>.deleted_from_source_at IS NULL
//
// The two common alias values ("" and "m") with both boolean values
// are returned from package-level constants to keep this allocation-
// free on hot paths. Other aliases fall back to fmt.Sprintf.
func LiveMessagesWhere(alias string, hideDeletedFromSource bool) string {
	switch {
	case alias == "" && !hideDeletedFromSource:
		return liveMessagesUnaliasedDedupOnly
	case alias == "" && hideDeletedFromSource:
		return liveMessagesUnaliasedFull
	case alias == "m" && !hideDeletedFromSource:
		return liveMessagesMDedupOnly
	case alias == "m" && hideDeletedFromSource:
		return liveMessagesMFull
	}
	if hideDeletedFromSource {
		return fmt.Sprintf(
			"%s.deleted_at IS NULL AND %s.deleted_from_source_at IS NULL",
			alias, alias,
		)
	}
	return fmt.Sprintf("%s.deleted_at IS NULL", alias)
}
