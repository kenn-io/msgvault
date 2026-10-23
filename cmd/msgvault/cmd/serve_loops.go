package cmd

import (
	"context"
	"iter"
	"time"

	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// The daemon hands the API server this adapter instead of *store.Store, so
// each loop read and write is forwarded for the open-loop routes to see it.
var _ loops.Store = (*storeAPIAdapter)(nil)

func (a *storeAPIAdapter) LoopConversationsContext(ctx context.Context, q store.LoopQuery) iter.Seq2[store.LoopConversation, error] {
	return a.store.LoopConversationsContext(ctx, q)
}

func (a *storeAPIAdapter) LoopMarksContext(ctx context.Context) (map[int64]store.LoopMark, error) {
	return a.store.LoopMarksContext(ctx)
}

func (a *storeAPIAdapter) GetMessageBodyTextContext(ctx context.Context, messageID int64) (string, error) {
	return a.store.GetMessageBodyTextContext(ctx, messageID)
}

func (a *storeAPIAdapter) LoopOriginalEvidenceContext(ctx context.Context, messageID int64) (mime.AutomationSignals, bool, bool, error) {
	return a.store.LoopOriginalEvidenceContext(ctx, messageID)
}

func (a *storeAPIAdapter) MessageConversationContext(ctx context.Context, messageID int64) (store.MessageConversationRef, error) {
	return a.store.MessageConversationContext(ctx, messageID)
}

func (a *storeAPIAdapter) MarkLoopDoneContext(ctx context.Context, anchorID int64, questions []store.LoopMessage) (store.LoopDecision, error) {
	return a.store.MarkLoopDoneContext(ctx, anchorID, questions)
}

func (a *storeAPIAdapter) SnoozeLoopContext(ctx context.Context, anchorID int64, until time.Time, questions []store.LoopMessage) (store.LoopDecision, error) {
	return a.store.SnoozeLoopContext(ctx, anchorID, until, questions)
}

func (a *storeAPIAdapter) ClearLoopDecisionContext(ctx context.Context, messageID int64) (bool, error) {
	return a.store.ClearLoopDecisionContext(ctx, messageID)
}

func (a *storeAPIAdapter) LoopMessageEvidenceContext(ctx context.Context, id int64, version int) (store.LoopMessageEvidence, string, bool, error) {
	return a.store.LoopMessageEvidenceContext(ctx, id, version)
}

func (a *storeAPIAdapter) SaveLoopMessageEvidenceContext(ctx context.Context, id int64, version int, token string, evidence store.LoopMessageEvidence) error {
	return a.store.SaveLoopMessageEvidenceContext(ctx, id, version, token, evidence)
}
