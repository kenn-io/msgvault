package cmd

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/sourceops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

type chatDraftIntent struct {
	Operation      string
	ConversationID int64
	DraftID        string
	Source         string
	SourceID       int64
	ReplyToMessage int64
	Revision       int64
	Body           string
	JSON           bool
	BodySet        bool
	SourceSet      bool
	SourceIDSet    bool
	ReplyToSet     bool
	RevisionSet    bool
}

type chatDraftOutput struct {
	Status                 string `json:"status"`
	Location               string `json:"location"`
	DraftID                string `json:"draft_id"`
	Revision               int64  `json:"revision"`
	Body                   string `json:"body"`
	SourceID               int64  `json:"source_id"`
	SourceType             string `json:"source_type"`
	Source                 string `json:"source"`
	ConversationID         int64  `json:"conversation_id"`
	SourceConversationID   string `json:"source_conversation_id"`
	ConversationType       string `json:"conversation_type"`
	ReplyToSourceMessageID string `json:"reply_to_source_message_id,omitempty"`
}

func invalidChatDraftArgs(format string, args ...any) error {
	return chatDraftError("invalid_args", fmt.Errorf(format, args...))
}

func chatDraftError(code string, cause error) error {
	return &api.CLIRunCodedError{Code: code, Err: cause}
}

func parseChatDraftArgs(args []string) (chatDraftIntent, error) {
	if !api.IsCLIRunChatDraft(args) {
		return chatDraftIntent{}, invalidChatDraftArgs("unknown chat draft command")
	}
	intent := chatDraftIntent{Operation: args[0]}
	var positional string
	var jsonSet bool
	rest := args[1:]
	for len(rest) > 0 {
		arg := rest[0]
		rest = rest[1:]
		nameValue, isFlag := strings.CutPrefix(arg, "--")
		if !isFlag {
			if positional != "" {
				return chatDraftIntent{}, invalidChatDraftArgs("expected one positional identifier")
			}
			positional = arg
			continue
		}
		name, value, hasValue := strings.Cut(nameValue, "=")
		switch name {
		case "source", "source-id", "body", "reply-to", "revision":
			if !hasValue {
				if len(rest) == 0 {
					return chatDraftIntent{}, invalidChatDraftArgs("--%s requires a value", name)
				}
				value, rest = rest[0], rest[1:]
			}
			switch name {
			case "source":
				if intent.SourceSet || strings.TrimSpace(value) == "" {
					return chatDraftIntent{}, invalidChatDraftArgs("--source must be given once with a value")
				}
				intent.Source, intent.SourceSet = value, true
			case "source-id":
				if intent.SourceIDSet {
					return chatDraftIntent{}, invalidChatDraftArgs("--source-id given more than once")
				}
				id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				if err != nil || id <= 0 {
					return chatDraftIntent{}, invalidChatDraftArgs("--source-id must be a positive integer")
				}
				intent.SourceID, intent.SourceIDSet = id, true
			case "body":
				if intent.BodySet {
					return chatDraftIntent{}, invalidChatDraftArgs("--body given more than once")
				}
				intent.Body, intent.BodySet = value, true
			case "reply-to":
				if intent.ReplyToSet {
					return chatDraftIntent{}, invalidChatDraftArgs("--reply-to given more than once")
				}
				id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				if err != nil || id <= 0 {
					return chatDraftIntent{}, invalidChatDraftArgs("--reply-to must be a positive integer")
				}
				intent.ReplyToMessage, intent.ReplyToSet = id, true
			case "revision":
				if intent.RevisionSet {
					return chatDraftIntent{}, invalidChatDraftArgs("--revision given more than once")
				}
				revision, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
				if err != nil || revision <= 0 {
					return chatDraftIntent{}, invalidChatDraftArgs("--revision must be a positive integer")
				}
				intent.Revision, intent.RevisionSet = revision, true
			}
		case "json":
			if jsonSet || (hasValue && value != "true" && value != "false") {
				return chatDraftIntent{}, invalidChatDraftArgs("--json requires a boolean value")
			}
			intent.JSON, jsonSet = !hasValue || value == "true", true
		case "log-level", "log-sql-slow-ms":
			if !hasValue {
				if len(rest) == 0 {
					return chatDraftIntent{}, invalidChatDraftArgs("--%s requires a value", name)
				}
				rest = rest[1:]
			}
		case "verbose", "log-sql":
			// Global logging flags are already handled by Cobra.
		default:
			return chatDraftIntent{}, invalidChatDraftArgs("unknown flag --%s", name)
		}
	}

	switch intent.Operation {
	case api.CLIRunChatDraftCreateCommand:
		conversationID, err := parsePositiveChatDraftID(positional, "conversation ID")
		if err != nil {
			return chatDraftIntent{}, err
		}
		intent.ConversationID = conversationID
		if intent.SourceSet == intent.SourceIDSet || !intent.BodySet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-create requires --body and one of --source or --source-id")
		}
		if intent.RevisionSet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-create does not accept --revision")
		}
	case api.CLIRunChatDraftGetCommand:
		if intent.SourceSet || intent.SourceIDSet || intent.BodySet || intent.ReplyToSet || intent.RevisionSet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-get accepts only --json")
		}
		if !validChatDraftTextID(positional) {
			return chatDraftIntent{}, invalidChatDraftArgs("draft ID is required")
		}
		intent.DraftID = positional
	case api.CLIRunChatDraftEditCommand:
		if !intent.RevisionSet || !intent.BodySet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-edit requires --revision and --body")
		}
		if intent.SourceSet || intent.SourceIDSet || intent.ReplyToSet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-edit accepts only --revision, --body, and --json")
		}
		if !validChatDraftTextID(positional) {
			return chatDraftIntent{}, invalidChatDraftArgs("draft ID is required")
		}
		intent.DraftID = positional
	case api.CLIRunChatDraftDeleteCommand:
		if !intent.RevisionSet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-delete requires --revision")
		}
		if intent.SourceSet || intent.SourceIDSet || intent.BodySet || intent.ReplyToSet {
			return chatDraftIntent{}, invalidChatDraftArgs("chat-draft-delete accepts only --revision and --json")
		}
		if !validChatDraftTextID(positional) {
			return chatDraftIntent{}, invalidChatDraftArgs("draft ID is required")
		}
		intent.DraftID = positional
	}
	return intent, nil
}

