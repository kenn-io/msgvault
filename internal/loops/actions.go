package loops

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

type Suppression struct {
	ID             int64     `json:"id"`
	TargetKind     string    `json:"target_kind"`
	ParticipantID  *int64    `json:"participant_id"`
	PersonID       *int64    `json:"person_id"`
	ConversationID *int64    `json:"conversation_id"`
	Note           string    `json:"note"`
	CreatedAt      time.Time `json:"created_at"`
}

type SuppressionRequest struct {
	TargetKind     string `json:"target_kind"`
	ParticipantID  *int64 `json:"participant_id,omitempty"`
	PersonID       *int64 `json:"person_id,omitempty"`
	ConversationID *int64 `json:"conversation_id,omitempty"`
	Note           string `json:"note,omitempty"`
	DryRun         bool   `json:"dry_run,omitempty"`
}

type SuppressionResult struct {
	Suppression *Suppression `json:"suppression"`
	WouldClose  int64        `json:"would_close"`
}

func publicSuppression(row store.LoopSuppression) Suppression {
	return Suppression{ID: row.ID, TargetKind: row.TargetKind, ParticipantID: row.ParticipantID,
		PersonID: row.PersonID, ConversationID: row.ConversationID, Note: row.Note, CreatedAt: row.CreatedAt}
}

func (s *Service) actionResult(ctx context.Context, row store.OpenLoop, err error) (DetailResponse, error) {
	if err != nil {
		return DetailResponse{}, err
	}
	now := s.now().UTC()
	loop, err := publicLoop(row, now)
	if err != nil {
		return DetailResponse{}, err
	}
	report, err := s.Freshness(ctx, now, []int64{row.SourceID})
	return DetailResponse{Loop: loop, Freshness: report}, err
}

func (s *Service) Close(ctx context.Context, id int64, note string) (DetailResponse, error) {
	if err := s.check(); err != nil {
		return DetailResponse{}, err
	}
	row, err := s.st.CloseLoopContext(ctx, id, note, s.now().UTC())
	return s.actionResult(ctx, row, err)
}

func (s *Service) Snooze(ctx context.Context, id int64, until time.Time) (DetailResponse, error) {
	if err := s.check(); err != nil {
		return DetailResponse{}, err
	}
	row, err := s.st.SnoozeLoopContext(ctx, id, until, s.now().UTC())
	return s.actionResult(ctx, row, err)
}

func (s *Service) Unsnooze(ctx context.Context, id int64) (DetailResponse, error) {
	if err := s.check(); err != nil {
		return DetailResponse{}, err
	}
	row, err := s.st.UnsnoozeLoopContext(ctx, id, s.now().UTC())
	return s.actionResult(ctx, row, err)
}

func (s *Service) ListSuppressions(ctx context.Context) ([]Suppression, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	rows, err := s.st.LoadLoopSuppressionsContext(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]Suppression, 0, len(rows))
	for _, row := range rows {
		result = append(result, publicSuppression(row))
	}
	return result, nil
}

func (s *Service) Suppress(ctx context.Context, request SuppressionRequest) (SuppressionResult, error) {
	if err := s.check(); err != nil {
		return SuppressionResult{}, err
	}
	row, err := s.st.CreateLoopSuppressionContext(ctx, store.LoopSuppressionRequest{
		TargetKind: request.TargetKind, ParticipantID: request.ParticipantID, PersonID: request.PersonID,
		ConversationID: request.ConversationID, Note: request.Note, DryRun: request.DryRun}, s.now().UTC())
	if err != nil {
		return SuppressionResult{}, err
	}
	result := SuppressionResult{WouldClose: row.WouldClose}
	if row.Suppression != nil {
		value := publicSuppression(*row.Suppression)
		result.Suppression = &value
	}
	return result, nil
}

// DeleteSuppression takes a durable mute ID rather than a target ID, so an old
// confirmation cannot remove a replacement mute for the same target.
func (s *Service) DeleteSuppression(ctx context.Context, id int64) error {
	if err := s.check(); err != nil {
		return err
	}
	return s.st.DeleteLoopSuppressionContext(ctx, id, s.cfg.Loops.Lookback, s.now().UTC())
}
