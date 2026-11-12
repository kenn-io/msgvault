package mcp

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/query"
)

const ToolListMessageCollections = "list_message_collections"

type messageCollection struct {
	Name      string  `json:"name"`
	SourceIDs []int64 `json:"source_ids"`
}
type messageCollectionsResponse struct {
	Collections []messageCollection `json:"collections"`
}

func collectionProperty() *jsonschema.Schema {
	return stringSchema("Collection name from list_message_collections; mutually exclusive with account")
}

func listMessageCollectionsDefinition() toolDefinition {
	d := readDefinition(ToolListMessageCollections, "List named message collections and their source IDs. Empty collections match no messages.", closedObject(map[string]*jsonschema.Schema{}), outputSchemaFor[messageCollectionsResponse](), (*handlers).listMessageCollections)
	d.availability = func(c catalogCapabilities) bool { return c.collections }
	return d
}

func (h *handlers) listMessageCollections(ctx context.Context, _ toolRequest) (*toolResult, error) {
	lister, ok := h.engine.(query.CollectionScopeLister)
	if !ok {
		return toolErrorResult("message collections are unavailable"), nil
	}
	scopes, err := lister.ListCollectionScopes(ctx)
	if err != nil {
		return nil, newInternalError("list message collections", err)
	}
	out := messageCollectionsResponse{Collections: make([]messageCollection, 0, len(scopes))}
	for _, scope := range scopes {
		out.Collections = append(out.Collections, messageCollection{Name: scope.Name, SourceIDs: append([]int64{}, scope.SourceIDs...)})
	}
	return jsonResult(out)
}

func (h *handlers) resolveReadSelection(ctx context.Context, account, collection string) (accountSelection, error) {
	if collection == "" {
		return h.resolveAccount(ctx, account)
	}
	if account != "" {
		return accountSelection{}, &expectedHandlerError{message: "account and collection are mutually exclusive"}
	}
	lister, ok := h.engine.(query.CollectionScopeLister)
	if !ok {
		return accountSelection{}, &expectedHandlerError{message: "message collections are unavailable"}
	}
	scopes, err := lister.ListCollectionScopes(ctx)
	if err != nil {
		return accountSelection{}, newInternalError("list message collections", err)
	}
	for _, scope := range scopes {
		if scope.Name == collection {
			return accountSelection{sourceIDs: append([]int64{}, scope.SourceIDs...)}, nil
		}
	}
	return accountSelection{}, &expectedHandlerError{message: "collection not found: " + collection}
}

// singleSourceCollection limits native paths whose wire contract has one source.
func (a accountSelection) singleSourceCollection() error {
	if a.sourceIDs != nil && len(a.sourceIDs) > 1 {
		return &expectedHandlerError{message: "collection contains multiple sources; this operation requires one source"}
	}
	return nil
}
