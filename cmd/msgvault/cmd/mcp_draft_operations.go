package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

const (
	mcpDraftSourceIDFlag = "source-id"
	mcpDraftBCCFlag      = "bcc"
)

var mcpDraftCommands = map[string]string{
	"draft_reply": "draft-reply", "draft_compose": "draft-compose", "draft_forward": "draft-forward", "get_draft": "draft-get", "list_conversation_drafts": "draft-get", "edit_draft": "draft-edit", "delete_draft": "draft-delete", "recover_draft": "draft-recover", "list_draft_send_as": "draft-send-as",
}

func (b *daemonMCPOperations) draftDescriptor(name string) (apiprotocol.MCPCommandDescriptor, bool) {
	for _, d := range b.commands {
		if d.Name == mcpDraftCommands[name] {
			return d, true
		}
	}
	return apiprotocol.MCPCommandDescriptor{}, false
}

func (b *daemonMCPOperations) SupportsConversationDrafts() bool {
	d, ok := b.draftDescriptor("draft_compose")
	return ok && slices.Contains(d.Flags, "conversation") && slices.Contains(d.Flags, "reply-to")
}

func draftMCPCapabilities(capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	// Fixed ordering keeps discovery deterministic without admitting arbitrary CLI names.
	for _, name := range []string{"draft_reply", "draft_compose", "draft_forward", "get_draft", "list_conversation_drafts", "edit_draft", "delete_draft", "recover_draft", "list_draft_send_as"} {
		for _, d := range capabilities.Commands {
			if d.Name != mcpDraftCommands[name] || !slices.Contains(d.Flags, "json") {
				continue
			}
			if capabilities.Delegated && (!d.Delegated || name == "draft_forward" || name == "list_draft_send_as") {
				continue
			}
			required := []string{}
			switch name {
			case "draft_reply":
				required = []string{"body", "from", "all", "account", mcpDraftSourceIDFlag}
			case "draft_compose":
				required = []string{"body", "to", "cc", mcpDraftBCCFlag, "subject", "from", "account", mcpDraftSourceIDFlag}
			case "draft_forward":
				required = []string{"body", "to", "cc", mcpDraftBCCFlag, "from", "account", mcpDraftSourceIDFlag}
			case "list_conversation_drafts":
				required = []string{"conversation"}
			case "edit_draft":
				required = []string{"body", "revision"}
			case "delete_draft", "recover_draft":
				required = []string{"revision"}
			}
			if slices.ContainsFunc(required, func(flag string) bool { return !slices.Contains(d.Flags, flag) }) {
				continue
			}
			names = append(names, name)
			break
		}
	}
	return names
}

type mcpDraftInput struct {
	MessageID        *int64   `json:"message_id,omitempty"`
	DraftID          *string  `json:"draft_id,omitempty"`
	Revision         *int64   `json:"revision,omitempty"`
	Account          *string  `json:"account,omitempty"`
	SourceID         *int64   `json:"source_id,omitempty"`
	From             *string  `json:"from,omitempty"`
	Body             *string  `json:"body,omitempty"`
	ReplyAll         *bool    `json:"reply_all,omitempty"`
	To               []string `json:"to,omitempty"`
	Cc               []string `json:"cc,omitempty"`
	Bcc              []string `json:"bcc,omitempty"`
	Subject          *string  `json:"subject,omitempty"`
	ConversationID   *int64   `json:"conversation_id,omitempty"`
	ReplyToMessageID *int64   `json:"reply_to_message_id,omitempty"`
}

