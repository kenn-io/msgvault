package store

import (
	"context"
	"fmt"
	"time"
)

// LoopDerivationState reports durable progress. Nullable times distinguish
// unknown progress from a certified instant. A nil FeedCursorAt resumes at
// the start of FeedFrontier, including every ID stamped there. RescanReason
// flags a prepared/running scan; RescanStartedAt becomes nonnil with its first
// committed enumeration slice, independently of the legal cursor ID zero.
type LoopDerivationState struct {
	KataLastRunAt                *time.Time `json:"kata_last_run_at"`
	KataLastSuccessAt            *time.Time `json:"kata_last_success_at"`
	KataLastError                string     `json:"kata_last_error"`
	Singleton                    int        `json:"singleton"`
	FeedCursorAt                 *time.Time `json:"feed_cursor_at"`
	FeedCursorMessageID          int64      `json:"feed_cursor_message_id"`
	FeedFrontier                 *time.Time `json:"feed_frontier"`
	AppliedThrough               *time.Time `json:"applied_through"`
	IdentityRevision             int64      `json:"identity_revision"`
	AccountIdentityRevision      int64      `json:"account_identity_revision"`
	RulesetVersion               int        `json:"ruleset_version"`
	ConfigFingerprint            string     `json:"config_fingerprint"`
	RescanReason                 string     `json:"rescan_reason"`
	RescanEpoch                  string     `json:"rescan_epoch"`
	RescanLastConversationID     int64      `json:"rescan_last_conversation_id"`
	RescanStartedAt              *time.Time `json:"rescan_started_at"`
	LastReconcileAt              *time.Time `json:"last_reconcile_at"`
	SignalsBackfillComplete      bool       `json:"signals_backfill_complete"`
	SignalsBackfillLastMessageID int64      `json:"signals_backfill_last_message_id"`
	LastRunAt                    *time.Time `json:"last_run_at"`
	LastSuccessAt                *time.Time `json:"last_success_at"`
	LastError                    string     `json:"last_error"`
	UpdatedAt                    time.Time  `json:"updated_at"`
	QueueDepth                   int64      `json:"queue_depth"`
}

// LoopEpoch is the ownership/rules/configuration revision certified together.
type LoopEpoch struct {
	IdentityRevision, AccountIdentityRevision int64
	RulesetVersion                            int
	ConfigFingerprint                         string
}

func (e LoopEpoch) String() string {
	return fmt.Sprintf("%d:%d:%d:%s", e.IdentityRevision, e.AccountIdentityRevision, e.RulesetVersion, e.ConfigFingerprint)
}
func (s *Store) CurrentLoopEpochContext(ctx context.Context, rules int, fingerprint string) (LoopEpoch, error) {
	return currentLoopEpoch(ctx, s.db, rules, fingerprint)
}
func currentLoopEpoch(ctx context.Context, q contextRowQuerier, rules int, fingerprint string) (LoopEpoch, error) {
	e := LoopEpoch{RulesetVersion: rules, ConfigFingerprint: fingerprint}
	var err error
	e.IdentityRevision, err = readArchiveMetadataRevisionContext(ctx, q, identityRevisionKey, "identity")
	if err != nil {
		return e, err
	}
	e.AccountIdentityRevision, err = readArchiveMetadataRevisionContext(ctx, q, accountIdentityRevisionKey, "account identity")
	return e, err
}
func (s *Store) LoopDerivationStateContext(ctx context.Context) (LoopDerivationState, error) {
	return readLoopState(ctx, s.db)
}
func readLoopState(ctx context.Context, q contextRowQuerier) (LoopDerivationState, error) {
	var out LoopDerivationState
	var cursor, frontier, applied, started, reconciled, run, success, updated, kataRun, kataSuccess nullableTimestamp
	err := q.QueryRowContext(ctx, `SELECT kata_last_run_at,kata_last_success_at,kata_last_error,singleton,feed_cursor_at,feed_cursor_message_id,feed_frontier,applied_through,identity_revision,account_identity_revision,ruleset_version,config_fingerprint,rescan_reason,rescan_epoch,rescan_last_conversation_id,rescan_started_at,last_reconcile_at,signals_backfill_complete,signals_backfill_last_message_id,last_run_at,last_success_at,last_error,updated_at,(SELECT COUNT(*) FROM loop_conversation_queue WHERE revision>processed_revision) FROM loop_derivation_state WHERE singleton=1`).Scan(&kataRun, &kataSuccess, &out.KataLastError, &out.Singleton, &cursor, &out.FeedCursorMessageID, &frontier, &applied, &out.IdentityRevision, &out.AccountIdentityRevision, &out.RulesetVersion, &out.ConfigFingerprint, &out.RescanReason, &out.RescanEpoch, &out.RescanLastConversationID, &started, &reconciled, &out.SignalsBackfillComplete, &out.SignalsBackfillLastMessageID, &run, &success, &out.LastError, &updated, &out.QueueDepth)
	out.KataLastRunAt = optionalTimestamp(kataRun)
	out.KataLastSuccessAt = optionalTimestamp(kataSuccess)
	out.FeedCursorAt = optionalTimestamp(cursor)
	out.FeedFrontier = optionalTimestamp(frontier)
	out.AppliedThrough = optionalTimestamp(applied)
	out.RescanStartedAt = optionalTimestamp(started)
	out.LastReconcileAt = optionalTimestamp(reconciled)
	out.LastRunAt = optionalTimestamp(run)
	out.LastSuccessAt = optionalTimestamp(success)
	out.UpdatedAt = updated.Time
	if err != nil {
		return out, fmt.Errorf("read loop derivation state: %w", err)
	}
	return out, nil
}

