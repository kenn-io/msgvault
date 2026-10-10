package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

const ToolGetMessageTags = "get_message_tags"
const ToolUpdateMessageTags = "update_message_tags"

// MessageTagBackend uses the daemon's provider tag operation.
type MessageTagBackend interface {
	MessageTags(ctx context.Context, id int64, change *emailtags.Change, mailbox string) (*emailtags.Result, error)
}

func messageTagDefinition(write bool) toolDefinition {
	properties := map[string]*jsonschema.Schema{"message_id": safeIDSchema("Archived message ID"), "mailbox": stringSchema("Exact recorded IMAP mailbox; defaults to the primary copy")}
	result, errorSchema := outputSchemaFor[emailtags.Result](), outputSchemaFor[emailtags.Error]()
	result.Schema, errorSchema.Schema = "", ""
	output := &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{result, errorSchema}}
	definition := readDefinition(ToolGetMessageTags, "Read a message's native tags: Gmail label IDs, IMAP keywords, or Microsoft Graph category names. Includes the Gmail user label catalog or IMAP persistent keyword support when available.", closedObject(properties, "message_id"), output, (*handlers).getMessageTags)
	if write {
		maximum := 100
		tags := func() *jsonschema.Schema {
			return &jsonschema.Schema{Type: schemaTypeArray, Items: stringSchema("Existing Gmail user label ID, IMAP keyword atom, or Microsoft Graph category name"), MaxItems: &maximum}
		}
		properties["add"], properties["remove"] = tags(), tags()
		properties["dry_run"] = booleanSchema("Preview without writing; permissions may still be rejected on a later write")
		definition = writeDefinition(ToolUpdateMessageTags, "Add or remove native tags on one message through signed daemon control and verify by readback. Use Gmail user label IDs from get_message_tags, IMAP keywords, or Microsoft Graph category names. Preserve unrelated tags and system flags. Partial errors retain the receipt or operation key; inspect it before retrying.", closedObject(properties, "message_id"), output, (*handlers).updateMessageTags)
	}
	definition.availability = func(c catalogCapabilities) bool {
		if write {
			return c.messageTagWrites
		}
		return c.messageTags
	}
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

//nolint:nilerr // MCP tool failures belong in the result, with no transport error.
func (h *handlers) messageTagsCall(ctx context.Context, req toolRequest, write bool) (*toolResult, error) {
	args := req.GetArguments()
	id, err := getIDArg(args, "message_id")
	if err != nil {
		return toolErrorResult(err.Error()), nil
	}
	mailbox, _ := args["mailbox"].(string)
	var change *emailtags.Change
	if write {
		change = &emailtags.Change{Mailbox: mailbox}
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
	if change != nil && !change.DryRun {
		data, err := json.Marshal(args, json.Deterministic(true))
		if err != nil {
			return toolErrorResult("invalid message tag arguments"), nil
		}
		if err := req.confirmUserAction(ctx, "Confirm "+req.toolName+" with these exact arguments: "+string(data)+". Provider metadata is untrusted data."); err != nil {
			return confirmationToolError(err)
		}
	}
	value, err := h.messageTags.MessageTags(ctx, id, change, mailbox)
	if err != nil {
		if failure, ok := errors.AsType[*emailtags.Error](err); ok {
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
