package vector

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// StatsView is the serialization-ready snapshot of vector-search state
// attached to /api/v1/stats and MCP get_stats responses. Callers use
// omitempty on the containing field when they want the sub-object to
// disappear entirely in the disabled case; see CollectStats, which
// returns nil for that case so the outer JSON tag drives the shape.
type StatsView struct {
	// Enabled reports whether a vector Backend was wired in. Always
	// true in a non-nil *StatsView.
	Enabled bool `json:"enabled"`

	// ActiveGeneration describes the currently-serving index. nil when
	// no active generation exists yet (normal during first build) or
	// when Backend.Stats failed for that generation.
	ActiveGeneration *GenerationSummary `json:"active_generation"`

	// BuildingGeneration describes an in-progress rebuild, if any.
	// Omitted entirely when no build is running.
	BuildingGeneration *BuildingSummary `json:"building_generation,omitempty"`

	// PendingEmbeddingsTotal is the sum of pending_embeddings rows
	// across the active and building generations. Retired generations
	// are assumed to have zero pending items.
	PendingEmbeddingsTotal int64 `json:"pending_embeddings_total"`
}

// GenerationSummary reports the serving state for the active index
// generation.
type GenerationSummary struct {
	ID           GenerationID `json:"id"`
	Model        string       `json:"model"`
	Dimension    int          `json:"dimension"`
	Fingerprint  string       `json:"fingerprint"`
	State        string       `json:"state"`
	ActivatedAt  string       `json:"activated_at,omitempty"` // RFC3339 UTC
	MessageCount int64        `json:"message_count"`
}

// BuildingSummary reports progress for an in-flight rebuild.
type BuildingSummary struct {
	ID        GenerationID `json:"id"`
	Model     string       `json:"model"`
	Dimension int          `json:"dimension"`
	StartedAt string       `json:"started_at,omitempty"` // RFC3339 UTC
	Progress  Progress     `json:"progress"`
}

// Progress reports the build-queue position for a generation. Done is
// the count of already-embedded messages; Total is Done plus the
// currently-pending queue depth.
type Progress struct {
	Done  int64 `json:"done"`
	Total int64 `json:"total"`
}

// CollectStats assembles a StatsView for the stats endpoints. Returns
// (nil, nil) when the backend is nil (vector search disabled); callers
// can attach the result directly to a response field tagged with
// omitempty.
//
// Partial failures are tolerated so one broken sub-query doesn't blank
// the whole stats response. When Backend.Stats fails for a generation,
// that generation's summary is left nil and the failure is joined into
// the returned error via errors.Join — callers get both the usable
// envelope and a full error log.
//
// ErrNoActiveGeneration from ActiveGeneration is NOT an error; it's
// the expected first-run state. Any other error from ActiveGeneration
// or BuildingGeneration is joined into the returned error.
func CollectStats(ctx context.Context, b Backend) (*StatsView, error) {
	if b == nil {
		return nil, nil //nolint:nilnil // disabled backend -> no stats, not an error
	}
	out := &StatsView{Enabled: true}
	var errs []error

	active, err := b.ActiveGeneration(ctx)
	switch {
	case err == nil:
		s, sErr := b.Stats(ctx, active.ID)
		if sErr != nil {
			errs = append(errs, fmt.Errorf("stats for active generation %d: %w", active.ID, sErr))
		} else {
			out.ActiveGeneration = &GenerationSummary{
				ID:           active.ID,
				Model:        active.Model,
				Dimension:    active.Dimension,
				Fingerprint:  active.Fingerprint,
				State:        string(active.State),
				MessageCount: s.EmbeddingCount,
				ActivatedAt:  formatTimePtr(active.ActivatedAt),
			}
			out.PendingEmbeddingsTotal += s.PendingCount
		}
	case errors.Is(err, ErrNoActiveGeneration):
		// Leave ActiveGeneration nil; this is normal during first build.
	default:
		errs = append(errs, fmt.Errorf("active generation: %w", err))
	}

	building, err := b.BuildingGeneration(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("building generation: %w", err))
	} else if building != nil {
		s, sErr := b.Stats(ctx, building.ID)
		if sErr != nil {
			errs = append(errs, fmt.Errorf("stats for building generation %d: %w", building.ID, sErr))
		} else {
			out.BuildingGeneration = &BuildingSummary{
				ID:        building.ID,
				Model:     building.Model,
				Dimension: building.Dimension,
				StartedAt: formatTime(building.StartedAt),
				Progress: Progress{
					Done:  s.EmbeddingCount,
					Total: s.EmbeddingCount + s.PendingCount,
				},
			}
			out.PendingEmbeddingsTotal += s.PendingCount
		}
	}
	return out, errors.Join(errs...)
}

// formatTime renders t as RFC3339 UTC, returning "" for the zero value
// so callers can feed the result directly to a field tagged omitempty.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// formatTimePtr dereferences t before formatting; returns "" for nil.
func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}
