package mcp

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const toolArgCollection = "collection"
const toolListMessageCollections = "list_message_collections"

type messageCollection struct {
	Name      string  `json:"name"`
	SourceIDs []int64 `json:"source_ids"`
}
type messageCollectionsResponse struct {
	Collections []messageCollection `json:"collections"`
}

var fixedCollectionDefinition = sync.OnceValue(func() toolDefinition {
	return readDefinition(toolListMessageCollections, "List the daemon's user-managed named message collections and exact source IDs. The built-in All collection is excluded; an empty source_ids array means no matches. Membership is resolved anew on each scoped call.", closedObject(map[string]*jsonschema.Schema{}), outputSchemaFor[messageCollectionsResponse](), (*handlers).listMessageCollections)
})

// addCollectionScopeSchema runs only while an immutable catalog root is built.
func addCollectionScopeSchema(definition *toolDefinition) {
	switch definition.name {
	case ToolSearchMetadata, ToolSearchMessages, ToolListMessages, ToolAggregate, ToolGetStats, ToolSearchMessageBodies, ToolSemanticSearchMessages, ToolStageDeletion:
		if definition.name == ToolGetStats {
			definition.inputSchema.Properties[toolArgAccount] = accountProperty()
		}
		definition.inputSchema.Properties[toolArgCollection] = stringSchema("Exact user-managed message collection, mutually exclusive with account. Resolve once per call; empty collections match nothing on supported reads. Body/semantic/deletion paths require exactly one source.")
	}
}

func (h *handlers) listMessageCollections(ctx context.Context, _ toolRequest) (*toolResult, error) {
	lister, ok := h.engine.(query.CollectionScopeLister)
	if !ok {
		return toolErrorResult("message collections are unavailable on this engine"), nil
	}
	scopes, err := lister.ListCollectionScopes(ctx)
	if err != nil {
		return dependencyError("list message collections", err)
	}
	collections := make([]messageCollection, 0, len(scopes))
	for _, scope := range scopes {
		if scope.Name == store.DefaultCollectionName {
			continue
		}
		collections = append(collections, messageCollection{Name: scope.Name, SourceIDs: append([]int64{}, scope.SourceIDs...)})
	}
	return jsonResult(messageCollectionsResponse{Collections: collections})
}

// readSourceScope keeps explicit empty membership distinct from no scope.
func (h *handlers) readSourceScope(ctx context.Context, args map[string]any) (*int64, []int64, error) {
	collection, supplied := args[toolArgCollection]
	if !supplied {
		account, _ := args[toolArgAccount].(string)
		id, err := h.getAccountID(ctx, account)
		return id, nil, err
	}
	name, ok := collection.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, nil, &expectedHandlerError{message: "collection must be a non-empty name"}
	}
	if _, supplied := args[toolArgAccount]; supplied {
		return nil, nil, &expectedHandlerError{message: "account and collection are mutually exclusive"}
	}
	lister, ok := h.engine.(query.CollectionScopeLister)
	if !ok {
		return nil, nil, &expectedHandlerError{message: "message collections are unavailable on this engine"}
	}
	scopes, err := lister.ListCollectionScopes(ctx)
	if err != nil {
		return nil, nil, newInternalError("resolve message collection", err)
	}
	var sourceIDs []int64
	found := false
	for _, scope := range scopes {
		if scope.Name != name || scope.Name == store.DefaultCollectionName {
			continue
		}
		if found {
			return nil, nil, &expectedHandlerError{message: "collection name is ambiguous"}
		}
		found = true
		sourceIDs = append([]int64{}, scope.SourceIDs...)
	}
	if !found {
		return nil, nil, &expectedHandlerError{message: "collection not found"}
	}
	return nil, sourceIDs, nil
}

func (h *handlers) singleSourceScope(ctx context.Context, args map[string]any) (*int64, error) {
	id, ids, err := h.readSourceScope(ctx, args)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		return id, nil
	}
	if len(ids) != 1 {
		return nil, &expectedHandlerError{message: "this tool requires exactly one collection source; use metadata search, message lists, aggregates or statistics for multi-source or empty collections"}
	}
	return &ids[0], nil
}

func collectionAccounts(accounts []query.AccountInfo, id *int64, ids []int64) []query.AccountInfo {
	if id == nil && ids == nil {
		return accounts
	}
	selected := make([]query.AccountInfo, 0, len(accounts))
	for _, account := range accounts {
		if id != nil && account.ID == *id || ids != nil && slices.Contains(ids, account.ID) {
			selected = append(selected, account)
		}
	}
	return selected
}
