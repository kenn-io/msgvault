package query

import (
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// searchMessageVisibilityWhere returns the message visibility predicate for
// the lexical Search path. An explicit scope wins; the zero value keeps the
// historical HideDeleted-driven visibility so callers that never set a scope
// (the TUI, MCP staging, and the aggregate/stats search filters) are
// unaffected. Dedup-hidden rows remain excluded in every scope, and unknown
// non-empty values fail closed to active.
func searchMessageVisibilityWhere(alias string, q *search.Query) string {
	switch q.DeletionScope {
	case search.DeletionScopeDeleted:
		return store.SourceDeletedMessagesWhere(alias)
	case search.DeletionScopeAny:
		return store.LiveMessagesWhere(alias, false)
	case search.DeletionScopeActive:
		return store.LiveMessagesWhere(alias, true)
	case "":
		return store.LiveMessagesWhere(alias, q.HideDeleted)
	default:
		return store.LiveMessagesWhere(alias, true)
	}
}
