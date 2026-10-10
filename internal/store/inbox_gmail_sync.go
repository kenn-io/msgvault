package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// GmailInboxObservation is metadata obtained by sync, independently of MIME
// content and the archive's UI read state. Missing labels or history leave the
// provider markers unknown.
type GmailInboxObservation struct {
	Tags       []string
	HistoryID  uint64
	ObservedAt time.Time
}

func (s *Store) observeGmailInboxSyncTx(ctx context.Context, tx *loggedTx, sourceID, messageID int64, providerID string, observation GmailInboxObservation) error {
	var kind, identifier string
	if err := tx.QueryRowContext(ctx, `SELECT source_type,identifier FROM sources WHERE id=?`, sourceID).Scan(&kind, &identifier); err != nil {
		return fmt.Errorf("resolve Gmail inbox sync source: %w", err)
	}
	if kind != sourceTypeGmail && kind != "" {
		return fmt.Errorf("%w: inbox sync evidence requires a Gmail source", inboxcontrol.ErrInvalid)
	}
	target := inboxcontrol.Target{SourceID: sourceID, SourceType: sourceTypeGmail, SourceIdentifier: identifier, AccountID: identifier, Scope: inboxcontrol.ScopeMessage, ItemID: messageID, ProviderID: providerID}
	state := inboxcontrol.State{Target: target, ObservedAt: observation.ObservedAt}
	if observation.Tags != nil && observation.HistoryID > 0 {
		state.Tags = slices.Clone(observation.Tags)
		state.Revision = strconv.FormatUint(observation.HistoryID, 10)
		inbox := slices.Contains(state.Tags, "INBOX")
		read := !slices.Contains(state.Tags, "UNREAD")
		state.Inbox = &inbox
		state.Read = &read
	}
	hash, err := inboxcontrol.SemanticFingerprint(state)
	if err != nil {
		return err
	}
	if err = validateInboxReconcileFreshness(ctx, tx, target, state, hash); err != nil {
		return err
	}
	_, err = observeInboxStateTx(ctx, tx, state)
	return err
}

// RefreshGmailInboxLabelsContext commits a metadata-only refresh atomically.
// Unknown labels preserve the archive's prior labels. Older observations never
// restore old label membership over newer provider evidence.
func (s *Store) RefreshGmailInboxLabelsContext(ctx context.Context, sourceID, messageID int64, providerID string, labelIDs []int64, observation GmailInboxObservation) (bool, error) {
	var changed bool
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		if err := s.observeGmailInboxSyncTx(ctx, tx, sourceID, messageID, providerID, observation); err != nil {
			return err
		}
		if observation.Tags == nil {
			return nil
		}
		var err error
		changed, err = s.reconcileMessageLabelsTxContext(ctx, tx, messageID, labelIDs, true)
		return err
	})
	return changed, err
}
