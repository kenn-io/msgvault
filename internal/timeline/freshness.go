package timeline

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/freshness"
	"go.kenn.io/msgvault/internal/sourceinventory"
	"go.kenn.io/msgvault/internal/store"
)

type storeInventory struct {
	st  *store.Store
	cfg *config.Config
}

func (s storeInventory) Build(ctx context.Context, now time.Time) (Freshness, error) {
	items, err := sourceinventory.Build(ctx, s.cfg, s.st)
	if err != nil {
		return Freshness{}, err
	}
	report, err := freshness.Build(ctx, freshness.Inputs{Sources: items, Now: now, StaleAfter: 24 * time.Hour})
	if err != nil {
		return Freshness{}, err
	}
	state, err := s.st.ActivityProjectionStateContext(ctx)
	if err != nil {
		return Freshness{}, err
	}
	result := Freshness{AsOf: now, StaleThreshold: report.StaleAfterSeconds, Sources: []SourceFreshness{}, Projections: []ProjectionFreshness{}, Warnings: report.Warnings}
	for _, source := range report.Sources {
		status := source.Reason
		if status == "" {
			status = "current"
		}
		last := source.Outcome
		if last != "failed" && last != "cancelled" && last != "paused" {
			last = "ok"
		}
		warnings := []string{}
		if source.Scheduling == "disabled" {
			warnings = append(warnings, "sync disabled")
		}
		if source.LastSyncErrors > 0 {
			warnings = append(warnings, "last successful sync had errors")
		}
		result.Sources = append(result.Sources, SourceFreshness{SourceID: source.SourceID, Source: source.SourceType + ":" + source.Identifier, Status: status, LastRun: last, Scheduling: source.Scheduling, Warnings: warnings, SyncCommand: source.SyncCommand})
	}
	projection := ProjectionFreshness{Name: "activity", State: "current"}
	if state.Queued > 0 {
		projection.State = "incomplete"
		projection.Frontier = state.OldestQueuedAt
	}
	if state.Reconciling {
		projection.State = "reconciling"
	}
	result.Projections = append(result.Projections, projection)
	return result, nil
}
