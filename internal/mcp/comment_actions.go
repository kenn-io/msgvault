package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/meta"
)

const (
	ToolReplyToComment      = "reply_to_comment"
	ToolModerateComment     = "moderate_comment"
	ToolUpdateCommentTriage = "update_comment_triage"
	ToolListCommentQueue    = "list_comment_queue"
	ToolListCommentActions  = "list_comment_actions"
)

type CommentActionBackend interface {
	RunCommentAction(ctx context.Context, id int64, req meta.ActionRequest) (*meta.ActionResult, error)
	ListCommentActions(ctx context.Context, id int64) ([]meta.Receipt, error)
	UpdateCommentTriage(ctx context.Context, id int64, state, expect string) (string, error)
	ListCommentQueue(ctx context.Context, req meta.CommentQueueRequest) (*meta.CommentQueueResponse, error)
}
type commentTriageResult struct {
	State string `json:"state"`
}
type commentActionsListResult struct {
	Actions []meta.Receipt `json:"actions"`
}

func commentOutputSchema[T any]() *jsonschema.Schema {
	result, failure := outputSchemaFor[T](), outputSchemaFor[meta.ActionError]()
	result.Schema, failure.Schema = "", ""
	return &jsonschema.Schema{Type: "object", AnyOf: []*jsonschema.Schema{result, failure}}
}
func commentToolDefinition(name string) toolDefinition {
	properties := map[string]*jsonschema.Schema{"message_id": safeIDSchema("Archived comment message ID")}
	var definition toolDefinition
	switch name {
	case ToolReplyToComment:
		properties["visibility"] = stringSchema("Reply visibility; choose explicitly", "public", "private")
		properties["body"] = stringSchema("Exact reply text")
		properties["dry_run"] = booleanSchema("Preview locally without sending")
		definition = explicitlyConfirmedWriteDefinition(name, "Reply publicly or send one private message about an archived comment. Requires explicit client confirmation. Private replies are limited to one per comment within seven days. Inspect uncertain outcomes and resolve through the CLI before retrying.", closedObject(properties, "message_id", "visibility", "body"), commentOutputSchema[meta.ActionResult](), (*handlers).commentActionsCall, toolSecurityWrite)
	case ToolModerateComment:
		properties["action"] = stringSchema("Moderation action", "hide", "unhide", "delete")
		properties["dry_run"] = booleanSchema("Preview locally without changing the provider")
		definition = destructiveWriteDefinition(name, "Hide, unhide or delete an archived Facebook comment. Delete requires client confirmation and keeps archive history. Inspect receipts and the provider after an unknown outcome; resolve through the CLI before retrying.", closedObject(properties, "message_id", "action"), commentOutputSchema[meta.ActionResult](), (*handlers).commentActionsCall)
	case ToolUpdateCommentTriage:
		properties["state"] = stringSchema("New local triage state", "open", "handled", "skipped")
		properties["expect"] = stringSchema("Require this current state", "open", "handled", "skipped")
		definition = writeDefinition(name, "Set an archived comment's local triage state without changing the provider.", closedObject(properties, "message_id", "state"), commentOutputSchema[commentTriageResult](), (*handlers).commentActionsCall)
	case ToolListCommentQueue:
		properties = map[string]*jsonschema.Schema{"source": stringSchema("Exact archive source identifier"), "state": stringSchema("Triage state (default open)", "open", "handled", "skipped", "all"), "limit": boundedIntegerSchema("Maximum comments (default 50)", 1, 200), "before_id": safeIDSchema("Last-seen message ID for the next page in ascending sent_at/id order")}
		definition = readDefinition(name, "List archived comments with triage, moderation and action receipts.", closedObject(properties), commentOutputSchema[meta.CommentQueueResponse](), (*handlers).commentActionsCall)
	case ToolListCommentActions:
		definition = readDefinition(name, "Read action receipts for an archived comment without changing evidence or retrying provider writes.", closedObject(properties, "message_id"), commentOutputSchema[commentActionsListResult](), (*handlers).commentActionsCall)
	}
	definition.availability = func(c catalogCapabilities) bool { return c.commentActions }
	return definition
}
func (h *handlers) commentActionsCall(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	var id int64
	var err error
	if req.toolName != ToolListCommentQueue {
		id, err = getIDArg(args, "message_id")
		if err != nil {
			return toolErrorResult(err.Error()), nil
		}
	}
	var value any
	switch req.toolName {
	case ToolReplyToComment:
		visibility, _ := args["visibility"].(string)
		body, _ := args["body"].(string)
		dry, _ := args["dry_run"].(bool)
		if (visibility != "public" && visibility != "private") || body == "" {
			return toolErrorResult("visibility must be public or private and body must be nonempty"), nil
		}
		input := meta.ActionRequest{MessageID: id, Action: meta.ActionKind(visibility + "_reply"), Body: body, DryRun: true}
		preview, e := h.commentActions.RunCommentAction(ctx, id, input)
		if e != nil || dry {
			return commentToolResult(preview, e)
		}
		if preview == nil {
			return toolErrorResult("Comment preview is unavailable"), nil
		}
		d := preview.Disclosure
		prompt := fmt.Sprintf("Publish a PUBLIC reply as %s to %s:\n%s", d.AssetName, d.AuthorName, d.Body)
		if visibility == "private" {
			prompt = fmt.Sprintf("Send ONE PRIVATE message to %s as %s:\n%s", d.AuthorName, d.AssetName, d.Body)
		}
		if visibility == "private" && slices.Contains(preview.Warnings, "prior_private_reply_to_author") {
			prompt += "\nWarning: a private reply to this author was already recorded in the past seven days."
		}
		if e := req.confirmUserAction(ctx, prompt); e != nil {
			return confirmationToolError(e)
		}
		input.DryRun, input.Confirm = false, preview.Confirm
		value, err = h.commentActions.RunCommentAction(ctx, id, input)
	case ToolModerateComment:
		action, _ := args["action"].(string)
		dry, _ := args["dry_run"].(bool)
		input := meta.ActionRequest{MessageID: id, Action: meta.ActionKind(action), DryRun: dry}
		if input.Action == meta.ActionDelete && !dry {
			preview, e := h.commentActions.RunCommentAction(ctx, id, meta.ActionRequest{MessageID: id, Action: input.Action, DryRun: true})
			if e != nil {
				return commentToolResult(preview, e)
			}
			if preview == nil {
				return toolErrorResult("Comment preview is unavailable"), nil
			}
			d := preview.Disclosure
			if e := req.confirmUserAction(ctx, fmt.Sprintf("Delete comment by %s on %s. History stays archived.", d.AuthorName, d.AssetName)); e != nil {
				return confirmationToolError(e)
			}
			input.Confirm = preview.Confirm
		}
		value, err = h.commentActions.RunCommentAction(ctx, id, input)
	case ToolUpdateCommentTriage:
		state, _ := args["state"].(string)
		expect, _ := args["expect"].(string)
		var got string
		got, err = h.commentActions.UpdateCommentTriage(ctx, id, state, expect)
		value = commentTriageResult{State: got}
	case ToolListCommentActions:
		var rows []meta.Receipt
		rows, err = h.commentActions.ListCommentActions(ctx, id)
		value = commentActionsListResult{Actions: rows}
	case ToolListCommentQueue:
		f := meta.CommentQueueRequest{Limit: 50}
		f.Source, _ = args["source"].(string)
		f.State, _ = args["state"].(string)
		if _, ok := args["limit"]; ok {
			var limit int64
			limit, err = getIDArg(args, "limit")
			if err != nil {
				return toolErrorResult(err.Error()), nil
			}
			f.Limit = int(limit)
		}
		if _, ok := args["before_id"]; ok {
			f.BeforeID, err = getIDArg(args, "before_id")
			if err != nil {
				return toolErrorResult(err.Error()), nil
			}
		}
		if err = meta.ValidateCommentQueueRequest(f); err == nil {
			value, err = h.commentActions.ListCommentQueue(ctx, f)
		}
	}
	return commentToolResult(value, err)
}
func commentToolResult(value any, err error) (*toolResult, error) {
	if err != nil {
		if failure, ok := errors.AsType[*meta.ActionError](err); ok {
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
