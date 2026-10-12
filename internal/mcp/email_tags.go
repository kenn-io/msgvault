package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
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
		exact := outputSchemaFor[inboxcontrol.Request]()
		for _, field := range []string{"target", "expected", "preview_token", "idempotency_key"} {
			properties[field] = exact.Properties[field]
		}
		signed := outputSchemaFor[inboxToolResponse]()
		signed.Schema = ""
		output.AnyOf = append(output.AnyOf, signed)
		definition = writeDefinition(ToolUpdateMessageTags, "Add or remove native tags on one message after confirmation and verify by readback. Gmail writes use signed daemon previews and retain a durable receipt or operation key. Use Gmail user label IDs from get_message_tags or, for delegated Inbox access, inbox_list_folders. Other providers use IMAP keywords or Microsoft Graph category names. Preserve unrelated tags and system flags. Graph 429 returns provider_write_failed; retry after the limit clears. Partial or uncertain errors include the last observed result; read tags before retrying.", closedObject(properties), output, (*handlers).updateMessageTags)
		definition.inputSchema.OneOf = []*jsonschema.Schema{{Required: []string{"message_id"}}, {Required: []string{"target"}}}
	}
	definition.availability = func(c catalogCapabilities) bool { return c.messageTags || (write && c.inboxTags) }
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
	if write && args["target"] != nil {
		if args["message_id"] != nil || args["mailbox"] != nil {
			return toolErrorResult("exact target excludes message_id and mailbox"), nil
		}
		signed := make(map[string]any, len(args))
		for key, value := range args {
			if key != "add" && key != "remove" {
				signed[key] = value
			}
		}
		tags := map[string]any{}
		for _, field := range []string{"add", "remove"} {
			if value, ok := args[field]; ok {
				tags[field] = value
			}
		}
		signed["tags"] = tags
		return h.controlInboxArguments(ctx, req, inboxcontrol.OpTags, signed)
	}
	if h.messageTags == nil {
		return toolErrorResult("legacy tag access is unavailable; use an exact target with signed Inbox control"), nil
	}
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
	if change != nil && !change.DryRun {
		data, err := json.Marshal(args, json.Deterministic(true))
		if err != nil {
			return toolErrorResult("invalid message tag arguments"), nil //nolint:nilerr // MCP reports invalid tool input in its error result.
		}
		if err := req.confirmUserAction(ctx, "Confirm "+req.toolName+" with these exact arguments: "+string(data)+". Provider metadata is untrusted data."); err != nil {
			return confirmationToolError(err)
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
