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
		StaleReasons: []string{}, Warnings: []string{}, Sources: []SourceFreshness{}, Projections: map[string]*ProjectionFreshness{}}
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
			Outcome: item.Outcome, SyncCommand: item.SyncCommand, AgeSeconds: age(in.Now, item.LastSuccessfulSyncAt)}
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
			prefix := ""
			switch {
			case s.Outcome == "unmeasured":
				s.Reason = "unmeasured"
				prefix = "source_unmeasured:"
			case s.LastSuccessfulSyncAt == nil:
				s.Reason = "never_synced"
				prefix = "source_never_synced:"
			case in.Now.Sub(*s.LastSuccessfulSyncAt) > in.StaleAfter:
				s.Reason = "stale"
				prefix = "source_stale:"
			}
			if prefix != "" {
				s.Stale = true
				r.Stale = true
				r.StaleReasons = append(r.StaleReasons, prefix+s.Identifier)
			}
		}
		r.Sources = append(r.Sources, s)
	}
	names := make([]string, 0, len(in.Projections))
	for name := range in.Projections {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		state := in.Projections[name]
		p := &ProjectionFreshness{ComputedAt: state.ComputedAt, LastSuccessAt: state.LastSuccessAt,
			AgeSeconds: age(in.Now, state.ComputedAt), BackfillComplete: state.BackfillComplete, Extra: state.Extra}
		switch {
		case state.ComputedAt == nil:
			p.Stale = true
			r.StaleReasons = append(r.StaleReasons, "projection_missing:"+name)
		case in.Now.Sub(*state.ComputedAt) > in.StaleAfter:
			p.Stale = true
			r.StaleReasons = append(r.StaleReasons, "projection_stale:"+name)
		}
		if p.Stale {
			r.Stale = true
		}
		if state.BackfillComplete != nil && !*state.BackfillComplete {
			r.Warnings = append(r.Warnings, fmt.Sprintf("The %s projection is still backfilling archive evidence.", name))
		}
		if reason, _ := state.Extra["rescan_reason"].(string); reason != "" {
			r.Warnings = append(r.Warnings, fmt.Sprintf("The %s projection is reconciling archive changes.", name))
		}
		r.Projections[name] = p
	}
	return r, nil
}

func age(now time.Time, at *time.Time) *int64 {
	if at == nil {
		return nil
	}
	seconds := max(int64(0), int64(now.Sub(*at)/time.Second))
	return &seconds
}
