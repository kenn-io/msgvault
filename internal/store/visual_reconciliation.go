package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type VisualPublicationFilter struct {
	AfterMessageID int64
	LimitMessages  int
	MessageIDs     []int64
}

type VisualPublicationPage struct {
	Publications       []VisualPublication
	NextAfterMessageID int64
	HasMore            bool
}

func (s *Store) AttachmentChangeHighWater(ctx context.Context) (int64, error) {
	var sequence int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT MAX(sequence) FROM (
			SELECT COALESCE(MAX(sequence), 0) AS sequence FROM attachment_change_log
			UNION ALL
			SELECT COALESCE(MAX(last_sequence), 0) AS sequence FROM attachment_change_consumers
			UNION ALL
			SELECT COALESCE(MAX(baseline_sequence), 0) AS sequence FROM attachment_change_consumers
		) visual_change_high_water`).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("read attachment change high water: %w", err)
	}
	return sequence, nil
}

func (s *Store) GetVisualGeneration(ctx context.Context, generationID int64) (VisualGeneration, error) {
	if generationID <= 0 {
		return VisualGeneration{}, errors.New("visual generation ID must be positive")
	}
	var generation VisualGeneration
	err := s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, model, dimension, state, source_fence, consented_at IS NOT NULL
		FROM visual_generations WHERE id = ?`, generationID).Scan(
		&generation.ID, &generation.Fingerprint, &generation.Model,
		&generation.Dimension, &generation.State, &generation.SourceFence, &generation.Consented,
	)
	if err != nil {
		return VisualGeneration{}, fmt.Errorf("read visual generation: %w", err)
	}
	return generation, nil
}

func (s *Store) ActiveVisualGeneration(ctx context.Context) (VisualGeneration, error) {
	return s.visualGenerationByState(ctx, VisualGenerationActive)
}

func (s *Store) BuildingVisualGeneration(ctx context.Context) (VisualGeneration, error) {
	return s.visualGenerationByState(ctx, VisualGenerationBuilding)
}

func (s *Store) visualGenerationByState(ctx context.Context, state VisualGenerationState) (VisualGeneration, error) {
	var generation VisualGeneration
	err := s.db.QueryRowContext(ctx, `
		SELECT id, fingerprint, model, dimension, state, source_fence, consented_at IS NOT NULL
		FROM visual_generations WHERE state = ?`, state).Scan(
		&generation.ID, &generation.Fingerprint, &generation.Model,
		&generation.Dimension, &generation.State, &generation.SourceFence, &generation.Consented,
	)
	if err != nil {
		return VisualGeneration{}, fmt.Errorf("read %s visual generation: %w", state, err)
	}
	return generation, nil
}