// Acquire the singleton before reading mutable projection bookkeeping. This
// reserves SQLite's writer and serializes PostgreSQL state transitions.
func lockLoopState(ctx context.Context, tx *loggedTx) error {
	_, err := tx.ExecContext(ctx, "UPDATE loop_derivation_state SET updated_at=updated_at WHERE singleton=1")
	return err
}
func (s *Store) loopTime(at *time.Time) any {
	if at == nil {
		return nil
	}
	return s.dialect.TimestampParam(*at)
}

// BeginLoopRescanContext prepares an inclusive scan. Resetting the feed is only
// used for explicit rebuild; live/user/history rows are never deleted.
func (s *Store) BeginLoopRescanContext(ctx context.Context, epoch LoopEpoch, reason string, resetFeed *time.Time, now time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := lockLoopState(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE loop_derivation_state SET rescan_reason=?,rescan_epoch=?,rescan_last_conversation_id=0,rescan_started_at=NULL,updated_at=? WHERE singleton=1`, reason, epoch.String(), s.dialect.TimestampParam(now))
		if err != nil {
			return err
		}
		if resetFeed != nil {
			_, err = tx.ExecContext(ctx, `UPDATE loop_derivation_state SET feed_cursor_at=NULL,feed_cursor_message_id=0,feed_frontier=? WHERE singleton=1`, s.dialect.TimestampParam(*resetFeed))
		}
		return err
	})
}

// FinalizeLoopProgressContext releases the prior-progress cap only after a
// same-epoch scan has enumerated and drained. Unknown pending bounds remain
// unknown: SQL MIN alone would incorrectly ignore their NULL values.
func (s *Store) FinalizeLoopProgressContext(ctx context.Context, expected LoopEpoch, cutoff, now time.Time) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := lockLoopState(ctx, tx); err != nil {
			return err
		}
		state, err := readLoopState(ctx, tx)
		if err != nil {
			return err
		}
		certified := false
		if state.RescanReason != "" && state.RescanStartedAt != nil && state.RescanEpoch == expected.String() {
			actual, err := currentLoopEpoch(ctx, tx, expected.RulesetVersion, expected.ConfigFingerprint)
			if err != nil {
				return err
			}
			ids, err := s.loopRescanIDs(ctx, tx, state, cutoff, 1)
			if err != nil {
				return err
			}
			var pending bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM loop_conversation_queue WHERE revision>processed_revision AND epoch=?)`, state.RescanEpoch).Scan(&pending); err != nil {
				return err
			}
			certified = actual == expected && len(ids) == 0 && !pending
			if certified {
				_, err = tx.ExecContext(ctx, `UPDATE loop_derivation_state SET identity_revision=?,account_identity_revision=?,ruleset_version=?,config_fingerprint=?,rescan_reason='',rescan_epoch='',rescan_last_conversation_id=0,rescan_started_at=NULL,last_reconcile_at=? WHERE singleton=1`, expected.IdentityRevision, expected.AccountIdentityRevision, expected.RulesetVersion, expected.ConfigFingerprint, s.dialect.TimestampParam(now))
				if err != nil {
					return err
				}
			}
		}
		progress := state.FeedFrontier
		if state.QueueDepth > 0 {
			var bound nullableTimestamp
			if err := tx.QueryRowContext(ctx, `SELECT feed_bound FROM loop_conversation_queue WHERE revision>processed_revision ORDER BY CASE WHEN feed_bound IS NULL THEN 0 ELSE 1 END,feed_bound LIMIT 1`).Scan(&bound); err != nil {
				return err
			}
			progress = loopEarlier(progress, optionalTimestamp(bound))
		}
		if state.RescanReason != "" && !certified {
			progress = loopEarlier(progress, state.AppliedThrough)
		}
		_, err = tx.ExecContext(ctx, `UPDATE loop_derivation_state SET applied_through=?,updated_at=? WHERE singleton=1`, s.loopTime(progress), s.dialect.TimestampParam(now))
		return err
	})
}
func loopEarlier(a, b *time.Time) *time.Time {
	if a == nil || b == nil {
		return nil
	}
	if a.Before(*b) {
		return a
	}
	return b
}

func (s *Store) RecordLoopRunContext(ctx context.Context, now time.Time, runErr error) error {
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE loop_derivation_state SET last_run_at=?,last_success_at=CASE WHEN ?='' THEN ? ELSE last_success_at END,last_error=?,updated_at=? WHERE singleton=1`, s.dialect.TimestampParam(now), message, s.dialect.TimestampParam(now), message, s.dialect.TimestampParam(now))
	return err
}

func (s *Store) AdvanceLoopTimersContext(ctx context.Context, now time.Time) (promoted, unsnoozed int64, err error) {
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		result, err := tx.ExecContext(ctx, `UPDATE open_loops SET status='open',updated_at=? WHERE status='held' AND opens_at<=?`, s.dialect.TimestampParam(now), s.dialect.TimestampParam(now))
		if err != nil {
			return err
		}
		promoted, err = result.RowsAffected()
		if err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE open_loops SET snoozed_until=NULL,updated_at=? WHERE status<>'closed' AND snoozed_until<=?`, s.dialect.TimestampParam(now), s.dialect.TimestampParam(now))
		if err != nil {
			return err
		}
		unsnoozed, err = result.RowsAffected()
		return err
	})
	if err != nil {
		return 0, 0, err
	}
	return promoted, unsnoozed, nil
}
