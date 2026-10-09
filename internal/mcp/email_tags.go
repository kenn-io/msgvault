package mcp

import (
	"context"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

const ToolGetMessageTags = "get_message_tags"
const ToolUpdateMessageTags = "update_message_tags"

// MessageTagBackend uses the daemon's provider tag operation.
type MessageTagBackend interface {
	MessageTags(ctx context.Context, id int64, change *emailtags.MessageTagChange, mailbox string) (*emailtags.MessageTagResult, error)
}

func messageTagDefinition(write bool) toolDefinition {
	properties := map[string]*jsonschema.Schema{"message_id": safeIDSchema("Archived message ID"), "mailbox": stringSchema("Exact recorded IMAP mailbox; defaults to the current original copy or sole current membership")}
	result, errorSchema := outputSchemaFor[emailtags.MessageTagResult](), outputSchemaFor[emailtags.MessageTagError]()
	result.Schema, errorSchema.Schema = "", ""
	output := &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{result, errorSchema}}
	definition := readDefinition(ToolGetMessageTags, "Read a message's native tags: Gmail label IDs, IMAP keywords, or Microsoft Graph category names. Includes the Gmail user label catalog or IMAP persistent keyword support when available.", closedObject(properties, "message_id"), output, (*handlers).getMessageTags)
	if write {
		maximum := 100
		tags := func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: schemaTypeArray, Items: stringSchema("Existing Gmail user label ID matched exactly, IMAP keyword atom compared without case, or Microsoft Graph category name of at most 255 Unicode characters, without commas, compared without case"), MaxItems: &maximum}
		}
		properties["add"], properties["remove"] = tags(), tags()
		properties["dry_run"] = booleanSchema("Preview without writing; permissions may still be rejected on a later write")
		definition = writeDefinition(ToolUpdateMessageTags, "Add or remove native tags on one message and verify by readback. Use Gmail user label IDs from get_message_tags, IMAP keywords, or Microsoft Graph category names. Preserve unrelated tags and system flags. Graph 429 returns provider_write_failed; retry after the limit clears. Partial or uncertain errors include the last observed result; read tags before retrying.", closedObject(properties, "message_id"), output, (*handlers).updateMessageTags)
	}
	definition.availability = func(c catalogCapabilities) bool { return c.messageTags }
	yes := true
	definition.annotations.OpenWorldHint = &yes
	return definition
}
func (h *handlers) getMessageTags(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.messageTagsCall(ctx, req, false)
}
func (h *handlers) updateMessageTags(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.messageTagsCall(ctx, req, true)
}
func (h *handlers) messageTagsCall(ctx context.Context, req toolRequest, write bool) (*toolResult, error) {
	args := req.GetArguments()
	id, err := getIDArg(args, "message_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	mailbox, _ := args["mailbox"].(string)
	var change *emailtags.MessageTagChange
	if write {
		change = &emailtags.MessageTagChange{Mailbox: mailbox}
		change.DryRun, _ = args["dry_run"].(bool)
		for _, field := range []string{"add", "remove"} {
			values, ok := args[field].([]any)
			if !ok && args[field] != nil {
				return toolErrorResult(field + " must be an array of strings"), nil
			}
			for _, value := range values {
				tag, ok := value.(string)
				if !ok {
					return toolErrorResult(field + " must contain strings"), nil
				}
				if field == "add" {
					change.Add = append(change.Add, tag)
				} else {
					change.Remove = append(change.Remove, tag)
				}
			}
		}
	}
	value, err := h.messageTags.MessageTags(ctx, id, change, mailbox)
	if err != nil {
		if failure, ok := errors.AsType[*emailtags.MessageTagError](err); ok {
			result, encodeErr := jsonResult(failure)
			if result != nil {
				result.isError = true
			}
			return result, encodeErr
		}
		return toolErrorResult(daemonclient.SafeMCPError(err).Error()), nil
	}
	return jsonResult(value)
}
