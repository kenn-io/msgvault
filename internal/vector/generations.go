package vector

import (
	"context"
	"errors"
	"fmt"
)

// ResolveActiveForFingerprint returns the active generation if its
// fingerprint matches the supplied one. Production callers pass
// Config.GenerationFingerprint() so the preprocessing policy is part
// of the staleness check; a mismatch means an operator changed the
// model, dimension, or any preprocess toggle without rebuilding the
// index, and the active vectors are no longer comparable to what new
// queries would embed.
//
// Error semantics:
//   - ErrIndexStale: an active generation exists but its fingerprint
//     does not match the supplied one.
//   - ErrIndexBuilding: no active yet, but a first-ever build is in
//     progress.
//   - ErrNotEnabled: no generation exists at all (vector search not
//     initialized).
//
// Any other error from the Backend is wrapped and returned as-is.
func ResolveActiveForFingerprint(ctx context.Context, b Backend, fingerprint string) (Generation, error) {
	active, err := b.ActiveGeneration(ctx)
	if err == nil {
		if fingerprint != "" && active.Fingerprint != fingerprint {
			return Generation{}, fmt.Errorf("%w: active=%q configured=%q",
				ErrIndexStale, active.Fingerprint, fingerprint)
		}
		return active, nil
	}
	if !errors.Is(err, ErrNoActiveGeneration) {
		return Generation{}, fmt.Errorf("active generation: %w", err)
	}
	// No active generation. Check for a building one to distinguish
	// "first-time build" from "nothing configured".
	building, bErr := b.BuildingGeneration(ctx)
	if bErr != nil {
		return Generation{}, fmt.Errorf("building generation: %w", bErr)
	}
	if building != nil {
		return Generation{}, ErrIndexBuilding
	}
	return Generation{}, ErrNotEnabled
}
