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
	result := Freshness{AsOf: now, StaleThreshold: report.StaleAfterSeconds, Sources: []freshness.SourceFreshness{}, Projections: []ProjectionFreshness{}, Warnings: report.Warnings}
	for _, source := range report.Sources {
		if source.Reason != "" || source.Outcome == "failed" || source.Outcome == "cancelled" || source.Outcome == "paused" || source.Scheduling == "disabled" || source.LastSyncErrors > 0 {
			result.Sources = append(result.Sources, source)
		}
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
