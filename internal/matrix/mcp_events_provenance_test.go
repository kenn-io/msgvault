package matrix

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func matrixEventsCount(t *testing.T, f *eventsMatrixFixture) int {
	t.Helper()
	var count int
	require.NoError(t, f.store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&count))
	return count
}

func TestMCPMatrixNativeCoverage(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{true: "empty", false: "history"}[empty], func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEventsMatrixFixture(t)
			f.emptyBaseline = empty
			require.NoError(f.importRound(t, store.IngestLive))
			assert.Zero(matrixEventsCount(t, f), "initial history must override caller Live")
			f.round.Store(1)
			require.NoError(f.importRound(t, store.IngestBackfill))
			assert.Equal(1, matrixEventsCount(t, f), "native completed coverage determines eligibility")
			require.NoError(f.importRound(t, store.IngestLive))
			assert.Equal(1, matrixEventsCount(t, f), "immutable duplicates stay silent")
		})
	}
}

func TestMCPMatrixFullResetSurvivesFirstRequestFailure(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEventsMatrixFixture(t)
	require.NoError(f.importRound(t, store.IngestBackfill))
	f.round.Store(1)
	f.failSync = true
	_, err := NewImporter(f.store, f.runtime).Import(t.Context(), ImportOptions{UserID: "@archive:example.org", Full: true})
	require.Error(err)
	f.failSync = false
	require.NoError(f.importRound(t, store.IngestLive))
	assert.Zero(matrixEventsCount(t, f), "failed full reset cannot fall back to old live coverage")
	f.round.Store(2)
	require.NoError(f.importRound(t, store.IngestBackfill))
	assert.Equal(1, matrixEventsCount(t, f))
}

func TestMCPMatrixInterruptedFullStaysHistorical(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEventsMatrixFixture(t)
	require.NoError(f.importRound(t, store.IngestBackfill))
	f.round.Store(1)
	f.gap, f.failGap = true, true
	_, err := NewImporter(f.store, f.runtime).Import(t.Context(), ImportOptions{UserID: "@archive:example.org", Full: true})
	require.Error(err)
	f.round.Store(2)
	f.failGap = false
	require.NoError(f.importRound(t, store.IngestLive))
	assert.Zero(matrixEventsCount(t, f), "newer timeline cannot upgrade interrupted full history")
	f.round.Store(3)
	f.gap = false
	require.NoError(f.importRound(t, store.IngestBackfill))
	assert.Equal(1, matrixEventsCount(t, f))
}

func TestMCPMatrixGapSavedBeforeFirstRequest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEventsMatrixFixture(t)
	require.NoError(f.importRound(t, store.IngestBackfill))
	f.round.Store(1)
	f.gap, f.failGap, f.gapMessage = true, true, true
	require.Error(f.importRound(t, store.IngestBackfill))
	assert.Equal(1, matrixEventsCount(t, f), "timeline arrival is independently durable")
	f.round.Store(2)
	f.gap, f.failGap = false, false
	require.NoError(f.importRound(t, store.IngestBackfill))
	assert.Equal(3, matrixEventsCount(t, f), "new timeline and saved live gap both arrive")
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$gap-child"})
	require.NoError(err)
	assert.NotZero(ids["$gap-child"], "first failed gap request must not be forgotten")
}

func TestMCPMatrixLegacyAndUnknownCoverageIsHistorical(t *testing.T) {
	for _, kind := range []string{"legacy", "unknown", "new room"} {
		t.Run(kind, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEventsMatrixFixture(t)
			require.NoError(f.importRound(t, store.IngestBackfill))
			room := map[string]any{"backfilled": true, "synced_to": "sync-0"}
			roomID := "!room:example.org"
			if kind == "unknown" {
				room["events_covered"], room["events_mode"], room["events_since"] = true, "future", "sync-0"
				room["gap_from"], room["gap_to"], room["gap_mode"] = "gap", "sync-0", "live"
				f.gapMessage = true
			}
			if kind == "new room" {
				roomID = "!other:example.org"
			}
			state := map[string]any{"next_batch": "sync-0", "rooms": map[string]any{roomID: room}}
			encoded, err := json.Marshal(state)
			require.NoError(err)
			syncID, err := f.store.StartSync(f.sourceID, SourceType)
			require.NoError(err)
			require.NoError(f.store.FailSyncWithCheckpoint(syncID, "synthetic legacy state", &store.Checkpoint{PageToken: string(encoded)}))
			f.round.Store(1)
			require.NoError(f.importRound(t, store.IngestLive))
			assert.Zero(matrixEventsCount(t, f))
			f.round.Store(2)
			require.NoError(f.importRound(t, store.IngestBackfill))
			assert.Equal(1, matrixEventsCount(t, f))
		})
	}
}