func mcpDraftArguments(name string, args map[string]any, descriptor apiprotocol.MCPCommandDescriptor) ([]string, error) {
	command, known := mcpDraftCommands[name]
	if !known || command != descriptor.Name {
		return nil, errors.New("unsupported draft operation")
	}
	data, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	var input mcpDraftInput
	if err := json.Unmarshal(data, &input, json.RejectUnknownMembers(true)); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	allow := func(keys ...string) {
		for _, key := range keys {
			allowed[key] = true
		}
	}
	switch name {
	case "draft_reply":
		allow("message_id", "body", "source_id", "account", "from", "reply_all")
	case "draft_compose":
		allow("body", "source_id", "account", "from", "to", "cc", mcpDraftBCCFlag, "subject")
		if slices.Contains(descriptor.Flags, "conversation") && slices.Contains(descriptor.Flags, "reply-to") {
			allow("conversation_id", "reply_to_message_id")
		}
	case "draft_forward":
		allow("message_id", "body", "source_id", "account", "from", "to", "cc", mcpDraftBCCFlag)
	case "get_draft":
		allow("draft_id")
	case "list_conversation_drafts":
		allow("conversation_id")
	case "list_draft_send_as":
		allow("account")
	case "edit_draft":
		allow("draft_id", "revision", "body")
	case "delete_draft", "recover_draft":
		allow("draft_id", "revision")
	}
	for key, value := range args {
		if !allowed[key] || value == nil {
			return nil, errors.New("invalid draft argument")
		}
	}
	out := []string{command}
	positive := func(value *int64) bool { return value != nil && *value > 0 && *value <= 9007199254740991 }
	positional := func(value *string) bool {
		return value != nil && strings.TrimSpace(*value) != "" && !strings.HasPrefix(*value, "-") && !strings.ContainsAny(*value, "\x00\r\n") && utf8.ValidString(*value)
	}
	switch name {
	case "draft_reply", "draft_forward":
		if !positive(input.MessageID) {
			return nil, errors.New("message ID required")
		}
		out = append(out, strconv.FormatInt(*input.MessageID, 10))
	case "get_draft", "edit_draft", "delete_draft", "recover_draft":
		if !positional(input.DraftID) {
			return nil, errors.New("draft ID required")
		}
		out = append(out, *input.DraftID)
	case "list_draft_send_as":
		if !positional(input.Account) {
			return nil, errors.New("account required")
		}
		out = append(out, *input.Account)
	case "list_conversation_drafts":
		if !positive(input.ConversationID) {
			return nil, errors.New("conversation ID required")
		}
	}
	if name == "draft_reply" || name == "draft_compose" || name == "draft_forward" || name == "edit_draft" {
		if input.Body == nil {
			return nil, errors.New("explicit body required")
		}
	}
	if input.Account != nil && input.SourceID != nil {
		return nil, errors.New("source selectors are mutually exclusive")
	}
	if input.ReplyToMessageID != nil && input.ConversationID == nil {
		return nil, errors.New("reply target requires conversation")
	}
	if name == "edit_draft" || name == "delete_draft" || name == "recover_draft" {
		if !positive(input.Revision) {
			return nil, errors.New("positive revision required")
		}
	}
	add := func(flag, value string) error {
		if !slices.Contains(descriptor.Flags, flag) || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return errors.New("unsupported or invalid draft field")
		}
		out = append(out, "--"+flag+"="+value)
		return nil
	}
	for _, item := range []struct {
		flag  string
		value *int64
	}{{"revision", input.Revision}, {mcpDraftSourceIDFlag, input.SourceID}, {"conversation", input.ConversationID}, {"reply-to", input.ReplyToMessageID}} {
		if item.value != nil {
			if !positive(item.value) {
				return nil, errors.New("positive ID required")
			}
			if err := add(item.flag, strconv.FormatInt(*item.value, 10)); err != nil {
				return nil, err
			}
		}
	}
	for _, item := range []struct {
		flag  string
		value *string
	}{{"account", input.Account}, {"from", input.From}, {"body", input.Body}, {"subject", input.Subject}} {
		if item.value == nil || (name == "list_draft_send_as" && item.flag == "account") {
			continue
		}
		if (item.flag == "account" || item.flag == "from") && strings.TrimSpace(*item.value) == "" {
			return nil, errors.New("empty selector")
		}
		if err := add(item.flag, *item.value); err != nil {
			return nil, err
		}
	}
	for _, item := range []struct {
		flag   string
		values []string
	}{{"to", input.To}, {"cc", input.Cc}, {mcpDraftBCCFlag, input.Bcc}} {
		for _, value := range item.values {
			if strings.TrimSpace(value) == "" {
				return nil, errors.New("empty recipient")
			}
			if err := add(item.flag, value); err != nil {
				return nil, err
			}
		}
	}
	if input.ReplyAll != nil && *input.ReplyAll {
		if err := add("all", "true"); err != nil {
			return nil, err
		}
	}
	if err := add("json", "true"); err != nil {
		return nil, err
	}
	return out, nil
}

