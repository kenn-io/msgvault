package query

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
)

func TestSearchFastWithStatsMarkerInvalidatedAfterReadiness(t *testing.T) {
	for _, mode := range []string{"missing", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)

			b := NewTestDataBuilder(t)
			source := b.AddSourceWithType("owner@example.test", "imessage")
			b.AddMessage(MessageOpt{MessageType: "imessage", SourceID: source})
			dir, cleanup := b.Build()
			t.Cleanup(cleanup)
			engine, err := NewDuckDBEngine(dir, "", nil)
			requirements.NoError(err)
			t.Cleanup(func() { require.NoError(t, engine.Close()) })
			state, err := ReadCacheSyncState(dir)
			requirements.NoError(err)
			state.PublishedAt = state.PublishedAt.Add(time.Second)
			data, err := json.Marshal(state)
			requirements.NoError(err)
			requirements.NoError(os.WriteFile(CacheStatePath(dir), data, 0600))
			original := inspectDatasetFingerprint
			t.Cleanup(func() { inspectDatasetFingerprint = original })
			invoked := false
			inspectDatasetFingerprint = func(root string) (string, error) {
				fingerprint, err := original(root)
				if err != nil {
					return fingerprint, err
				}
				invoked = true
				if mode == "missing" {
					err = os.Remove(CacheStatePath(root))
				} else {
					err = os.WriteFile(CacheStatePath(root), []byte("{"), 0600)
				}
				return fingerprint, err
			}
			_, err = engine.SearchFastWithStats(t.Context(), search.Parse("needle"), "needle", MessageFilter{}, ViewNoStats, 10, 0)
			requirements.True(invoked, "remove the real marker after readiness reads it")
			requirements.ErrorIs(err, ErrCacheUnavailable)
		})
	}
}
