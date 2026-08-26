package query

import (
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
)

// searchMessageVisibilityWhere returns the message visibility predicate for
// the lexical Search path. Source deletion is selectable, while dedup-hidden
// rows remain excluded in every scope. Unknown values fail closed to active.
func searchMessageVisibilityWhere(alias string, scope search.DeletionScope) string {
	switch scope {
	case search.DeletionScopeDeleted:
		return store.SourceDeletedMessagesWhere(alias)
	case search.DeletionScopeAny:
		return store.LiveMessagesWhere(alias, false)
	case search.DeletionScopeActive:
		return store.LiveMessagesWhere(alias, true)
	default:
		return store.LiveMessagesWhere(alias, true)
	}
}
