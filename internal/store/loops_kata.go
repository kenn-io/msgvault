package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"
)

var ErrLoopKataLinkNotFound = errors.New("open loop Kata link not found")

// LoopKataLink identifies one installation of a loop's tracked issue. LinkToken
// changes on every insert, including relinking the same issue after untracking.
type LoopKataLink struct {
	LoopID                                                              int64
	LinkToken, IssueUID, IssueRef, IssueTitle, IssueStatus, IssueWebURL string
	LinkedVia                                                           string
	LinkedAt                                                            time.Time
	CheckedAt                                                           *time.Time
	Attempts                                                            int
	NextAttemptAt                                                       *time.Time
	LastError                                                           string
	ClosedNotifiedAt, BridgeClosedLoopAt                                *time.Time
	CreatedAt, UpdatedAt                                                time.Time
}

const loopKataColumns = `COALESCE(k.loop_id,0),COALESCE(k.link_token,''),COALESCE(k.issue_uid,''),COALESCE(k.issue_ref,''),COALESCE(k.issue_title,''),COALESCE(k.issue_status,''),COALESCE(k.issue_web_url,''),COALESCE(k.linked_via,''),k.linked_at,k.checked_at,COALESCE(k.attempts,0),k.next_attempt_at,COALESCE(k.last_error,''),k.closed_notified_at,k.bridge_closed_loop_at,k.created_at,k.updated_at`
const openLoopFrom = ` FROM open_loops l LEFT JOIN loop_kata_links k ON k.loop_id=l.id`

type loopKataScan struct {
	link                                                      LoopKataLink
	linked, checked, next, notified, closed, created, updated nullableTimestamp
}

func (k *loopKataScan) targets() []any {
	l := &k.link
	return []any{&l.LoopID, &l.LinkToken, &l.IssueUID, &l.IssueRef, &l.IssueTitle, &l.IssueStatus, &l.IssueWebURL, &l.LinkedVia, &k.linked, &k.checked, &l.Attempts, &k.next, &l.LastError, &k.notified, &k.closed, &k.created, &k.updated}
}
func (k *loopKataScan) value() *LoopKataLink {
	if k.link.LoopID == 0 {
		return nil
	}
	k.link.LinkedAt = k.linked.Time
	k.link.CheckedAt = optionalTimestamp(k.checked)
	k.link.NextAttemptAt = optionalTimestamp(k.next)
	k.link.ClosedNotifiedAt = optionalTimestamp(k.notified)
	k.link.BridgeClosedLoopAt = optionalTimestamp(k.closed)
	k.link.CreatedAt = k.created.Time
	k.link.UpdatedAt = k.updated.Time
	return &k.link
}
func scanLoopKataLink(row scanner) (LoopKataLink, error) {
	var k loopKataScan
	if err := row.Scan(k.targets()...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LoopKataLink{}, ErrLoopKataLinkNotFound
		}
		return LoopKataLink{}, fmt.Errorf("read loop Kata link: %w", err)
	}
	return *k.value(), nil
}
func (s *Store) GetLoopKataLinkContext(ctx context.Context, id int64) (LoopKataLink, error) {
	return scanLoopKataLink(s.db.QueryRowContext(ctx, "SELECT "+loopKataColumns+" FROM loop_kata_links k WHERE k.loop_id=?", id))
}

