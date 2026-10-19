package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const openLoopCurrentPersonID = `COALESCE((SELECT pp.person_id FROM person_participants pp WHERE pp.participant_id=l.counterparty_participant_id),l.counterparty_person_id)`

const openLoopColumns = `l.id,l.turn_key,l.cycle,l.conversation_id,l.source_id,l.kind,l.detector,l.status,l.channel,l.owner_address,l.counterparty_participant_id,` + openLoopCurrentPersonID + `,l.anchor_message_id,l.owed_since_at,l.last_activity_message_id,l.last_activity_at,l.opens_at,l.due_at,l.snoozed_until,l.summary,l.evidence_json,l.ruleset_version,l.fact_claim_id,l.agenda_state,l.fulfilment_suggested_at,l.closed_at,l.closed_reason,l.closed_by,l.closed_message_id,l.created_at,l.updated_at,COALESCE((SELECT p.display_name FROM persons p WHERE p.id=` + openLoopCurrentPersonID + `),(SELECT p.display_name FROM participants p WHERE p.id=l.counterparty_participant_id),'')` + "," + loopKataColumns

func scanOpenLoop(row scanner) (OpenLoop, error) {
	var l OpenLoop
	var evidence []byte
	var kata loopKataScan
	targets := []any{&l.ID, &l.TurnKey, &l.Cycle, &l.ConversationID, &l.SourceID, &l.Kind, &l.Detector, &l.Status, &l.Channel, &l.OwnerAddress, &l.CounterpartyParticipantID, &l.CounterpartyPersonID, &l.AnchorMessageID, &l.OwedSinceAt, &l.LastActivityMessageID, &l.LastActivityAt, &l.OpensAt, &l.DueAt, &l.SnoozedUntil, &l.Summary, &evidence, &l.RulesetVersion, &l.FactClaimID, &l.AgendaState, &l.FulfilmentSuggestedAt, &l.ClosedAt, &l.ClosedReason, &l.ClosedBy, &l.ClosedMessageID, &l.CreatedAt, &l.UpdatedAt, &l.CounterpartyDisplayName}
	targets = append(targets, kata.targets()...)
	err := row.Scan(targets...)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrLoopNotFound
	}
	l.EvidenceJSON = evidence
	l.Kata = kata.value()
	return l, err
}
func (s *Store) GetOpenLoopContext(ctx context.Context, id int64) (OpenLoop, error) {
	return scanOpenLoop(s.db.QueryRowContext(ctx, "SELECT "+openLoopColumns+openLoopFrom+" WHERE l.id=?", id))
}
func listLoopsQuery(ctx context.Context, q contextRowsQuerier, query string, args ...any) ([]OpenLoop, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := []OpenLoop{}
	for rows.Next() {
		l, err := scanOpenLoop(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, l)
	}
	return result, rows.Err()
}
func (s *Store) ListLiveConversationLoopsContext(ctx context.Context, conversationID int64) ([]OpenLoop, error) {
	return listLoopsQuery(ctx, s.db, "SELECT "+openLoopColumns+openLoopFrom+" WHERE l.conversation_id=? AND l.status<>'closed' ORDER BY l.id", conversationID)
}
func (s *Store) ListOpenLoopsContext(ctx context.Context, filter LoopListFilter) ([]OpenLoop, error) {
	if filter.Limit < 1 || filter.Limit > 10000 {
		return nil, fmt.Errorf("%w: limit must be between 1 and 10000", ErrInvalidLoop)
	}
	query := "SELECT " + openLoopColumns + openLoopFrom + " WHERE 1=1"
	args := []any{}
	if filter.Kind != "" {
		query += " AND l.kind=?"
		args = append(args, filter.Kind)
	}
	if filter.Status != "all" {
		status := filter.Status
		if status == "" {
			status = "open"
		}
		query += " AND l.status=?"
		args = append(args, status)
	}
	if filter.PersonID != 0 {
		query += " AND " + openLoopCurrentPersonID + "=?"
		args = append(args, filter.PersonID)
	}
	if filter.SourceID != 0 {
		query += " AND l.source_id=?"
		args = append(args, filter.SourceID)
	}
	if !filter.IncludeSnoozed {
		query += " AND (l.status='closed' OR l.snoozed_until IS NULL OR l.snoozed_until<=?)"
		args = append(args, s.dialect.TimestampParam(filter.Now))
	}
	if filter.HasCursor {
		query += " AND (l.owed_since_at>? OR (l.owed_since_at=? AND l.id>?))"
		at := s.dialect.TimestampParam(filter.CursorAt)
		args = append(args, at, at, filter.CursorID)
	}
	query += " ORDER BY l.owed_since_at,l.id LIMIT ?"
	args = append(args, filter.Limit)
	return listLoopsQuery(ctx, s.db, query, args...)
}
