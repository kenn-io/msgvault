package freshness

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Build evaluates a supplied inventory, including configured sources which do
// not have archive rows. Failed/cancelled/paused attempts warn independently
// of freshness: a recent completed sync can still be within the age threshold.
func Build(ctx context.Context, in Inputs) (Report, error) {
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if in.StaleAfter <= 0 {
		return Report{}, errors.New("freshness: stale threshold must be positive")
	}
	if in.Now.IsZero() {
		return Report{}, errors.New("freshness: as-of time is required")
	}
	r := Report{AsOf: in.Now.UTC(), StaleAfterSeconds: int64(in.StaleAfter / time.Second),
		Warnings: []string{}, Sources: []SourceFreshness{}}
	sources := slices.Clone(in.Sources)
	slices.SortStableFunc(sources, func(a, b SourceInventoryItem) int {
		if c := cmp.Compare(a.SourceType, b.SourceType); c != 0 {
			return c
		}
		return cmp.Compare(a.Identifier, b.Identifier)
	})
	for _, item := range sources {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		s := SourceFreshness{SourceID: item.SourceID, SourceType: item.SourceType, Identifier: item.Identifier,
			Scheduling: item.Scheduling, LastSuccessfulSyncAt: item.LastSuccessfulSyncAt, LastSyncErrors: item.LastSyncErrors,
			Outcome: item.Outcome, SyncCommand: item.SyncCommand}
		if s.Outcome == "" {
			s.Outcome = "never"
		}
		if s.Scheduling == "" {
			s.Scheduling = "manual"
		}
		if s.Scheduling == "disabled" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("Sync is disabled for %s source %s.", s.SourceType, s.Identifier))
		}
		switch s.Outcome {
		case "failed", "cancelled", "paused":
			r.Warnings = append(r.Warnings, fmt.Sprintf("The latest sync %s for %s source %s.", s.Outcome, s.SourceType, s.Identifier))
		}
		if s.Scheduling != "static" && s.Scheduling != "disabled" {
			switch {
			case s.Outcome == "unmeasured":
				s.Reason = "unmeasured"
			case s.LastSuccessfulSyncAt == nil:
				s.Reason = "never_synced"
			case in.Now.Sub(*s.LastSuccessfulSyncAt) > in.StaleAfter:
				s.Reason = "stale"
			}
		}
		r.Sources = append(r.Sources, s)
	}
	return r, nil
}
