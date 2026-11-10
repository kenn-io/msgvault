package mcp

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/emailtags"
)

const ToolArchiveMessage = "archive_message"

type MessageArchiveBackend interface {
	ArchiveMessage(context.Context, int64, emailtags.ArchiveChange) (*emailtags.ArchiveResult, error)
}

func messageArchiveDefinition() toolDefinition {
	result, failure := outputSchemaFor[emailtags.ArchiveResult](), outputSchemaFor[emailtags.MessageTagError]()
	result.Schema, failure.Schema = "", ""
	definition := writeDefinition(ToolArchiveMessage, "Set one Gmail message's archive state. archived=true removes INBOX; false restores it. Retries preserve the requested state. Other providers return archive_unsupported. dry_run previews without writing.", closedObject(map[string]*jsonschema.Schema{
		"message_id": safeIDSchema("Archived message ID"), "archived": booleanSchema("Desired archive state"), "dry_run": booleanSchema("Preview without writing"),
	}, "message_id", "archived"), &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{result, failure}}, (*handlers).archiveMessage)
	definition.availability = func(c catalogCapabilities) bool { return c.messageArchive }
	yes := true
	definition.annotations.OpenWorldHint = &yes
	definition.annotations.IdempotentHint = true
	return definition
}

func (h *handlers) archiveMessage(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	id, err := getIDArg(args, "message_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	archived, ok := args["archived"].(bool)
	if !ok {
		return toolErrorResult("archived must be true or false"), nil
	}
	dryRun, _ := args["dry_run"].(bool)
	value, err := h.messageArchive.ArchiveMessage(ctx, id, emailtags.ArchiveChange{Archived: &archived, DryRun: dryRun})
	if err != nil {
		return messageTagErrorResult(err)
	}
	return jsonResult(value)
}
