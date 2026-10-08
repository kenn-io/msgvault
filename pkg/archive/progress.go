package archive

import (
	"context"
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/discord"
	"go.kenn.io/msgvault/internal/slack"
	"go.kenn.io/msgvault/internal/store"
)

// Progress describes the most recent run and its durable provider checkpoint.
// A completed run is not proof that every provider message was available. The
// host combines this evidence with selection, catalog, and credential status.
// These are read-only snapshots; callers cannot replace stored checkpoints.
type Progress struct {
	Status            string           `json:"status"`
	StartedAt         time.Time        `json:"started_at,omitzero"`
	CompletedAt       *time.Time       `json:"completed_at,omitzero"`
	MessagesProcessed int64            `json:"messages_processed"`
	MessagesAdded     int64            `json:"messages_added"`
	Errors            int64            `json:"errors"`
	Slack             *SlackProgress   `json:"slack,omitzero"`
	Discord           *DiscordProgress `json:"discord,omitzero"`
}

// SlackProgress includes history, pending replies, and per-channel audit/sweep coverage.
type SlackProgress = slack.SyncState

// DiscordProgress includes independent parent/thread backfill and retry state.
type DiscordProgress = discord.SyncState

// Progress returns persisted evidence without starting or recovering a run.
func (a *Archive) Progress(ctx context.Context, sourceID int64) (Progress, error) {
	source, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return Progress{}, err
	}
	run, err := a.store.GetLatestSyncContext(ctx, sourceID, 0)
	if errors.Is(err, store.ErrSyncRunNotFound) {
		return Progress{Status: "not_started"}, nil
	}
	if err != nil {
		return Progress{}, err
	}
	progress := Progress{Status: run.Status, StartedAt: run.StartedAt, MessagesProcessed: run.MessagesProcessed, MessagesAdded: run.MessagesAdded, Errors: run.ErrorsCount}
	if run.CompletedAt.Valid {
		progress.CompletedAt = &run.CompletedAt.Time
	}
	checkpoint := run.CursorBefore.String
	if run.CursorAfter.Valid {
		checkpoint = run.CursorAfter.String
	}
	switch source.SourceType {
	case "slack":
		progress.Slack, err = slack.LoadSyncState(checkpoint)
	case "discord":
		progress.Discord, err = discord.LoadSyncState(checkpoint)
	}
	return progress, err
}