func (b *daemonMCPOperations) executeDraftOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if _, known := mcpDraftCommands[name]; !known {
		return nil, false, nil
	}
	descriptor, ok := b.draftDescriptor(name)
	if !ok {
		return operationFailure("operation_not_supported", false), true, nil
	}
	argv, err := mcpDraftArguments(name, args, descriptor)
	if err != nil {
		return operationFailure("invalid_args", false), true, nil
	}
	stream, runErr := b.client.RunMCPCLICommand(ctx, argv, "")
	if stream == nil {
		if coded, ok := errors.AsType[*daemonclient.MCPCLIError](runErr); ok && draftClearRefusal(coded.Code) {
			return operationFailure(coded.Code, false), true, nil
		}
		return operationFailure("draft_execution_failed", draftWrites(name)), true, runErr
	}
	result, decodeErr := decodeMCPDraftResult(name, stream)
	// The strict stream owns completion. Even a valid receipt never turns a
	// failed/truncated stream into success, and its private error stays private.
	if runErr != nil && result != nil && !result.IsError {
		return operationFailure("draft_execution_failed", draftWrites(name)), true, runErr
	}
	return result, true, decodeErr
}

func draftWrites(name string) bool {
	return name != "get_draft" && name != "list_conversation_drafts" && name != "list_draft_send_as"
}
func draftClearRefusal(code string) bool {
	switch code {
	case "invalid_args", "invalid_request", "command_not_allowed", "not_permitted", "unauthorized", "operation_in_progress", "draft_disabled", "invalid_mailbox", "invalid_from", "from_ambiguous", "invalid_parent", "invalid_source", "invalid_reply_metadata", "invalid_compose_metadata", "draft_not_found", "draft_discarded", "revision_conflict", "revision_mismatch", "draft_conflict", "draft_pending", "pending_operation", "insufficient_scope", "not_supported", "invalid_draft", "invalid_state", "invalid_message", "uidplus_required", "attachment_preflight_failed", "chat_not_found", "draft_exists", "invalid_destination", "invalid_forward_metadata", "unsupported_source":
		return true
	}
	return false
}

