package discord

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// Persisted classification must survive serialization and checkpoint merge:
// neither a newer historical checkpoint nor an interrupted live window may
// inherit the baseline's opposite classification.
func FuzzDiscordEventCheckpointRoundTrip(f *testing.F) {
	f.Add(uint64(501), uint64(300), true, true)
	f.Add(uint64(1), uint64(301), false, false)
	f.Fuzz(func(t *testing.T, message, container uint64, covered, live bool) {
		require := require.New(t)
		assert := assert.New(t)
		id := strconv.FormatUint(max(container, 1), 10)
		mode := "history"
		want := store.IngestBackfill
		if covered && live {
			mode = "live"
			want = store.IngestLive
		}
		state := NewSyncState()
		state.Containers[id] = ContainerState{HighWater: strconv.FormatUint(max(message, 1), 10), BackfillComplete: covered, RetryRequired: true, EventsCovered: covered, EventsForwardMode: mode}
		blob, err := state.Marshal()
		require.NoError(err)
		decoded, err := LoadSyncState(blob)
		require.NoError(err)
		assert.Equal(state, decoded)
		baseline := NewSyncState()
		baseline.Containers[id] = ContainerState{HighWater: "1", EventsCovered: !covered, EventsForwardMode: "live"}
		require.NoError(baseline.Merge(decoded))
		assert.Equal(state.Containers[id], baseline.Containers[id])
		assert.Equal(want, baseline.Containers[id].forwardIngestContext().Mode)
	})
}

func TestDiscordEventCheckpointRejectsUnknownMode(t *testing.T) {
	require := require.New(t)
	_, err := LoadSyncState(`{"version":1,"containers":{"300":{"events_forward_mode":"unrecognized"}},"thread_catalog":{}}`)
	require.Error(err)
}
