package query

import (
	"strings"

	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
)

// deletableSourceTypesSQL limits interactive deletion and MCP structured staging
// to Gmail and Graph. MCP query staging also accepts IMAP because delete-staged
// has an IMAP client.
const deletableSourceTypesSQL = "('gmail', 'msmail')"

// executableDeletionSourceTypesSQL uses the same source types as delete-staged.
var executableDeletionSourceTypesSQL = "('" + strings.Join(sourceops.ExecutableDeletionSourceTypes(), "', '") + "')"

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
