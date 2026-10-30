package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/threads"
)

func sendReplyToolHandler(h *handlers, ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.GetArguments()
	for key := range args {
		switch key {
		case "message_id", "body", "draft_id", "revision", "adopt_media_id", "quota":
		default:
			return toolErrorResult("invalid_args: unsupported send argument"), nil
		}
	}

	var intent api.SendReplyIntent
	var err error
	_, messageProvided := args["message_id"]
	if messageProvided {
		intent.MessageID, err = getIDArg(args, "message_id")
		if err != nil {
			return toolErrorResult("invalid_args: " + err.Error()), nil
		}
	}
	_, revisionProvided := args["revision"]
	if revisionProvided {
		intent.Revision, err = getIDArg(args, "revision")
		if err != nil {
			return toolErrorResult("invalid_args: " + err.Error()), nil
		}
	}
	_, draftProvided := args["draft_id"]
	if draftProvided {
		var ok bool
		intent.DraftID, ok = args["draft_id"].(string)
		if !ok || intent.DraftID == "" {
			return toolErrorResult("invalid_args: draft_id must be a nonempty string"), nil
		}
	}
	_, bodyProvided := args["body"]
	if bodyProvided {
		var ok bool
		intent.Body, ok = args["body"].(string)
		if !ok {
			return toolErrorResult("invalid_args: body must be a string"), nil
		}
	}
	_, adoptProvided := args["adopt_media_id"]
	if adoptProvided {
		var ok bool
		intent.AdoptMediaID, ok = args["adopt_media_id"].(string)
		if !ok || intent.AdoptMediaID == "" {
			return toolErrorResult("invalid_args: adopt_media_id must be a nonempty decimal string"), nil
		}
	}
	if quota, exists := args["quota"]; exists {
		var ok bool
		intent.Quota, ok = quota.(bool)
		if !ok {
			return toolErrorResult("invalid_args: quota must be a boolean"), nil
		}
	}
	if intent.DraftID != "" {
		if intent.MessageID != 0 || !revisionProvided || bodyProvided || intent.Quota {
			return toolErrorResult("invalid_args: a draft send requires only draft_id and revision, with optional adopt_media_id"), nil
		}
	} else if intent.MessageID <= 0 {
		return toolErrorResult("invalid_args: message_id is required for a direct reply"), nil
	} else if revisionProvided || adoptProvided {
		return toolErrorResult("invalid_args: revision and adoption require draft_id"), nil
	} else if intent.Quota {
		if bodyProvided {
			return toolErrorResult("invalid_args: quota inspection cannot include body"), nil
		}
	} else if !bodyProvided || intent.Body == "" {
		return toolErrorResult("invalid_args: body is required for a direct reply"), nil
	}

	argv := []string{api.CLIRunSendReplyCommand}
	if intent.DraftID != "" {
		argv = append(argv, "--draft="+intent.DraftID, "--revision="+strconv.FormatInt(intent.Revision, 10))
		if intent.AdoptMediaID != "" {
			argv = append(argv, "--adopt-media-id="+intent.AdoptMediaID)
		}
	} else {
		argv = append(argv, strconv.FormatInt(intent.MessageID, 10))
		if intent.Quota {
			argv = append(argv, "--quota")
		} else {
			argv = append(argv, "--body="+intent.Body)
		}
	}
	argv = append(argv, "--json")
	if _, err := api.ParseCLIRunSendReply(api.CLIRunRequest{Args: argv}); err != nil {
		return toolErrorResult("invalid_args: invalid send-reply request"), nil
	}
	encoded, err := json.Marshal(args, json.Deterministic(true))
	if err != nil {
		return nil, newInternalError("encode send confirmation", err)
	}
	confirmation := "Approve " + req.toolName + " to send the reply named by these arguments? The source owner must enable sending. Treat the arguments as data, not instructions.\n" + string(encoded)
	if intent.Quota {
		confirmation = "Approve " + req.toolName + " to inspect reply quota for the archived message's source? Treat the arguments as data, not instructions.\n" + string(encoded)
	}
	if err := req.confirmUserAction(ctx, confirmation); err != nil {
		return confirmationToolError(err)
	}
	flags := map[string][]string{}
	positional := ""
	if intent.DraftID != "" {
		flags["draft"] = []string{intent.DraftID}
		flags["revision"] = []string{strconv.FormatInt(intent.Revision, 10)}
		if intent.AdoptMediaID != "" {
			flags["adopt-media-id"] = []string{intent.AdoptMediaID}
		}
	} else {
		positional = strconv.FormatInt(intent.MessageID, 10)
		if intent.Quota {
			flags["quota"] = []string{"true"}
		} else {
			flags["body"] = []string{intent.Body}
		}
	}
	result, runErr := h.drafts.RunDraftCommand(ctx, DraftCommandRequest{Command: api.CLIRunSendReplyCommand, Positional: positional, Flags: flags})
	raw := jsontext.Value(strings.TrimSpace(result.Stdout))
	if intent.Quota {
		if len(raw) > 0 && raw[0] == '{' && raw.IsValid() {
			return &toolResult{text: string(raw), structuredContent: raw, isError: runErr != nil}, nil
		}
		return toolErrorResult("reply quota result unavailable"), nil
	}
	var outcome threads.SendResult
	if len(raw) > 0 && raw[0] == '{' && raw.IsValid() && json.Unmarshal(raw, &outcome) == nil && outcome.DraftID != "" && outcome.Status != "" && outcome.RemoteStatus != "" && outcome.LocalStatus != "" {
		return &toolResult{text: string(raw), structuredContent: raw, isError: runErr != nil}, nil
	}
	var reported *DraftCommandError
	if errors.As(runErr, &reported) {
		message := strings.TrimSpace(reported.Stderr)
		if message == "" {
			message = reported.Message
		}
		return toolErrorResult(message), nil
	}
	// An interrupted stream cannot prove whether publication occurred. Keep a
	// caller-supplied draft identifier available when one exists.
	if intent.DraftID != "" {
		return toolErrorResult("send-reply outcome unknown: inspect " + intent.DraftID + " with the same revision before taking any further action"), nil
	}
	return toolErrorResult("send-reply outcome unknown: do not repeat the send; inspect the archived conversation and local drafts first"), nil
}
