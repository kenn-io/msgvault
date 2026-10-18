package store

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/calrecur"
)

const meetingLinkCandidateLimit = 200

func linkScopeSQL(scope SourceScope) (string, []any) {
	if scope.All {
		return "", nil
	}
	if len(scope.IDs) == 0 {
		return " AND 1=0", nil
	}
	args := make([]any, len(scope.IDs))
	marks := make([]string, len(scope.IDs))
	for i, id := range scope.IDs {
		args[i] = id
		marks[i] = "?"
	}
	return " AND m.source_id IN (" + strings.Join(marks, ",") + ")", args
}

func linkKeyPredicate(key meetingLinkKey) (string, []any) {
	switch key.Kind {
	case "google_event_id":
		if key.Base == "" {
			return `e.provider = 'google' AND e.provider_event_id = ?`, []any{key.Value}
		}
		id, err := calrecur.ParseKey(key.InstanceKey)
		if err != nil {
			return `e.provider = 'google' AND e.provider_event_id = ?`, []any{key.Value}
		}
		original, args := linkOriginalIdentityPredicate(id)
		return `e.provider = 'google' AND (e.provider_event_id = ? OR (e.kind = 'series' AND e.provider_event_id = ?) OR (e.kind = 'exception' AND e.series_provider_id = ? AND ` + original + `))`, append([]any{key.Value, key.Base, key.Base}, args...)
	case "ical_uid":
		return `e.ical_uid = ?`, []any{key.Value}
	case "join_key":
		return `e.join_key = ?`, []any{key.Value}
	default:
		return "1=0", nil
	}
}

func linkOriginalIdentityPredicate(id calrecur.Identity) (string, []any) {
	if id.AllDay {
		return `e.original_start_date = ?`, []any{id.Date}
	}
	return `(e.original_start_at >= ? AND e.original_start_at < ?)`, []any{id.Instant, id.Instant.Add(time.Second)}
}

// Scope precedes the sentinel row. A 201st row proves that this key is too
// shared for an automatic match; callers never evaluate a truncated set.
func boundedLinkEventIDs(ctx context.Context, r rowReader, scope SourceScope, predicate string, args []any) ([]int64, bool, error) {
	clause, scopeArgs := linkScopeSQL(scope)
	rows, err := r.QueryContext(ctx, `SELECT e.message_id FROM calendar_events e JOIN messages m ON m.id = e.message_id WHERE m.deleted_at IS NULL AND (`+predicate+`)`+clause+` ORDER BY e.message_id LIMIT 201`, append(args, scopeArgs...)...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, false, err
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if len(ids) > meetingLinkCandidateLimit {
		return nil, true, nil
	}
	return ids, false, nil
}