// InsertLoopKataLinkContext returns the winner and whether it was inserted.
// It never replaces a link. Closed loops are admitted because tracking's remote
// call can finish after an owner closes the obligation.
func (s *Store) InsertLoopKataLinkContext(ctx context.Context, link LoopKataLink, agendaPersonUID string) (LoopKataLink, bool, error) {
	if link.LoopID <= 0 || link.IssueUID == "" || link.IssueRef == "" || link.LinkedAt.IsZero() || !slices.Contains([]string{"created", "existing", "commitments"}, link.LinkedVia) {
		return LoopKataLink{}, false, fmt.Errorf("%w: invalid Kata link", ErrInvalidLoop)
	}
	initial, err := s.GetOpenLoopContext(ctx, link.LoopID)
	if err != nil {
		return LoopKataLink{}, false, err
	}
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return LoopKataLink{}, false, fmt.Errorf("generate loop Kata link token: %w", err)
	}
	link.LinkToken = hex.EncodeToString(token)
	var result LoopKataLink
	inserted := false
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockLoopConversationContext(ctx, tx, initial.ConversationID); err != nil {
			return err
		}
		current, err := scanOpenLoop(tx.QueryRowContext(ctx, "SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=?", link.LoopID))
		if err != nil {
			return err
		}
		if current.Kata != nil {
			result = *current.Kata
			return nil
		}
		if link.CheckedAt == nil {
			link.CheckedAt = &link.LinkedAt
		}
		if link.NextAttemptAt == nil {
			link.NextAttemptAt = &link.LinkedAt
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO loop_kata_links(loop_id,link_token,issue_uid,issue_ref,issue_title,issue_status,issue_web_url,linked_via,linked_at,checked_at,attempts,next_attempt_at,last_error,closed_notified_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(loop_id) DO NOTHING`, link.LoopID, link.LinkToken, link.IssueUID, link.IssueRef, link.IssueTitle, link.IssueStatus, link.IssueWebURL, link.LinkedVia, s.dialect.TimestampParam(link.LinkedAt), s.loopTime(link.CheckedAt), link.Attempts, s.loopTime(link.NextAttemptAt), link.LastError, s.loopTime(link.ClosedNotifiedAt), s.dialect.TimestampParam(link.LinkedAt), s.dialect.TimestampParam(link.LinkedAt))
		if err != nil {
			return fmt.Errorf("insert loop Kata link: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		result, err = scanLoopKataLink(tx.QueryRowContext(ctx, "SELECT "+loopKataColumns+" FROM loop_kata_links k WHERE k.loop_id=?", link.LoopID))
		if err != nil {
			return err
		}
		if inserted && agendaPersonUID != "" && current.CounterpartyPersonID != nil {
			var matches bool
			err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE id=? AND vcard_uid=?) OR EXISTS(SELECT 1 FROM person_uid_aliases WHERE surviving_person_id=? AND retired_uid=?)`, *current.CounterpartyPersonID, agendaPersonUID, *current.CounterpartyPersonID, agendaPersonUID).Scan(&matches)
			if err != nil {
				return err
			}
			if matches {
				_, err = tx.ExecContext(ctx, "UPDATE open_loops SET agenda_state='created',updated_at=? WHERE id=?", s.dialect.TimestampParam(link.LinkedAt), link.LoopID)
			}
		}
		return err
	})
	if err != nil {
		return LoopKataLink{}, false, err
	}
	return result, inserted, nil
}
func (s *Store) DeleteLoopKataLinkContext(ctx context.Context, id int64) error {
	initial, err := s.GetOpenLoopContext(ctx, id)
	if err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockLoopConversationContext(ctx, tx, initial.ConversationID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM loop_kata_links WHERE loop_id=?", id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrLoopNotFound
		}
		return nil
	})
}

// UpdateLoopKataLinkContext updates a cache installation only while its token
// still matches. Lifecycle ownership stays with ReconcileLoopKataContext.
func (s *Store) UpdateLoopKataLinkContext(ctx context.Context, link LoopKataLink) error {
	_, err := s.db.ExecContext(ctx, `UPDATE loop_kata_links SET issue_ref=?,issue_title=?,issue_status=?,issue_web_url=?,checked_at=?,attempts=?,next_attempt_at=?,last_error=?,closed_notified_at=COALESCE(?,closed_notified_at),updated_at=COALESCE(?,updated_at) WHERE loop_id=? AND link_token=?`, link.IssueRef, link.IssueTitle, link.IssueStatus, link.IssueWebURL, s.loopTime(link.CheckedAt), link.Attempts, s.loopTime(link.NextAttemptAt), link.LastError, s.loopTime(link.ClosedNotifiedAt), s.loopKataUpdatedTime(link.UpdatedAt), link.LoopID, link.LinkToken)
	return err
}
func (s *Store) loopKataUpdatedTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}
	return s.dialect.TimestampParam(at)
}
func (s *Store) LoopKataWorkContext(ctx context.Context, now time.Time, reopenWindow time.Duration, batch int, commentOnClose, closeOnIssueClosed bool) ([]OpenLoop, error) {
	if batch < 1 || reopenWindow <= 0 || now.IsZero() {
		return nil, fmt.Errorf("%w: invalid Kata work bounds", ErrInvalidLoop)
	}
	query := "SELECT " + openLoopColumns + openLoopFrom + ` WHERE k.loop_id IS NOT NULL AND (k.next_attempt_at IS NULL OR k.next_attempt_at<=?) AND (l.status<>'closed'`
	args := []any{s.dialect.TimestampParam(now)}
	if closeOnIssueClosed {
		query += " OR (k.bridge_closed_loop_at IS NOT NULL AND l.closed_at>=?)"
		args = append(args, s.dialect.TimestampParam(now.Add(-reopenWindow)))
	}
	if commentOnClose {
		query += " OR (l.closed_reason='turn_flip' AND k.closed_notified_at IS NULL AND k.issue_status<>'missing')"
	}
	query += ") ORDER BY k.next_attempt_at NULLS FIRST,k.loop_id LIMIT ?"
	args = append(args, batch)
	return listLoopsQuery(ctx, s.db, query, args...)
}

