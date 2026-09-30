package cmd

import (
	"context"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

// chatDraftOutput reports a local chat draft. Location is always msgvault:
// the text never reaches the provider's composer.
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

// chatDraftAuthorizer admits the owner, or a grant holding one of permissions
// on the conversation's source.
func chatDraftAuthorizer(grant *agentgrant.Grant, permissions ...agentgrant.Permission) store.ChatDraftAuthorizer {
	return func(sourceType, identifier string) error {
		ref := agentgrant.SourceRef{Type: sourceType, Identifier: identifier}
		if grant == nil || slices.ContainsFunc(permissions, func(p agentgrant.Permission) bool { return grant.Allows(p, ref) }) {
			return nil
		}
		return draftReplyNotPermitted(fmt.Errorf("source %s:%s is not in grant %s", sourceType, identifier, grant.ID))
	}
}

func chatDraftStoreError(err error, grant *agentgrant.Grant) error {
	var coded *api.CLIRunCodedError
	switch {
	case errors.As(err, &coded):
		return err
	case grant != nil && (errors.Is(err, store.ErrChatDraftNotFound) || errors.Is(err, store.ErrChatDraftInvalidDestination)):
		return draftReplyNotPermitted(err)
	case errors.Is(err, store.ErrChatDraftNotFound):
		return draftReplyError("draft_not_found", err)
	case errors.Is(err, store.ErrChatDraftInvalidDestination):
		return draftReplyError("invalid_destination", err)
	case errors.Is(err, store.ErrChatDraftUnsupportedSource):
		return draftReplyError("unsupported_source", err)
	case errors.Is(err, store.ErrChatDraftRevisionConflict):
		return draftReplyError("revision_mismatch", err)
	default:
		return draftReplyError("local_store_failed", err)
	}
}

// invalidChatDraftBody rejects text PostgreSQL cannot store, so both backends agree.
func invalidChatDraftBody(body string) error {
	if strings.ContainsRune(body, 0) || !utf8.ValidString(body) {
		return draftReplyError("invalid_args", errors.New("--body must be valid UTF-8 without NUL"))
	}
	return nil
}

func (a *storeAPIAdapter) runCLIChatDraftCreate(
	ctx context.Context, intent draftComposeIntent, grant *agentgrant.Grant, emit func(api.CLIRunEvent) error,
) error {
	if err := invalidChatDraftBody(intent.Body); err != nil {
		return err
	}
	authorize := chatDraftAuthorizer(grant, agentgrant.PermissionDraftCreate)
	authorized := false
	draft, err := a.store.CreateChatDraftContext(ctx, intent.ConversationID, intent.ReplyTo, intent.Body,
		func(sourceType, identifier string) error {
			err := authorize(sourceType, identifier)
			authorized = err == nil
			return err
		})
	if err != nil {
		if authorized {
			grant = nil // the grant covers this source, so later errors reveal nothing to hide
		}
		return chatDraftStoreError(err, grant)
	}
	return emitChatDrafts(emit, intent.JSON, false, "created", draft)
}

func (a *storeAPIAdapter) runCLIChatDraftLifecycle(
	ctx context.Context, intent draftLifecycleIntent, grant *agentgrant.Grant, emit func(api.CLIRunEvent) error,
) error {
	if intent.Operation == api.CLIRunDraftRecoverCommand {
		return draftReplyError("not_supported", errors.New("local chat drafts have nothing to recover"))
	}
	// Chat drafts carry no sender, so draft.create alone never reads them.
	permissions := slices.DeleteFunc(api.CLIRunDraftLifecyclePermissions(intent.Operation), func(p agentgrant.Permission) bool { return p == agentgrant.PermissionDraftCreate })
	authorize := chatDraftAuthorizer(grant, permissions...)
	if intent.ConversationID != 0 {
		drafts, err := a.store.ListChatDraftsContext(ctx, intent.ConversationID, authorize)
		if err != nil {
			return chatDraftStoreError(err, grant)
		}
		return emitChatDrafts(emit, intent.JSON, true, "ok", drafts...)
	}
	draft, err := a.store.GetChatDraftContext(ctx, intent.DraftID)
	if err == nil {
		err = authorize(draft.SourceType, draft.SourceIdentifier)
	}
	if err != nil {
		return chatDraftStoreError(err, grant)
	}
	status := "ok"
	switch intent.Operation {
	case api.CLIRunDraftEditCommand:
		status = "edited"
		if err := invalidChatDraftBody(intent.Body); err != nil {
			return err
		}
		draft, err = a.store.UpdateChatDraftContext(ctx, draft.DraftID, intent.Revision, intent.Body)
	case api.CLIRunDraftDeleteCommand:
		status = "deleted"
		err = a.store.DeleteChatDraftContext(ctx, draft.DraftID, intent.Revision)
	}
	if err != nil {
		return chatDraftStoreError(err, grant)
	}
	return emitChatDrafts(emit, intent.JSON, false, status, draft)
}

// emitChatDrafts writes one draft, or a list as a JSON array, to stdout.
func emitChatDrafts(emit func(api.CLIRunEvent) error, asJSON, list bool, status string, drafts ...store.ChatDraft) error {
	outputs := make([]chatDraftOutput, 0, len(drafts))
	var text strings.Builder
	for _, d := range drafts {
		output := chatDraftOutput{
			Status: status, Location: "msgvault", DraftID: d.DraftID, Revision: d.Revision, Body: d.Body,
			SourceID: d.SourceID, SourceType: d.SourceType, Source: d.SourceIdentifier,
			ConversationID: d.ConversationID, SourceConversationID: d.SourceConversationID,
			ConversationType: d.ConversationType, ReplyToSourceMessageID: d.ReplyToSourceMessageID,
		}
		outputs = append(outputs, output)
		fmt.Fprintf(&text, "location=msgvault draft=%s revision=%d status=%s source=%s source_type=%s conversation=%d conversation_type=%s source_conversation=%s",
			textutil.SanitizeTerminal(d.DraftID), d.Revision, status, textutil.SanitizeTerminal(d.SourceIdentifier),
			textutil.SanitizeTerminal(d.SourceType), d.ConversationID, textutil.SanitizeTerminal(d.ConversationType),
			textutil.SanitizeTerminal(d.SourceConversationID))
		if d.ReplyToSourceMessageID != "" {
			text.WriteString(" reply_to=" + textutil.SanitizeTerminal(d.ReplyToSourceMessageID))
		}
		text.WriteString("\nbody:\n" + textutil.SanitizeTerminalMultiline(d.Body) + "\n")
	}
	if len(drafts) == 0 {
		text.WriteString("drafts=0\n")
	}
	data := text.String()
	if asJSON {
		var value any = outputs
		if !list {
			value = outputs[0]
		}
		encoded, err := jsonv2.Marshal(value)
		if err != nil {
			return draftReplyError("output_failed", err)
		}
		data = string(encoded) + "\n"
	}
	if err := emit(api.CLIRunEvent{Type: cliStreamStdout, Data: data}); err != nil {
		return draftReplyError("output_failed", err)
	}
	return nil
}
