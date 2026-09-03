//go:build pgvector

package pgvector

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/vector"
)

// escapeLikeSubject escapes SQL LIKE special characters so they match
// literally. Mirrors the sqlitevec helper of the same name.
func escapeLikeSubject(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// buildPGFilterClauses builds the list of SQL WHERE clauses (without
// leading "AND") that restrict a query to messages matching the
// structured filter. Each value is bound via the supplied bind closure,
// which appends the value to the caller's args slice and returns the
// matching $N placeholder.
//
// The returned slice never contains the live-message predicate; callers
// are expected to prepend that themselves (filterExistsClause and
// filteredChunkAndMessageCount seed it from store.LiveMessagesWhere;
// applyFilterClauses uses it inline).
func buildPGFilterClauses(f vector.Filter, bind func(any) string) []string {
	var clauses []string
	if len(f.MessageIDs) > 0 {
		clauses = append(clauses, fmt.Sprintf("m.id = ANY(%s::bigint[])", bind(int64Array(f.MessageIDs))))
	}

	if len(f.SourceIDs) > 0 {
		clauses = append(clauses, fmt.Sprintf("m.source_id = ANY(%s::bigint[])", bind(int64Array(f.SourceIDs))))
	}
	if len(f.ConversationIDs) > 0 {
		clauses = append(clauses, fmt.Sprintf("m.conversation_id = ANY(%s::bigint[])", bind(int64Array(f.ConversationIDs))))
	}
	if len(f.MessageTypes) > 0 {
		var exact []string
		includeLegacyEmail := false
		for _, value := range f.MessageTypes {
			if strings.EqualFold(value, "email") {
				includeLegacyEmail = true
			} else {
				exact = append(exact, value)
			}
		}
		var parts []string
		if len(exact) > 0 {
			parts = append(parts, fmt.Sprintf("m.message_type = ANY(%s::text[])", bind(textArray(exact))))
		}
		if includeLegacyEmail {
			parts = append(parts, fmt.Sprintf(
				"(m.message_type = %s OR m.message_type IS NULL OR m.message_type = '')", bind("email")))
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}
	for _, group := range f.SenderGroups {
		if len(group) == 0 {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (
				SELECT 1 FROM message_recipients mr
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type = 'from'
				   AND mr.participant_id = ANY(%s::bigint[])
			)`, bind(int64Array(group))))
	}
	for _, group := range f.SenderExactGroups {
		if len(group) == 0 {
			continue
		}
		ids := bind(int64Array(group))
		clauses = append(clauses, fmt.Sprintf(
			`(m.sender_id = ANY(%[1]s::bigint[]) OR EXISTS (
				SELECT 1 FROM message_recipients mr
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type = 'from'
				   AND mr.participant_id = ANY(%[1]s::bigint[])
			))`, ids))
	}
	for _, group := range f.RecipientAnyGroups {
		if len(group) == 0 {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (
				SELECT 1 FROM message_recipients mr
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type IN ('to', 'cc', 'bcc')
				   AND mr.participant_id = ANY(%s::bigint[])
			)`, bind(int64Array(group))))
	}
	appendRecipientGroups := func(recipientType string, groups [][]int64) {
		for _, ids := range groups {
			if len(ids) == 0 {
				continue
			}
			clauses = append(clauses, fmt.Sprintf(
				`EXISTS (
				SELECT 1 FROM message_recipients mr
				 WHERE mr.message_id = m.id
				   AND mr.recipient_type = '%s'
				   AND mr.participant_id = ANY(%s::bigint[])
			)`, recipientType, bind(int64Array(ids))))
		}
	}
	appendRecipientGroups("to", f.ToGroups)
	appendRecipientGroups("cc", f.CcGroups)
	appendRecipientGroups("bcc", f.BccGroups)

	for _, ids := range f.LabelGroups {
		if len(ids) == 0 {
			continue
		}
		clauses = append(clauses, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM message_labels ml
			          WHERE ml.message_id = m.id
			            AND ml.label_id = ANY(%s::bigint[]))`,
			bind(int64Array(ids))))
	}

	if f.HasAttachment != nil {
		clauses = append(clauses, "m.has_attachments = "+bind(*f.HasAttachment))
	}
	if f.After != nil {
		clauses = append(clauses, "m.sent_at >= "+bind(*f.After))
	}
	if f.Before != nil {
		clauses = append(clauses, "m.sent_at < "+bind(*f.Before))
	}
	if f.LargerThan != nil {
		clauses = append(clauses, "m.size_estimate > "+bind(*f.LargerThan))
	}
	if f.SmallerThan != nil {
		clauses = append(clauses, "m.size_estimate < "+bind(*f.SmallerThan))
	}
	for _, term := range f.SubjectSubstrings {
		// Case-insensitive to match SQLite's default ASCII-insensitive
		// LIKE and the store/query PostgreSQL search path. LOWER on both
		// sides keeps ESCAPE semantics intact (escape chars are ASCII).
		clauses = append(clauses, fmt.Sprintf(
			`LOWER(m.subject) LIKE LOWER(%s) ESCAPE '\'`,
			bind("%"+escapeLikeSubject(term)+"%")))
	}
	for _, term := range f.ListIDSubstrings {
		clauses = append(clauses, fmt.Sprintf(
			`LOWER(m.list_id) LIKE LOWER(%s) ESCAPE '\'`,
			bind("%"+escapeLikeSubject(term)+"%")))
	}
	for _, group := range f.ListIDExactGroups {
		if len(group) == 0 {
			continue
		}
		parts := make([]string, len(group))
		for i, listID := range group {
			parts[i] = fmt.Sprintf("LOWER(m.list_id) = LOWER(%s)", bind(listID))
		}
		clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
	}
	if f.ListID != "" {
		clauses = append(clauses, fmt.Sprintf(
			"LOWER(m.list_id) = LOWER(%s)", bind(f.ListID)))
	}

	return clauses
}

// buildPGFilterFragment converts buildPGFilterClauses output into the
// " AND ..." string appended after an existing WHERE predicate. Returns
// "" when the filter is empty so callers can safely concatenate.
func buildPGFilterFragment(f vector.Filter, bind func(any) string) string {
	clauses := buildPGFilterClauses(f, bind)
	if len(clauses) == 0 {
		return ""
	}
	return " AND " + strings.Join(clauses, " AND ")
}