// ReconcileLoopKataContext applies a remote observation only if both its link
// installation and the owner's loop state are unchanged under the conversation
// lock. Reopening and enqueueing are committed together.
func (s *Store) ReconcileLoopKataContext(ctx context.Context, observed OpenLoop, issueClosed bool, now time.Time, reopenWindow time.Duration) error {
	if observed.Kata == nil {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockLoopConversationContext(ctx, tx, observed.ConversationID); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		current, err := scanOpenLoop(tx.QueryRowContext(ctx, "SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=? AND l.conversation_id=?", observed.ID, observed.ConversationID))
		if errors.Is(err, ErrLoopNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Kata == nil || current.Kata.LinkToken != observed.Kata.LinkToken || current.Status != observed.Status || !current.UpdatedAt.Equal(observed.UpdatedAt) {
			return nil
		}
		if issueClosed {
			if current.Status == "closed" {
				return nil
			}
			evidence, err := loopEvidenceMetadata(current.EvidenceJSON, "kata_closed_ref", current.Kata.IssueRef)
			if err != nil {
				return err
			}
			if err = s.closeLoopTx(ctx, tx, current.ID, "done", "system", now, nil); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE open_loops SET evidence_json=? WHERE id=?", string(evidence), current.ID); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, "UPDATE loop_kata_links SET bridge_closed_loop_at=?,updated_at=? WHERE loop_id=? AND link_token=?", s.dialect.TimestampParam(now), s.dialect.TimestampParam(now), current.ID, current.Kata.LinkToken)
			return err
		}
		if current.Status != "closed" || current.ClosedReason == nil || *current.ClosedReason != "done" || current.ClosedBy == nil || *current.ClosedBy != "system" || current.Kata.BridgeClosedLoopAt == nil || current.ClosedAt == nil || current.ClosedAt.Before(now.Add(-reopenWindow)) {
			return nil
		}
		muted, err := loopSuppressionAppliesTx(ctx, tx, current.ConversationID, current.CounterpartyParticipantID, current.CounterpartyPersonID)
		if err != nil {
			return fmt.Errorf("check loop suppression before Kata reopen: %w", err)
		}
		if !muted {
			// Both live uniqueness predicates are checked while holding the same lock
			// used by all loop insertions. Avoiding a speculative constraint violation
			// also keeps PostgreSQL's transaction usable for the durable blocked stamp.
			var blocker int64
			err = tx.QueryRowContext(ctx, `SELECT id FROM open_loops WHERE id<>? AND status<>'closed' AND (turn_key=? OR (conversation_id=? AND kind=? AND detector='turn_state' AND ?='turn_state')) ORDER BY id LIMIT 1`, current.ID, current.TurnKey, current.ConversationID, current.Kind, current.Detector).Scan(&blocker)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if blocker != 0 {
				evidence, err := loopEvidenceMetadata(current.EvidenceJSON, "kata_reopen_blocked", strconv.FormatInt(blocker, 10))
				if err != nil {
					return err
				}
				if _, err = tx.ExecContext(ctx, "UPDATE open_loops SET evidence_json=?,updated_at=? WHERE id=?", string(evidence), s.dialect.TimestampParam(now), current.ID); err != nil {
					return err
				}
			} else {
				status := "open"
				if current.OpensAt.After(now) {
					status = "held"
				}
				if _, err = tx.ExecContext(ctx, "UPDATE open_loops SET status=?,closed_at=NULL,closed_reason=NULL,closed_by=NULL,closed_message_id=NULL,updated_at=? WHERE id=?", status, s.dialect.TimestampParam(now), current.ID); err != nil {
					return err
				}
				if err = s.enqueueLoopTx(ctx, tx, []int64{current.ConversationID}, "reopen", "", now); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE loop_kata_links SET bridge_closed_loop_at=NULL,updated_at=? WHERE loop_id=? AND link_token=?", s.dialect.TimestampParam(now), current.ID, current.Kata.LinkToken)
		return err
	})
}
func (s *Store) RecordLoopKataRunContext(ctx context.Context, now time.Time, runErr error) error {
	message := ""
	if runErr != nil {
		message = runErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE loop_derivation_state SET kata_last_run_at=?,kata_last_success_at=CASE WHEN ?='' THEN ? ELSE kata_last_success_at END,kata_last_error=?,updated_at=? WHERE singleton=1`, s.dialect.TimestampParam(now), message, s.dialect.TimestampParam(now), message, s.dialect.TimestampParam(now))
	return err
}

func (s *Store) ensureLoopKataStateColumns(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		var (
			rows *loggedRows
			err  error
		)
		if s.IsPostgreSQL() {
			rows, err = tx.QueryContext(ctx, `SELECT column_name FROM information_schema.columns
				WHERE table_schema = current_schema() AND table_name = 'loop_derivation_state'`)
		} else {
			rows, err = tx.QueryContext(ctx, "PRAGMA table_info(loop_derivation_state)")
		}
		if err != nil {
			return err
		}
		columns := make(map[string]struct{})
		for rows.Next() {
			var name string
			if s.IsPostgreSQL() {
				if err := rows.Scan(&name); err != nil {
					_ = rows.Close()
					return err
				}
			} else {
				var cid, notNull, primaryKey int
				var columnType string
				var defaultValue sql.NullString
				if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
					_ = rows.Close()
					return err
				}
			}
			columns[name] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		timestamp := "DATETIME"
		if s.IsPostgreSQL() {
			timestamp = "TIMESTAMPTZ"
		}
		for _, column := range []struct{ name, definition string }{{"kata_last_run_at", timestamp}, {"kata_last_success_at", timestamp}, {"kata_last_error", "TEXT NOT NULL DEFAULT ''"}} {
			if _, exists := columns[column.name]; exists {
				continue
			}
			if _, err := tx.ExecContext(ctx, "ALTER TABLE loop_derivation_state ADD COLUMN "+column.name+" "+column.definition); err != nil {
				return fmt.Errorf("add loop Kata state %s: %w", column.name, err)
			}
		}
		return nil
	})
}

// RefreshLoopKataLinkContext persists a detail read without overwriting bridge
// progress that may have changed while the remote request was in flight.
func (s *Store) RefreshLoopKataLinkContext(ctx context.Context, link LoopKataLink) error {
	_, err := s.db.ExecContext(ctx, `UPDATE loop_kata_links SET issue_ref=?,issue_title=?,issue_status=?,issue_web_url=?,checked_at=?,last_error=?,updated_at=COALESCE(?,updated_at) WHERE loop_id=? AND link_token=?`, link.IssueRef, link.IssueTitle, link.IssueStatus, link.IssueWebURL, s.loopTime(link.CheckedAt), link.LastError, s.loopKataUpdatedTime(link.UpdatedAt), link.LoopID, link.LinkToken)
	return err
}

// RecordLoopKataRefreshErrorContext preserves the last successful observation
// when a detail read fails after another request refreshed the same link.
func (s *Store) RecordLoopKataRefreshErrorContext(ctx context.Context, link LoopKataLink) error {
	_, err := s.db.ExecContext(ctx, `UPDATE loop_kata_links SET last_error=?,updated_at=COALESCE(?,updated_at) WHERE loop_id=? AND link_token=?`, link.LastError, s.loopKataUpdatedTime(link.UpdatedAt), link.LoopID, link.LinkToken)
	return err
}

// RecordLoopKataFailureContext updates bridge backoff without replacing a
// successful issue observation made while the bridge request was in flight.
func (s *Store) RecordLoopKataFailureContext(ctx context.Context, link LoopKataLink) error {
	_, err := s.db.ExecContext(ctx, `UPDATE loop_kata_links SET attempts=?,next_attempt_at=?,last_error=?,updated_at=COALESCE(?,updated_at) WHERE loop_id=? AND link_token=?`, link.Attempts, s.loopTime(link.NextAttemptAt), link.LastError, s.loopKataUpdatedTime(link.UpdatedAt), link.LoopID, link.LinkToken)
	return err
}