func decodeMCPDraftResult(name string, stream *daemonclient.MCPCLIResult) (*mcpserver.OperationResult, error) {
	if stream == nil {
		return operationFailure("draft_execution_failed", draftWrites(name)), nil
	}
	if stream.Failed {
		uncertain := draftWrites(name) && stream.OperationMayHaveCompleted && !draftClearRefusal(stream.ErrorCode)
		// Only a completed error event may carry the producer's typed failure receipt.
		switch stream.ErrorCode {
		case "invalid_cli_stream", "output_limit_exceeded", "cli_execution_failed":
			return operationFailure("draft_execution_failed", uncertain), nil
		}
		var receipt mcpserver.DraftOutput
		if json.Unmarshal([]byte(stream.Stderr), &receipt, json.RejectUnknownMembers(true)) == nil && validMCPDraftOutput(receipt, true) {
			var output map[string]any
			_ = json.Unmarshal([]byte(stream.Stderr), &output)
			return &mcpserver.OperationResult{IsError: true, Output: struct {
				Error                     string         `json:"error"`
				OperationMayHaveCompleted bool           `json:"operation_may_have_completed"`
				Draft                     map[string]any `json:"draft"`
			}{stream.ErrorCode, uncertain, output}}, nil
		}
		return operationFailure(stream.ErrorCode, uncertain), nil
	}
	if name == "list_draft_send_as" {
		var output mcpserver.DraftSendAsOutput
		if err := json.Unmarshal([]byte(stream.Stdout), &output, json.RejectUnknownMembers(true)); err != nil || output.SourceID <= 0 || strings.TrimSpace(output.Account) == "" || output.Entries == nil {
			return operationFailure("invalid_draft_response", false), err
		}
		return &mcpserver.OperationResult{Output: output}, nil
	}
	if name == "list_conversation_drafts" {
		var drafts []mcpserver.DraftOutput
		if err := json.Unmarshal([]byte(stream.Stdout), &drafts, json.RejectUnknownMembers(true)); err != nil || drafts == nil {
			return operationFailure("invalid_draft_response", false), err
		}
		for _, draft := range drafts {
			if !validMCPDraftOutput(draft, false) || draft.Location != "msgvault" {
				return operationFailure("invalid_draft_response", false), nil
			}
		}
		var output []map[string]any
		_ = json.Unmarshal([]byte(stream.Stdout), &output)
		return &mcpserver.OperationResult{Output: struct {
			Drafts []map[string]any `json:"drafts"`
		}{output}}, nil
	}
	var draft mcpserver.DraftOutput
	if err := json.Unmarshal([]byte(stream.Stdout), &draft, json.RejectUnknownMembers(true)); err != nil || !validMCPDraftOutput(draft, false) {
		return operationFailure("invalid_draft_response", draftWrites(name)), err
	}
	var output map[string]any
	_ = json.Unmarshal([]byte(stream.Stdout), &output)
	if draft.ChatID != "" {
		if _, present := output["content"]; !present {
			return operationFailure("invalid_draft_response", draftWrites(name)), nil
		}
	}
	return &mcpserver.OperationResult{Output: output}, nil
}

func validMCPDraftOutput(draft mcpserver.DraftOutput, failed bool) bool {
	if draft.SourceID <= 0 || draft.Status == "" {
		return false
	}
	// A provider may have accepted creation before a local ID could be saved.
	if failed && draft.OperationRef != "" && draft.RFC822MessageID != "" {
		return true
	}
	if draft.DraftID == "" || draft.Revision <= 0 {
		return false
	}
	if draft.Location != "" {
		return draft.Location == "msgvault" && draft.Body != nil && draft.ConversationID > 0 && draft.SourceType != "" && draft.Source != "" && draft.SourceConversationID != "" && draft.ConversationType != ""
	}
	if draft.Lifecycle != "" && draft.Lifecycle != "active" && draft.Lifecycle != "discarded" {
		return false
	}
	if draft.ChatID != "" {
		return draft.Lifecycle != ""
	}
	if draft.Receipt != nil {
		if draft.Lifecycle == "" || draft.MessageID <= 0 {
			return false
		}
		if draft.Provider == "gmail" {
			return draft.Receipt.GmailDraftID != "" && draft.Receipt.GmailMessageID != ""
		}
		return draft.Provider == "" && draft.Receipt.Mailbox != "" && draft.Receipt.UID > 0 && draft.Receipt.UIDValidity > 0
	}
	if draft.OperationRef == "" || draft.RFC822MessageID == "" {
		return false
	}
	return (draft.Mailbox != "" && draft.UID > 0 && draft.UIDValidity > 0) || (draft.GmailDraftID != "" && draft.GmailMessageID != "")
}

func (b *daemonMCPOperations) draftOperationDisclosure(name string, args map[string]any) (string, bool, error) {
	if _, known := mcpDraftCommands[name]; !known {
		return "", false, nil
	}
	descriptor, ok := b.draftDescriptor(name)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_not_supported"}
	}
	if _, err := mcpDraftArguments(name, args, descriptor); err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_args"}
	}
	data, err := json.Marshal(args, json.Deterministic(true))
	if err != nil {
		return "", true, err
	}
	return fmt.Sprintf("Approve %s with these exact fields: %s. The daemon checks source, sender, grants and revision before changing a draft. A provider draft write may have an uncertain outcome; this tool never retries or sends the message. Local drafts stay in msgvault; native Beeper stages composer text for the user to send and has a read-check/write race.", name, data), true, nil
}