func parsePositiveChatDraftID(value, label string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || id <= 0 {
		return 0, invalidChatDraftArgs("%s must be a positive integer", label)
	}
	return id, nil
}

func validChatDraftTextID(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\x00\r\n")
}

func (a *storeAPIAdapter) runCLIChatDraft(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	if len(req.Env) != 0 || req.Cwd != "" {
		return chatDraftError("invalid_args", errors.New("chat draft commands accept no environment or working directory"))
	}
	intent, err := parseChatDraftArgs(req.Args)
	if err != nil {
		return err
	}

	if intent.Operation == api.CLIRunChatDraftCreateCommand {
		source, err := sourceops.ResolveExactOne(a.store, sourceops.Selector{
			Account: intent.Source, SourceID: intent.SourceID, SourceIDSet: intent.SourceIDSet,
		})
		if err != nil {
			if req.Grant != nil {
				return chatDraftError("not_permitted", errors.New("source is not available"))
			}
			return chatDraftError("invalid_destination", err)
		}
		if req.Grant != nil && !req.Grant.Allows(
			agentgrant.PermissionDraftCreate,
			agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier},
		) {
			return chatDraftError("not_permitted", errors.New("source is not in grant"))
		}
		draft, err := a.store.CreateChatDraftContext(ctx, store.ChatDraftCreate{
			SourceID:         source.ID,
			SourceType:       source.SourceType,
			SourceIdentifier: source.Identifier,
			ConversationID:   intent.ConversationID,
			ReplyToMessageID: intent.ReplyToMessage,
			Body:             intent.Body,
		})
		if err != nil {
			return chatDraftStoreError(err)
		}
		return emitChatDraftOutput(emit, intent.JSON, "created", source, draft)
	}

	if req.Grant != nil {
		return chatDraftError("not_permitted", errors.New("chat draft lifecycle is owner-only"))
	}
	draft, err := a.store.GetChatDraftContext(ctx, intent.DraftID)
	if err != nil {
		return chatDraftStoreError(err)
	}
	source, err := a.store.GetSourceByIDContext(ctx, draft.SourceID)
	if err != nil {
		return chatDraftError("local_store_failed", err)
	}

	switch intent.Operation {
	case api.CLIRunChatDraftGetCommand:
		return emitChatDraftOutput(emit, intent.JSON, "ok", source, draft)
	case api.CLIRunChatDraftEditCommand:
		draft, err = a.store.UpdateChatDraftContext(ctx, draft.DraftID, intent.Revision, intent.Body)
		if err != nil {
			return chatDraftStoreError(err)
		}
		return emitChatDraftOutput(emit, intent.JSON, "edited", source, draft)
	case api.CLIRunChatDraftDeleteCommand:
		if err := a.store.DeleteChatDraftContext(ctx, draft.DraftID, intent.Revision); err != nil {
			return chatDraftStoreError(err)
		}
		return emitChatDraftOutput(emit, intent.JSON, "deleted", source, draft)
	default:
		return chatDraftError("invalid_args", errors.New("unknown chat draft operation"))
	}
}

func chatDraftStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrChatDraftInvalidInput):
		return chatDraftError("invalid_args", err)
	case errors.Is(err, store.ErrChatDraftUnsupportedSource):
		return chatDraftError("unsupported_source", err)
	case errors.Is(err, store.ErrChatDraftInvalidDestination):
		return chatDraftError("invalid_destination", err)
	case errors.Is(err, store.ErrChatDraftNotFound):
		return chatDraftError("draft_not_found", err)
	case errors.Is(err, store.ErrChatDraftRevisionConflict):
		return chatDraftError("revision_conflict", err)
	default:
		return chatDraftError("local_store_failed", err)
	}
}

func chatDraftOutputValue(status string, source *store.Source, draft store.ChatDraft) chatDraftOutput {
	return chatDraftOutput{
		Status:                 status,
		Location:               draft.Location,
		DraftID:                draft.DraftID,
		Revision:               draft.Revision,
		Body:                   draft.Body,
		SourceID:               draft.SourceID,
		SourceType:             source.SourceType,
		Source:                 source.Identifier,
		ConversationID:         draft.ConversationID,
		SourceConversationID:   draft.SourceConversationID,
		ConversationType:       draft.ConversationType,
		ReplyToSourceMessageID: draft.ReplyToSourceMessageID,
	}
}

func emitChatDraftOutput(
	emit func(api.CLIRunEvent) error,
	asJSON bool,
	status string,
	source *store.Source,
	draft store.ChatDraft,
) error {
	if emit == nil {
		return nil
	}
	output := chatDraftOutputValue(status, source, draft)
	if asJSON {
		data, err := jsonv2.Marshal(output)
		if err != nil {
			return chatDraftError("output_failed", err)
		}
		if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: string(data) + "\n"}); err != nil {
			return chatDraftError("output_failed", err)
		}
		return nil
	}
	text := fmt.Sprintf(
		"location=%s draft=%s revision=%d status=%s source=%s source_type=%s conversation=%d conversation_type=%s native_conversation=%s",
		textutil.SanitizeTerminal(output.Location),
		textutil.SanitizeTerminal(output.DraftID),
		output.Revision,
		textutil.SanitizeTerminal(output.Status),
		textutil.SanitizeTerminal(output.Source),
		textutil.SanitizeTerminal(output.SourceType),
		output.ConversationID,
		textutil.SanitizeTerminal(output.ConversationType),
		textutil.SanitizeTerminal(output.SourceConversationID),
	)
	if output.ReplyToSourceMessageID != "" {
		text += " reply_to=" + textutil.SanitizeTerminal(output.ReplyToSourceMessageID)
	}
	text += "\nbody:\n" + textutil.SanitizeTerminalMultiline(output.Body) + "\n"
	if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: text}); err != nil {
		return chatDraftError("output_failed", err)
	}
	return nil
}