// ActivateVisualGeneration atomically swaps the searchable generation after
// the caller proves convergence at sourceFence. The monotonic fence prevents a
// stale coordinator from activating an older archive snapshot.
func (s *Store) ActivateVisualGeneration(ctx context.Context, generationID, sourceFence int64) error {
	if generationID <= 0 || sourceFence < 0 {
		return errors.New("invalid visual generation activation")
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		var currentHighWater int64
		if err := q.QueryRow(`
			SELECT MAX(sequence) FROM (
				SELECT COALESCE(MAX(sequence), 0) AS sequence FROM attachment_change_log
				UNION ALL
				SELECT COALESCE(MAX(last_sequence), 0) AS sequence FROM attachment_change_consumers
				UNION ALL
				SELECT COALESCE(MAX(baseline_sequence), 0) AS sequence FROM attachment_change_consumers
			) visual_change_high_water`).Scan(&currentHighWater); err != nil {
			return err
		}
		if currentHighWater != sourceFence {
			return errors.New("visual generation activation raced an attachment change")
		}
		var state VisualGenerationState
		var storedFence int64
		var consented bool
		if err := q.QueryRow(`SELECT state, source_fence, consented_at IS NOT NULL FROM visual_generations WHERE id = ?`, generationID).
			Scan(&state, &storedFence, &consented); err != nil {
			return err
		}
		if !consented {
			return errors.New("visual generation requires explicit hosted-processing consent")
		}
		if state != VisualGenerationBuilding && state != VisualGenerationActive {
			return errors.New("only a building visual generation can be activated")
		}
		if storedFence < sourceFence {
			return errors.New("visual generation has not reached the activation source fence")
		}
		if _, err := q.Exec(`UPDATE visual_generations SET state = 'retired'
			WHERE state = 'active' AND id <> ?`, generationID); err != nil {
			return err
		}
		result, err := q.Exec(`UPDATE visual_generations
			SET state = 'active', activated_at = `+s.dialect.Now()+`
			WHERE id = ? AND state IN ('building', 'active')`, generationID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (s *Store) RetireVisualGeneration(ctx context.Context, generationID int64) error {
	if generationID <= 0 {
		return errors.New("invalid visual generation retirement")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE visual_generations SET state = 'retired' WHERE id = ?`, generationID)
	if err != nil {
		return fmt.Errorf("retire visual generation: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) CountActiveVisualClaims(
	ctx context.Context,
	generationID int64,
	now time.Time,
) (int64, error) {
	if generationID <= 0 || now.IsZero() {
		return 0, errors.New("invalid active visual claim query")
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM visual_work_claims
		WHERE generation_id = ? AND lease_expires_at > ?`, generationID, now).Scan(&count); err != nil {
		return 0, fmt.Errorf("count active visual claims: %w", err)
	}
	return count, nil
}

// AdvanceVisualGenerationSourceFence records the greatest journal sequence
// for which reconciliation decisions are durable. It is monotonic so an older
// worker can never move truthful status backwards.
func (s *Store) AdvanceVisualGenerationSourceFence(
	ctx context.Context,
	generationID, sourceFence int64,
) error {
	if generationID <= 0 || sourceFence < 0 {
		return errors.New("invalid visual generation source fence")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE visual_generations
		SET source_fence = CASE WHEN source_fence < ? THEN ? ELSE source_fence END
		WHERE id = ?`, sourceFence, sourceFence, generationID)
	if err != nil {
		return fmt.Errorf("advance visual generation source fence: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read visual generation fence result: %w", err)
	}
	if rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}

// ListVisualPublications pages by distinct message ID so every publication
// owned by one message is returned atomically in a page.
func (s *Store) ListVisualPublications(
	ctx context.Context,
	generationID int64,
	filter VisualPublicationFilter,
) (VisualPublicationPage, error) {
	if generationID <= 0 || filter.AfterMessageID < 0 {
		return VisualPublicationPage{}, errors.New("invalid visual publication filter")
	}
	messageIDs, hasMore, err := s.visualPublicationMessagePage(ctx, generationID, filter)
	if err != nil || len(messageIDs) == 0 {
		return VisualPublicationPage{HasMore: hasMore}, err
	}
	args := make([]any, 0, len(messageIDs)+1)
	args = append(args, generationID)
	for _, id := range messageIDs {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT generation_id, message_id, blob_hash, media_input_key,
		       published_revision, prepared_revision, source_fence,
		       representative_attachment_id, attachment_role, role_source,
		       current_vector_token, pending_vector_token, state,
		       outcome_kind, outcome_reason
		FROM visual_publications
		WHERE generation_id = ? AND message_id IN (`+sqlPlaceholders(len(messageIDs))+`)
		ORDER BY message_id, blob_hash, media_input_key`), args...)
	if err != nil {
		return VisualPublicationPage{}, fmt.Errorf("list visual publications: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := VisualPublicationPage{
		Publications:       make([]VisualPublication, 0),
		NextAfterMessageID: messageIDs[len(messageIDs)-1],
		HasMore:            hasMore,
	}
	for rows.Next() {
		publication, scanErr := scanVisualPublicationRow(rows)
		if scanErr != nil {
			return VisualPublicationPage{}, scanErr
		}
		page.Publications = append(page.Publications, publication)
	}
	if err := rows.Err(); err != nil {
		return VisualPublicationPage{}, fmt.Errorf("iterate visual publications: %w", err)
	}
	return page, nil
}

func (s *Store) visualPublicationMessagePage(
	ctx context.Context,
	generationID int64,
	filter VisualPublicationFilter,
) ([]int64, bool, error) {
	if len(filter.MessageIDs) > 0 {
		if filter.AfterMessageID != 0 || filter.LimitMessages != 0 {
			return nil, false, errors.New("explicit publication message IDs cannot be combined with pagination")
		}
		ids, err := normalizedPositiveIDs(filter.MessageIDs, 10_000)
		if err != nil {
			return nil, false, err
		}
		args := make([]any, 0, len(ids)+1)
		args = append(args, generationID)
		for _, id := range ids {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
			SELECT DISTINCT message_id FROM visual_publications
			WHERE generation_id = ? AND message_id IN (`+sqlPlaceholders(len(ids))+`)
			ORDER BY message_id`), args...)
		if err != nil {
			return nil, false, fmt.Errorf("select visual publication messages: %w", err)
		}
		defer func() { _ = rows.Close() }()
		selected, err := scanInt64Rows(rows)
		return selected, false, err
	}
	limit := filter.LimitMessages
	if limit == 0 {
		limit = 500
	}
	if limit < 1 || limit > 1000 {
		return nil, false, errors.New("visual publication message limit must be between 1 and 1000")
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(`
		SELECT DISTINCT message_id FROM visual_publications
		WHERE generation_id = ? AND message_id > ?
		ORDER BY message_id LIMIT ?`), generationID, filter.AfterMessageID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("page visual publication messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ids, err := scanInt64Rows(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	return ids, hasMore, nil
}

func scanVisualPublicationRow(scanner scanner) (VisualPublication, error) {
	var publication VisualPublication
	var published, prepared, current, pending, outcomeKind, outcomeReason sql.NullString
	var representative sql.NullInt64
	if err := scanner.Scan(
		&publication.GenerationID, &publication.Owner.MessageID,
		&publication.Owner.BlobHash, &publication.Owner.MediaInputKey,
		&published, &prepared, &publication.SourceFence, &representative,
		&publication.Role, &publication.RoleSource, &current, &pending,
		&publication.State, &outcomeKind, &outcomeReason,
	); err != nil {
		return VisualPublication{}, fmt.Errorf("scan visual publication: %w", err)
	}
	publication.PublishedRevision = published.String
	publication.PreparedRevision = prepared.String
	publication.RepresentativeAttachmentID = representative.Int64
	publication.CurrentVectorToken = current.String
	publication.PendingVectorToken = pending.String
	publication.OutcomeKind = outcomeKind.String
	publication.OutcomeReason = outcomeReason.String
	return publication, nil
}

type int64Rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanInt64Rows(rows int64Rows) ([]int64, error) {
	result := make([]int64, 0)
	for rows.Next() {
		var value int64
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan database ID: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate database IDs: %w", err)
	}
	return result, nil
}
