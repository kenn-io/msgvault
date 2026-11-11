package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/kataissues"
)

const (
	CommitmentAgendaPending  = "pending"
	CommitmentAgendaLinked   = "linked"
	CommitmentAgendaRetired  = "retired"
	commitmentAgendaDeferFor = time.Hour
)

// CommitmentAgendaSuggestion is the exact create intent saved before Kata I/O.
type CommitmentAgendaSuggestion struct {
	CommitmentID   int64
	TargetKey      string
	IdempotencyKey string
	Request        kataissues.CreateInput
	Quote          string
	State          string
	Attempted      bool
	IssueUID       string
	IssueRef       string
}

const commitmentAgendaColumns = `commitment_id, target_key, idempotency_key, CAST(request_json AS TEXT),
	quote, state, attempted_at IS NOT NULL, COALESCE(issue_uid, ''), COALESCE(issue_ref, '')`
const commitmentAgendaByID = `SELECT ` + commitmentAgendaColumns + ` FROM person_commitment_agenda WHERE commitment_id = ?`

// Automatic filing waits for the existing bounded completion matching cycle.
// Missing progress is pending; attempted receipts may still recover a write.
func commitmentAgendaMatchingCompleteSQL(personID string) string {
	return `EXISTS (SELECT 1 FROM person_commitment_frontiers f WHERE f.person_id = ` + personID + `
		AND f.backfill_message_id < 0 AND f.anchor_after_id = 0
		AND f.anchor_through_sequence = 0 AND f.anchor_backfill_id = 0)`
}

func scanCommitmentAgenda(row scanner) (CommitmentAgendaSuggestion, error) {
	var item CommitmentAgendaSuggestion
	var request string
	err := row.Scan(&item.CommitmentID, &item.TargetKey, &item.IdempotencyKey, &request, &item.Quote,
		&item.State, &item.Attempted, &item.IssueUID, &item.IssueRef)
	if err == nil {
		err = json.Unmarshal([]byte(request), &item.Request)
	}
	return item, err
}

func (s *Store) GetCommitmentAgendaContext(ctx context.Context, id int64) (CommitmentAgendaSuggestion, error) {
	return scanCommitmentAgenda(s.db.QueryRowContext(ctx, s.Rebind(commitmentAgendaByID), id))
}

