package discord

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRoundTrip(t *testing.T) {
	state := NewSyncState()
	state.Containers["channel"] = ContainerState{
		HighWater:        "123456789012345678",
		BackfillBefore:   "100000000000000000",
		BackfillUpper:    "123456789012345678",
		BackfillComplete: false,
	}
	state.ThreadCatalog["channel"] = ThreadCatalogState{
		PublicArchiveWatermark:  "2026-07-18T12:00:00.123Z",
		PrivateArchiveWatermark: "2026-07-17T12:00:00Z",
	}

	blob, err := state.Marshal()
	require.NoError(t, err)
	loaded, err := LoadSyncState(blob)
	require.NoError(t, err)
	assert.Equal(t, state, loaded)
}

func TestLoadSyncStateInitializesEmptyState(t *testing.T) {
	state, err := LoadSyncState("")
	require.NoError(t, err)
	assert.Equal(t, SyncStateVersion, state.Version)
	assert.NotNil(t, state.Containers)
	assert.NotNil(t, state.ThreadCatalog)
}

func TestSyncStateMerge(t *testing.T) {
	baseline := NewSyncState()
	baseline.Containers["channel"] = ContainerState{
		HighWater:      "9000000000000000000",
		BackfillBefore: "200",
		BackfillUpper:  "1000",
	}
	baseline.Containers["complete"] = ContainerState{
		HighWater:        "80",
		BackfillComplete: true,
	}
	baseline.ThreadCatalog["parent"] = ThreadCatalogState{
		PublicArchiveWatermark:  "2026-07-19T12:00:00Z",
		PrivateArchiveWatermark: "2026-07-18T12:00:00Z",
	}

	checkpoint := NewSyncState()
	checkpoint.Containers["channel"] = ContainerState{
		HighWater:      "10000000000000000000",
		BackfillBefore: "100",
	}
	checkpoint.Containers["complete"] = ContainerState{
		HighWater:        "70",
		BackfillBefore:   "40",
		BackfillUpper:    "90",
		BackfillComplete: false,
	}
	checkpoint.Containers["new"] = ContainerState{HighWater: "50"}
	checkpoint.ThreadCatalog["parent"] = ThreadCatalogState{
		PublicArchiveWatermark:  "2026-07-19T11:00:00Z",
		PrivateArchiveWatermark: "2026-07-19T13:00:00Z",
	}

	baseline.Merge(checkpoint)

	assert.Equal(t, "10000000000000000000", baseline.Containers["channel"].HighWater, "numeric snowflake maximum")
	assert.Equal(t, "100", baseline.Containers["channel"].BackfillBefore, "newer opaque cursor")
	assert.Equal(t, "1000", baseline.Containers["channel"].BackfillUpper, "checkpoint cannot erase pinned bound")
	assert.Equal(t, "80", baseline.Containers["complete"].HighWater, "high-water cannot regress")
	assert.True(t, baseline.Containers["complete"].BackfillComplete, "completion cannot regress")
	assert.Equal(t, "50", baseline.Containers["new"].HighWater)
	assert.Equal(t, "2026-07-19T12:00:00Z", baseline.ThreadCatalog["parent"].PublicArchiveWatermark, "catalog watermark cannot regress")
	assert.Equal(t, "2026-07-19T13:00:00Z", baseline.ThreadCatalog["parent"].PrivateArchiveWatermark, "catalog watermark advances")
}

func TestLoadSyncStateRejectsMalformedState(t *testing.T) {
	tests := []struct {
		name string
		blob string
	}{
		{name: "invalid JSON", blob: "{not json"},
		{name: "missing version", blob: `{"containers":{}}`},
		{name: "unsupported version", blob: `{"version":2,"containers":{},"thread_catalog":{}}`},
		{name: "trailing JSON", blob: `{"version":1} {"version":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSyncState(tt.blob)
			require.Error(t, err)
		})
	}
}
