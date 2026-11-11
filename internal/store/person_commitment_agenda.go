package store

import (
	"context"
	"encoding/json/v2"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/kataissues"
)

const (
	CommitmentAgendaPending = "pending"
	CommitmentAgendaLinked  = "linked"
	CommitmentAgendaRetired = "retired"
)

// CommitmentAgendaSuggestion is the exact create intent saved before Kata I/O.
type CommitmentAgendaSuggestion struct {
	CommitmentID   int64
	TargetKey      string
	IdempotencyKey string
	DestinationKey string
	Request        kataissues.CreateInput
	Quote          string
	State          string
	Attempted      bool
	IssueUID       string
	IssueRef       string
}

const commitmentAgendaColumns = `commitment_id, target_key, idempotency_key, destination_key, CAST(request_json AS TEXT),
	quote, state, attempted_at IS NOT NULL, COALESCE(issue_uid, ''), COALESCE(issue_ref, '')`

func scanCommitmentAgenda(row scanner) (CommitmentAgendaSuggestion, error) {
	var item CommitmentAgendaSuggestion
	var request string
	err := row.Scan(&item.CommitmentID, &item.TargetKey, &item.IdempotencyKey, &item.DestinationKey, &request, &item.Quote,
		&item.State, &item.Attempted, &item.IssueUID, &item.IssueRef)
	if err == nil {
		err = json.Unmarshal([]byte(request), &item.Request)
	}
	return item, err
}

func (s *Store) GetCommitmentAgendaContext(ctx context.Context, id int64) (CommitmentAgendaSuggestion, error) {
	return scanCommitmentAgenda(s.db.QueryRowContext(ctx, s.Rebind(`SELECT `+commitmentAgendaColumns+
		` FROM person_commitment_agenda WHERE commitment_id = ?`), id))
}

func (s *Store) EnqueueCommitmentAgendaContext(ctx context.Context, item CommitmentAgendaSuggestion) (CommitmentAgendaSuggestion, error) {
	if item.CommitmentID <= 0 || item.TargetKey == "" || item.IdempotencyKey == "" || item.DestinationKey == "" ||
		item.Request.PersonUID == "" || len(item.Request.Evidence) != 1 || item.Quote == "" {
		return CommitmentAgendaSuggestion{}, ErrInvalidActivityRequest
	}
	request, err := json.Marshal(item.Request, json.Deterministic(true))
	if err != nil {
		return CommitmentAgendaSuggestion{}, err
	}
	_, err = s.db.ExecContext(ctx, s.Rebind(`INSERT INTO person_commitment_agenda
		(commitment_id, target_key, idempotency_key, destination_key, request_json, quote)
		SELECT ?, ?, ?, ?, `+s.dialect.JSONBindExpr()+`, ? WHERE EXISTS (SELECT 1 FROM person_commitments
			WHERE id = ? AND target_key = ? AND current_claim_id IS NOT NULL AND owner_state <> 'done')
		ON CONFLICT (commitment_id) DO NOTHING`), item.CommitmentID, item.TargetKey, item.IdempotencyKey, item.DestinationKey,
		string(request), item.Quote, item.CommitmentID, item.TargetKey)
	if err != nil {
		return CommitmentAgendaSuggestion{}, err
	}
	return s.GetCommitmentAgendaContext(ctx, item.CommitmentID)
}

func (s *Store) ListPendingCommitmentAgendaContext(ctx context.Context, limit int) ([]CommitmentAgendaSuggestion, error) {
	if limit < 1 || limit > 20 {
		return nil, ErrInvalidActivityRequest
	}
	rows, err := s.db.QueryContext(ctx, s.Rebind(`SELECT `+commitmentAgendaColumns+
		` FROM person_commitment_agenda WHERE state = 'pending'
		 ORDER BY attempted_at IS NOT NULL, attempted_at, commitment_id LIMIT ?`), limit)
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

func (s *Store) MarkCommitmentAgendaAttemptContext(ctx context.Context, id int64, now time.Time) (bool, error) {
	result, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE person_commitment_agenda SET attempted_at = ?
		WHERE commitment_id = ? AND state = 'pending' AND EXISTS (
			SELECT 1 FROM person_commitments c WHERE c.id = commitment_id
			AND c.target_key = person_commitment_agenda.target_key
			AND c.current_claim_id IS NOT NULL AND c.owner_state <> 'done')`), s.dialect.TimestampParam(now), id)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (s *Store) RetireCommitmentAgendaContext(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE person_commitment_agenda SET state = 'retired'
		WHERE commitment_id = ? AND state = 'pending'`), id)
	return err
}

// LinkCommitmentAgendaContext records a successful create even if an owner
// closed the promise while Kata was responding.
func (s *Store) LinkCommitmentAgendaContext(ctx context.Context, id int64, uid, ref string) (CommitmentAgendaSuggestion, error) {
	if uid == "" || ref == "" {
		return CommitmentAgendaSuggestion{}, ErrInvalidActivityRequest
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE person_commitment_agenda SET state = 'linked', issue_uid = ?, issue_ref = ?
		WHERE commitment_id = ? AND state IN ('pending','retired')`), uid, ref, id)
	if err != nil {
		return CommitmentAgendaSuggestion{}, err
	}
	item, err := s.GetCommitmentAgendaContext(ctx, id)
	if err == nil && item.IssueUID != uid {
		err = errors.New("commitment agenda already links a different issue")
	}
	return item, err
}

func (s *Store) ReopenCommitmentAgendaContext(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`UPDATE person_commitment_agenda SET state = 'pending'
		WHERE commitment_id = ? AND state = 'retired'`), id)
	return err
}