func (s *Store) EnqueueCommitmentAgendaContext(ctx context.Context, item CommitmentAgendaSuggestion) (CommitmentAgendaSuggestion, error) {
	if item.CommitmentID <= 0 || item.TargetKey == "" ||
		item.Request.PersonUID == "" || len(item.Request.Evidence) == 0 || item.Quote == "" {
		return CommitmentAgendaSuggestion{}, ErrInvalidActivityRequest
	}
	request, err := json.Marshal(item.Request, json.Deterministic(true))
	if err != nil {
		return CommitmentAgendaSuggestion{}, err
	}
	var queued CommitmentAgendaSuggestion
	err = s.withSourceMaintenanceGateContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO person_commitment_agenda
			(commitment_id, target_key, idempotency_key, request_json, quote)
			SELECT ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ? WHERE EXISTS (SELECT 1 FROM person_commitments
				WHERE id = ? AND target_key = ? AND quote = ? AND current_claim_id IS NOT NULL AND owner_state <> 'done')
			ON CONFLICT (commitment_id) DO UPDATE SET target_key = excluded.target_key,
				request_json = excluded.request_json,
				quote = excluded.quote, state = 'pending', deferred_at = NULL
			WHERE person_commitment_agenda.attempted_at IS NULL AND person_commitment_agenda.state <> 'linked'`,
			item.CommitmentID, item.TargetKey, newCommitmentAgendaKey(),
			string(request), item.Quote, item.CommitmentID, item.TargetKey, item.Quote)
		if err != nil {
			return err
		}
		queued, err = scanCommitmentAgenda(tx.QueryRowContext(ctx, commitmentAgendaByID, item.CommitmentID))
		return err
	})
	return queued, err
}

// A receipt freezes the quote of every message its request cites, and its only
// foreign key is to the survivor commitment, so permanently deleting a cited
// original (for example after a merge transferred the receipt) would not remove
// it. Callers run this before deleting the messages selected by messageIDs, in
// the same transaction. Soft deletion and merging never call it.
func (s *Store) deleteCommitmentAgendaCiting(ctx context.Context, q contextQuerier, messageIDs string, args ...any) error {
	cites := `SELECT 1 FROM json_each(person_commitment_agenda.request_json, '$.evidence') AS cited
		WHERE json_extract(cited.value, '$.message_id') IN (` + messageIDs + `)`
	if s.IsPostgreSQL() {
		cites = `SELECT 1 FROM jsonb_array_elements(person_commitment_agenda.request_json -> 'evidence') AS cited(value)
			WHERE (cited.value ->> 'message_id')::bigint IN (` + messageIDs + `)`
	}
	if _, err := q.ExecContext(ctx, s.Rebind(`DELETE FROM person_commitment_agenda WHERE EXISTS (`+cites+`)`), args...); err != nil {
		return fmt.Errorf("delete commitment agenda citing permanently deleted messages: %w", err)
	}
	return nil
}

// New rows receive an independent key before any Kata I/O. The receipt keeps
// that key through retries and merges.
func newCommitmentAgendaKey() string {
	return "commitment-agenda:" + rand.Text()
}

// DeferCommitmentAgendaContext moves an unpreparable item out of the next
// automatic batch. It records no attempted remote create.
func (s *Store) DeferCommitmentAgendaContext(ctx context.Context, item PersonCommitment, now time.Time) error {
	return s.withSourceMaintenanceGateContext(ctx, func(tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO person_commitment_agenda
			(commitment_id, target_key, idempotency_key, request_json, quote, state, deferred_at)
			SELECT ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ?, 'retired', ? WHERE EXISTS (SELECT 1 FROM person_commitments
				WHERE id = ? AND target_key = ? AND quote = ? AND current_claim_id IS NOT NULL AND owner_state <> 'done')
			ON CONFLICT (commitment_id) DO UPDATE SET deferred_at = excluded.deferred_at
			WHERE person_commitment_agenda.attempted_at IS NULL AND person_commitment_agenda.state <> 'linked'`,
			item.ID, item.TargetKey, newCommitmentAgendaKey(), "{}", item.Quote,
			s.dialect.TimestampParam(now), item.ID, item.TargetKey, item.Quote)
		return err
	})
}