func TestMCPMatrixEncryptedPlaceholderStaysSilent(t *testing.T) {
	require := require.New(t)
	f := newEventsMatrixFixture(t)
	require.NoError(f.importRound(t, store.IngestBackfill))
	f.round.Store(1)
	f.encrypted = true
	require.NoError(f.importRound(t, store.IngestLive))
	assert.Equal(t, 1, matrixEventsCount(t, f))
	ids, err := f.store.MessageExistsBatch(f.sourceID, []string{"$encrypted"})
	require.NoError(err)
	assert.NotZero(t, ids["$encrypted"])
}

func TestMCPMatrixRejectsRuntimeAccountMismatch(t *testing.T) {
	require := require.New(t)
	f := newEventsMatrixFixture(t)
	other, err := f.store.GetOrCreateSource(SourceType, "@other:example.org")
	require.NoError(err)
	_, err = NewImporter(f.store, f.runtime).Import(t.Context(), ImportOptions{UserID: "@other:example.org"})
	require.Error(err)
	count, err := f.store.CountMessagesForSource(other.ID)
	require.NoError(err)
	assert.Zero(t, count)
}

// The oracle counts actual native journal rows across interrupted checkpoint
// round trips. Neither history nor caller context can upgrade old provenance.
func FuzzMCPMatrixCheckpointProvenance(f *testing.F) {
	f.Add(false, false, uint8(0))
	f.Add(true, true, uint8(2))
	f.Fuzz(func(t *testing.T, live, empty bool, repeats uint8) {
		require := require.New(t)
		fixture := newEventsMatrixFixture(t)
		fixture.emptyBaseline = empty
		if live {
			require.NoError(fixture.importRound(t, store.IngestBackfill))
		}
		fixture.round.Store(1)
		fixture.gap, fixture.failGap, fixture.gapMessage = true, true, true
		for range 1 + int(repeats%3) {
			require.Error(fixture.importRound(t, store.IngestLive))
		}
		want := 0
		if live {
			want = 1
		}
		assert.Equal(t, want, matrixEventsCount(t, fixture))
		fixture.failGap, fixture.gap = false, false
		require.NoError(fixture.importRound(t, store.IngestLive))
		if live {
			want = 2
		}
		assert.Equal(t, want, matrixEventsCount(t, fixture))
		fixture.round.Store(2)
		require.NoError(fixture.importRound(t, store.IngestBackfill))
		assert.Equal(t, want+1, matrixEventsCount(t, fixture))
	})
}

func TestMCPMatrixPartialMultiRoomHistoryStaysHistorical(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newEventsMatrixFixture(t)
	f.secondRoom, f.failSecond = true, true
	require.Error(f.importRound(t, store.IngestLive))
	assert.Zero(matrixEventsCount(t, f))
	f.round.Store(1)
	require.Error(f.importRound(t, store.IngestLive))
	assert.Zero(matrixEventsCount(t, f), "completed first room must retain history until the global boundary completes")
	f.failSecond = false
	f.round.Store(2)
	require.NoError(f.importRound(t, store.IngestLive))
	assert.Zero(matrixEventsCount(t, f))
	f.round.Store(3)
	require.NoError(f.importRound(t, store.IngestBackfill))
	assert.Equal(1, matrixEventsCount(t, f))
}

func TestMCPMatrixRoomCoverageBoundaries(t *testing.T) {
	for _, boundary := range []string{"excluded", "left", "quiet"} {
		t.Run(boundary, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEventsMatrixFixture(t)
			require.NoError(f.importRound(t, store.IngestBackfill))
			f.round.Store(1)
			opts := ImportOptions{UserID: "@archive:example.org"}
			switch boundary {
			case "excluded":
				opts.ExcludeRooms = []string{"!room:example.org"}
			case "left":
				f.leave = true
			case "quiet":
				f.quiet = true
			}
			_, err := NewImporter(f.store, f.runtime).Import(t.Context(), opts)
			require.NoError(err)
			assert.Zero(matrixEventsCount(t, f))
			f.leave, f.quiet = false, false
			f.round.Store(2)
			require.NoError(f.importRound(t, store.IngestLive))
			want := 0
			if boundary == "quiet" {
				want = 1
			}
			assert.Equal(want, matrixEventsCount(t, f), "only an absent quiet room retains affirmative coverage")
			f.round.Store(3)
			require.NoError(f.importRound(t, store.IngestBackfill))
			assert.Equal(want+1, matrixEventsCount(t, f))
		})
	}
}
