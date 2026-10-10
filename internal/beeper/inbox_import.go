package beeper

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func (imp *Importer) observeChatInbox(ctx context.Context, sourceID, conversationID int64, chatID string, chat *Chat, accountID string) error {
	source, err := imp.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return err
	}
	if source == nil || source.SourceType != sourceTypeBeeper || source.Identifier != accountID || imp.client == nil {
		return nil
	}
	target := inboxcontrol.Target{SourceID: sourceID, SourceType: sourceTypeBeeper, SourceIdentifier: source.Identifier, AccountID: accountID, Scope: inboxcontrol.ScopeChat, ItemID: conversationID, ProviderID: chatID}
	state, err := inboxChatState(imp.client.baseURL, target, chat, nil)
	if err != nil {
		// A partial, foreign or merged response must replace prior known markers
		// with unknown evidence rather than keep an actionable stale candidate.
		state = inboxcontrol.State{Target: target, ObservedAt: time.Now().UTC()}
	}
	_, err = imp.store.ObserveInboxState(ctx, state)
	return err
}
