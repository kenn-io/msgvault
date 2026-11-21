package api

import "go.kenn.io/msgvault/internal/store"

const changesScanBudget = 2000

// scanChanges examines at most the budget; the extra row is only a sentinel.
func scanChanges(page store.ChangedMessagePage, since store.ChangedMessagesCursor, f changesFilter, limit int) ([]store.ChangedMessage, store.ChangedMessagesCursor, bool) {
	rows := page.Messages
	matches := make([]store.ChangedMessage, 0, limit)
	next := since
	examined := 0
	for i, row := range rows {
		if i == changesScanBudget {
			break
		}
		examined++
		next = store.ChangedMessagesAfter(row.ContentChangedAt, row.ID)
		if f.matches(row) {
			matches = append(matches, row)
			if len(matches) == limit {
				return matches, next, i+1 < len(rows)
			}
		}
	}
	if len(rows) > examined {
		return matches, next, true
	}
	if !page.CompleteThrough.IsZero() && (page.CompleteThrough.After(since.At()) || since.At().After(page.ServerTime)) {
		next = store.ChangedMessagesFrom(page.CompleteThrough)
	}
	return matches, next, false
}
