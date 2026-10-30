package cmd

import (
	"context"
	"errors"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
)

// runCLISendReply dispatches the shared reply surface by the source named by
// the archived message or local draft. Platform-specific senders stay small
// and own their provider policy and recovery behavior.
func (a *storeAPIAdapter) runCLISendReply(
	ctx context.Context,
	req api.CLIRunRequest,
	emit func(api.CLIRunEvent) error,
) error {
	coded := func(code string, err error) error { return &api.CLIRunCodedError{Code: code, Err: err} }
	if req.Grant != nil {
		return coded("not_permitted", errors.New("sending requires the source owner"))
	}
	intent, err := api.ParseCLIRunSendReply(req)
	if err != nil {
		return coded("invalid_arguments", err)
	}
	source, err := a.sendReplySource(ctx, intent)
	if err != nil {
		return coded("source_unavailable", err)
	}
	switch source.SourceType {
	case sourceTypeThreads:
		return a.runCLIThreadsSendReply(ctx, req, intent, source, emit)
	default:
		return errors.New("replies are not supported for " + source.SourceType + " sources")
	}
}

func (a *storeAPIAdapter) sendReplySource(ctx context.Context, intent api.SendReplyIntent) (*store.Source, error) {
	sourceID := int64(0)
	if intent.DraftID != "" {
		receipt, err := a.store.GetThreadsReplySendContext(ctx, intent.DraftID)
		if err == nil {
			sourceID = receipt.SourceID
		} else {
			if !errors.Is(err, store.ErrThreadsReplyNotFound) {
				return nil, err
			}
			draft, err := a.store.GetChatDraftContext(ctx, intent.DraftID)
			if err != nil {
				return nil, err
			}
			sourceID = draft.SourceID
		}
	} else {
		message, err := a.store.GetMessageContext(ctx, intent.MessageID)
		if err != nil {
			return nil, err
		}
		sourceID = message.SourceID
	}
	if sourceID <= 0 {
		return nil, store.ErrSourceNotFound
	}
	return a.store.GetSourceByIDContext(ctx, sourceID)
}
