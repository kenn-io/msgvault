package mcp

import (
	"context"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/store"
)

// ChatDiscoveryBackend discovers archived chats through the daemon.
type ChatDiscoveryBackend interface {
	SearchChats(ctx context.Context, q store.ChatDiscoveryQuery) (*store.ChatDiscoveryPage, error)
}

func findChatDefinition() toolDefinition {
	definition := readDefinition(ToolFindChat,
		"Find archived chats matching any whole query token in participant names, aliases, identifiers and titles. Exact matches rank first, then the most matching tokens within one name or title, then recent activity. Inspect the evidence before linking identities. Open the live archive with list_thread(id=result.message_id).",
		closedObject(map[string]*jsonschema.Schema{
			toolArgQuery: stringSchema("Name or chat title (max 256 UTF-8 bytes, 16 unique tokens)"),
			toolArgLimit: boundedIntegerWithDefault("Maximum results (default 20, max 100)", 1, 100, store.DefaultChatDiscoveryLimit),
			"source_id":  safeIDSchema("Optional archived source ID"),
		}, toolArgQuery), outputSchemaFor[store.ChatDiscoveryPage](),
		func(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
			return h.findChat(ctx, req)
		},
	)
	definition.availability = func(c catalogCapabilities) bool { return c.chatDiscovery }
	return definition
}

func (h *handlers) findChat(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	source, err := positiveInt64Arg(args, "source_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	q := store.ChatDiscoveryQuery{Query: stringArgument(args, toolArgQuery), Limit: limitArg(args, toolArgLimit, store.DefaultChatDiscoveryLimit), SourceID: source}
	if err := store.ValidateChatDiscoveryQuery(q); err != nil {
		return toolErrorResult(err.Error()), nil
	}
	page, err := h.chatDiscoveryBackend.SearchChats(ctx, q)
	if err != nil {
		var coded daemonAPIErrorCoder
		if errors.As(err, &coded) && coded.APIErrorCode() == "invalid_query" {
			return toolErrorResult("invalid_query: chat discovery query is invalid"), nil
		}
		return nil, newInternalError("find chat", err)
	}
	return jsonResult(page)
}