func (s *Store) ListPendingCommitmentAgendaContext(ctx context.Context, now time.Time, limit int) ([]CommitmentAgendaSuggestion, error) {
	if limit < 1 || limit > 20 {
		return nil, ErrInvalidActivityRequest
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+commitmentAgendaColumns+
		` FROM person_commitment_agenda WHERE state = 'pending'
		AND EXISTS (SELECT 1 FROM person_commitments c WHERE c.id = commitment_id
			AND EXISTS (SELECT 1 FROM person_tracking pt WHERE pt.person_id = c.person_id)
			AND (attempted_at IS NOT NULL OR `+commitmentAgendaMatchingCompleteSQL("c.person_id")+`))
		AND (deferred_at IS NULL OR `+s.loopTimeKey("deferred_at")+` <= `+s.loopTimeKey("?")+`)
		AND NOT EXISTS (SELECT 1 FROM person_commitments c WHERE c.id = commitment_id
			AND c.owner_state = 'snoozed' AND `+s.loopTimeKey("c.snoozed_until")+` > `+s.loopTimeKey("?")+`)
		ORDER BY deferred_at IS NOT NULL, deferred_at, attempted_at IS NOT NULL, attempted_at, commitment_id LIMIT ?`),
		s.dialect.TimestampParam(now.Add(-commitmentAgendaDeferFor)), s.dialect.TimestampParam(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := []CommitmentAgendaSuggestion{}
	for rows.Next() {
		item, err := scanCommitmentAgenda(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) MarkCommitmentAgendaAttemptContext(ctx context.Context, item CommitmentAgendaSuggestion, now time.Time, automatic bool) (bool, error) {
	candidates, err := s.loadCommitmentCandidates(ctx, commitmentListFilter{id: item.CommitmentID, includeSnoozed: true}, now, PersonCommitment{}, 1)
	if err != nil || len(candidates) == 0 {
		return false, err
	}
	current, live, err := s.hydrateCommitmentCandidate(ctx, candidates[0], now, s.currentCommitmentSource)
	if err != nil || !live || current.FulfilmentConfirmed {
		return false, err
	}
	previous := candidates[0].commitment
	var fulfilmentID int64
	if previous.FulfilmentEvidenceID != nil {
		fulfilmentID = *previous.FulfilmentEvidenceID
	}
	request, err := json.Marshal(item.Request, json.Deterministic(true))
	if err != nil {
		return false, err
	}
	query := `UPDATE person_commitment_agenda SET attempted_at = ?, deferred_at = NULL
		WHERE commitment_id = ? AND idempotency_key = ? AND state = 'pending' AND target_key = ? AND quote = ?
		AND request_json = ` + s.dialect.JSONBindExpr() + `
		AND EXISTS (SELECT 1 FROM person_commitments c WHERE c.id = commitment_id
			AND c.target_key = person_commitment_agenda.target_key AND c.quote = person_commitment_agenda.quote
			AND c.current_claim_id = ? AND COALESCE(c.fulfilment_evidence_id, 0) = ?
			AND c.fulfilment_confirmed = ? AND c.owner_state <> 'done'
			AND ` + s.loopTimeKey("c.expires_at") + ` > ` + s.loopTimeKey("?")
	args := []any{s.dialect.TimestampParam(now), item.CommitmentID, item.IdempotencyKey, item.TargetKey, item.Quote, string(request), *previous.ClaimID, fulfilmentID, boolInt(previous.FulfilmentConfirmed), s.dialect.TimestampParam(now)}
	if automatic {
		query += ` AND EXISTS (SELECT 1 FROM person_tracking pt WHERE pt.person_id = c.person_id)
			AND (person_commitment_agenda.attempted_at IS NOT NULL OR ` + commitmentAgendaMatchingCompleteSQL("c.person_id") + `)
			AND (c.owner_state <> 'snoozed' OR ` + s.loopTimeKey("c.snoozed_until") + ` <= ` + s.loopTimeKey("?") + `)`
		args = append(args, s.dialect.TimestampParam(now))
	}
	result, err := s.db.ExecContext(ctx, s.Rebind(query+`) `), args...)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) RetireCommitmentAgendaContext(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE person_commitment_agenda SET state = 'retired'
		WHERE idempotency_key = ? AND state = 'pending'`), key)
	return err
}

// LinkCommitmentAgendaContext records a successful create by its frozen key,
// even after a merge moved the receipt or an owner closed the promise.
func (s *Store) LinkCommitmentAgendaContext(ctx context.Context, key, uid, ref string) (CommitmentAgendaSuggestion, error) {
	if key == "" || uid == "" || ref == "" {
		return CommitmentAgendaSuggestion{}, ErrInvalidActivityRequest
	}
	item, err := scanCommitmentAgenda(s.db.QueryRowContext(ctx, s.Rebind(`UPDATE person_commitment_agenda
		SET state = 'linked', issue_uid = ?, issue_ref = ?
		WHERE idempotency_key = ? AND attempted_at IS NOT NULL AND state IN ('pending','retired')
		RETURNING `+commitmentAgendaColumns), uid, ref, key))
	if !errors.Is(err, sql.ErrNoRows) {
		return item, err
	}
	item, err = scanCommitmentAgenda(s.db.QueryRowContext(ctx, s.Rebind(`SELECT `+commitmentAgendaColumns+
		` FROM person_commitment_agenda WHERE idempotency_key = ? AND state = 'linked'`), key))
	if err == nil && item.IssueUID != uid {
		err = errors.New("commitment agenda already links a different issue")
	}
	return item, err
}

// Prefer an existing link, then an attempted receipt. Preserve the exact
// request and key for a possible create already in flight.
func (s *Store) transferCommitmentAgendaTx(ctx context.Context, tx *loggedTx, from, into int64) error {
	query := `WITH receipts AS (
		SELECT commitment_id, state, attempted_at FROM person_commitment_agenda
		WHERE commitment_id IN (?, ?) ORDER BY commitment_id`
	if s.IsPostgreSQL() {
		// The parent locks fence inserts through the receipt foreign key.
		// Lock both receipts before choosing so a completed create can win.
		var locked int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
			SELECT id FROM person_commitments WHERE id IN (?, ?) ORDER BY id FOR UPDATE
		) AS commitments`, from, into).Scan(&locked); err != nil {
			return err
		}
		query += " FOR UPDATE"
	}
	query += `) SELECT commitment_id FROM receipts
		ORDER BY CASE WHEN state = 'linked' THEN 2 WHEN attempted_at IS NOT NULL THEN 1 ELSE 0 END DESC,
			CASE WHEN commitment_id = ? THEN 0 ELSE 1 END LIMIT 1`
	var winner int64
	err := tx.QueryRowContext(ctx, query, from, into, into).Scan(&winner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if winner == into {
		_, err = tx.ExecContext(ctx, `DELETE FROM person_commitment_agenda WHERE commitment_id = ?`, from)
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_commitment_agenda WHERE commitment_id = ?`, into); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE person_commitment_agenda SET commitment_id = ? WHERE commitment_id = ?`, into, from)
	return err
}

func (s *Store) ReopenCommitmentAgendaContext(ctx context.Context, key string) (CommitmentAgendaSuggestion, error) {
	return scanCommitmentAgenda(s.db.QueryRowContext(ctx, s.Rebind(`UPDATE person_commitment_agenda
		SET state = CASE WHEN state = 'retired' THEN 'pending' ELSE state END
		WHERE idempotency_key = ? RETURNING `+commitmentAgendaColumns), key))
}

// Revalidation may replace a detached person's target while preserving its exact
// message citation. Retain the attempted request and key through that change.
func (s *Store) reattachCommitmentAgendaTx(ctx context.Context, tx *loggedTx, personID int64, p preparedCommitmentPromise) error {
	item, err := scanCommitmentAgenda(tx.QueryRowContext(ctx, commitmentAgendaByID+` AND target_key = (SELECT target_key FROM person_commitments WHERE id = ?)`, p.existingID, p.existingID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !item.Attempted || item.State == CommitmentAgendaLinked || item.TargetKey == p.promise.TargetKey || item.Quote != p.promise.Quote || len(item.Request.Evidence) == 0 {
		return nil
	}
	start := p.promise.Start
	for _, ref := range item.Request.Evidence {
		from, to := ref.Range()
		if ref.Kind != "message" || ref.Message == nil || to <= from || ref.MessageID != p.source.MessageID || int64(from) != start || int64(to) > p.promise.End {
			return nil
		}
		start = int64(to)
	}
	if start != p.promise.End {
		return nil
	}
	var samePerson bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM persons WHERE id = ? AND vcard_uid = ?)
  OR EXISTS(SELECT 1 FROM person_uid_aliases WHERE surviving_person_id = ? AND retired_uid = ?)`, personID, item.Request.PersonUID, personID, item.Request.PersonUID).Scan(&samePerson); err != nil {
		return err
	}
	if !samePerson {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE person_commitment_agenda SET target_key = ?, state = 'pending'
  WHERE idempotency_key = ? AND target_key = ? AND attempted_at IS NOT NULL AND state <> 'linked'`, p.promise.TargetKey, item.IdempotencyKey, item.TargetKey)
	return err
}
