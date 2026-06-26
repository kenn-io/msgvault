package calsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/store"
)

// Incremental syncs each of the account's existing calendar sources from its
// stored syncToken. A calendar with no token yet is full-synced. A 410 (expired
// token) self-heals: the cursor is cleared and the calendar is full-resynced.
// Per-calendar failures are logged and do not abort the others.
func (s *Syncer) Incremental(ctx context.Context) (Result, error) {
	if err := ValidateMinAccessRole(s.opts.MinAccessRole); err != nil {
		return Result{}, err
	}
	sources, err := s.store.GetSourcesByTypeAndAccount(gcal.SourceType, s.opts.AccountEmail)
	if err != nil {
		return Result{}, fmt.Errorf("enumerate calendar sources: %w", err)
	}

	var result Result
	var firstErr error
	recordErr := func(err error) {
		s.logger.Error("calendar incremental sync failed", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}

	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		cfg := parseSourceConfig(src.SyncConfig)
		if cfg.CalendarID == "" {
			continue
		}
		cal := gcal.Calendar{
			ID:         cfg.CalendarID,
			Summary:    cfg.CalendarSummary,
			AccessRole: cfg.AccessRole,
			Primary:    cfg.Primary,
			TimeZone:   cfg.TimeZone,
		}
		if !s.includeCalendar(cal) {
			continue
		}

		// No token yet → full sync.
		if !src.SyncCursor.Valid || src.SyncCursor.String == "" {
			if err := s.syncCalendarFull(ctx, cal, &result); err != nil {
				recordErr(err)
				continue
			}
			result.CalendarsSynced++
			continue
		}

		err := s.incrementalCalendar(ctx, src, cal, &result)
		if errors.Is(err, ErrSyncTokenExpired) {
			s.logger.Warn("calendar sync token expired; running full resync", "calendar", cal.ID)
			_ = s.store.UpdateSourceSyncCursor(src.ID, "")
			if ferr := s.syncCalendarFull(ctx, cal, &result); ferr != nil {
				recordErr(ferr)
				continue
			}
			result.CalendarsSynced++
			continue
		}
		if err != nil {
			recordErr(err)
			continue
		}
		result.CalendarsSynced++
	}

	s.enqueue(ctx, result.InsertedIDs)
	return result, firstErr
}

// incrementalCalendar runs a single calendar's incremental sync. It advances the
// stored cursor only when every event on the page persisted; if any event failed
// (recorded to sync_run_items), it fails the run and leaves the cursor untouched
// so the next sync re-delivers and retries the failed events rather than losing
// them. A 410 surfaces as ErrSyncTokenExpired for the caller to self-heal.
func (s *Syncer) incrementalCalendar(ctx context.Context, src *store.Source, cal gcal.Calendar, result *Result) error {
	syncID, err := s.store.StartSync(src.ID, "incremental")
	if err != nil {
		return fmt.Errorf("start sync: %w", err)
	}
	fail := func(e error) error {
		_ = s.store.FailSync(syncID, e.Error())
		return e
	}

	token := src.SyncCursor.String
	pageToken := ""
	finalToken := ""
	cp := store.Checkpoint{}

	for {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		page, err := s.client.ListEvents(ctx, cal.ID, gcal.EventsListParams{
			SyncToken:    token,
			SingleEvents: false,
			ShowDeleted:  true,
			MaxResults:   2500,
			PageToken:    pageToken,
		})
		if err != nil {
			var gone *gcal.GoneError
			if errors.As(err, &gone) {
				_ = s.store.FailSync(syncID, ErrSyncTokenExpired.Error())
				return ErrSyncTokenExpired
			}
			return fail(fmt.Errorf("events.list: %w", err))
		}

		for i := range page.Items {
			ev := page.Items[i]
			added, cancelled, perr := s.persistOne(src.ID, cal, ev, result)
			if perr != nil {
				cp.ErrorsCount++
				s.recordItemError(syncID, ev.ID, perr)
				continue
			}
			cp.MessagesProcessed++
			if added {
				cp.MessagesAdded++
			}
			if cancelled {
				cp.MessagesUpdated++
			}
		}

		if page.NextSyncToken != "" {
			finalToken = page.NextSyncToken
		}
		pageToken = page.NextPageToken
		cp.PageToken = pageToken
		if err := s.store.UpdateSyncCheckpoint(syncID, &cp); err != nil {
			return fail(fmt.Errorf("checkpoint: %w", err))
		}
		if pageToken == "" {
			break
		}
	}

	// If any event failed to persist, do NOT advance the cursor. The Calendar
	// syncToken only re-delivers CHANGED events, so silently swallowing a failure
	// would drop that event from the archive forever. Leaving the cursor
	// unadvanced (and failing the run) means the next incremental sync re-delivers
	// the same delta and retries the failed events, while the events that did
	// persist re-upsert idempotently — mirroring the full-sync path's
	// fail-and-resume rather than skip. A genuinely poison event then surfaces as
	// a repeated, visible per-item error instead of silent data loss.
	if cp.ErrorsCount > 0 {
		return fail(fmt.Errorf("%d calendar event(s) failed to persist; sync token not advanced so they retry on the next sync", cp.ErrorsCount))
	}

	if finalToken != "" {
		if err := s.store.UpdateSourceSyncCursor(src.ID, finalToken); err != nil {
			return fail(fmt.Errorf("update cursor: %w", err))
		}
	}
	if err := s.store.CompleteSync(syncID, finalToken); err != nil {
		return fail(fmt.Errorf("complete sync: %w", err))
	}
	if err := s.store.RecomputeConversationStats(src.ID); err != nil {
		s.logger.Warn("recompute conversation stats failed", "calendar", cal.ID, "error", err)
	}
	_ = s.store.CheckpointWAL()
	s.logger.Info("calendar incremental sync complete",
		"calendar", cal.ID, "events_processed", cp.MessagesProcessed,
		"events_added", cp.MessagesAdded, "events_cancelled", cp.MessagesUpdated,
		"errors", cp.ErrorsCount)
	return nil
}

func (s *Syncer) recordItemError(syncID int64, eventID string, err error) {
	_ = s.store.RecordSyncRunItem(store.SyncRunItem{
		SyncRunID:       syncID,
		SourceMessageID: eventID,
		Phase:           "ingest",
		Status:          store.SyncRunItemStatusError,
		ErrorKind:       "calendar_ingest_error",
		ErrorMessage:    err.Error(),
	})
}

// parseSourceConfig decodes a source's sync_config JSON into sourceConfig.
func parseSourceConfig(cfg sql.NullString) sourceConfig {
	var c sourceConfig
	if cfg.Valid && cfg.String != "" {
		_ = json.Unmarshal([]byte(cfg.String), &c)
	}
	return c
}
