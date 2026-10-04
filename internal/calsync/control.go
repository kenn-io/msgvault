package calsync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/gcal"
)

// PersistEvent writes a live mutation through the same archive path as sync.
// It refreshes source metadata but never advances or clears a sync cursor.
// A later delta can safely re-deliver the event to the same durable row.
func (s *Syncer) PersistEvent(ctx context.Context, cal gcal.Calendar, event gcal.Event) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if cal.ID == "" || event.ID == "" {
		return 0, errors.New("calendar and event IDs are required for write-through")
	}
	source, err := s.getOrCreateCalendarSource(ctx, cal)
	if err != nil {
		return 0, err
	}
	if err := s.store.UpdateSourceSyncConfig(source.ID, s.sourceConfigJSON(cal)); err != nil {
		return 0, fmt.Errorf("update calendar source metadata: %w", err)
	}
	if err := s.updateCalendarSourceOAuthApp(source.ID, cal.ID); err != nil {
		return 0, err
	}
	var id int64
	if event.IsCancelled() {
		id, _, err = s.flagCancelled(source.ID, cal, event)
	} else {
		id, err = s.ingestEvent(source.ID, cal, event)
	}
	if err != nil {
		return id, err
	}
	if err := s.store.RecomputeConversationStats(source.ID); err != nil {
		return id, fmt.Errorf("refresh calendar conversation statistics: %w", err)
	}
	if !event.IsCancelled() {
		s.enqueue(ctx, []int64{id})
	}
	return id, nil
}
