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

	require.NoError(t, baseline.Merge(checkpoint))

	assert.Equal(t, "10000000000000000000", baseline.Containers["channel"].HighWater, "numeric snowflake maximum")
	assert.Equal(t, "100", baseline.Containers["channel"].BackfillBefore, "newer opaque cursor")
	assert.Equal(t, "1000", baseline.Containers["channel"].BackfillUpper, "checkpoint cannot erase pinned bound")
	assert.Equal(t, "80", baseline.Containers["complete"].HighWater, "high-water cannot regress")
	assert.True(t, baseline.Containers["complete"].BackfillComplete, "completion cannot regress")
	assert.Equal(t, "50", baseline.Containers["new"].HighWater)
	assert.Equal(t, "2026-07-19T12:00:00Z", baseline.ThreadCatalog["parent"].PublicArchiveWatermark, "catalog watermark cannot regress")
	assert.Equal(t, "2026-07-19T13:00:00Z", baseline.ThreadCatalog["parent"].PrivateArchiveWatermark, "catalog watermark advances")
}

func TestSyncStateMergeRejectsMalformedState(t *testing.T) {
	tests := []struct {
		name       string
		mutateBase func(*SyncState)
		mutateNext func(*SyncState)
	}{
		{
			name: "invalid baseline snowflake",
			mutateBase: func(state *SyncState) {
				state.Containers["channel"] = ContainerState{HighWater: "invalid"}
			},
		},
		{
			name: "invalid checkpoint timestamp",
			mutateNext: func(state *SyncState) {
				state.ThreadCatalog["channel"] = ThreadCatalogState{PublicArchiveWatermark: "invalid"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := NewSyncState()
			checkpoint := NewSyncState()
			if tt.mutateBase != nil {
				tt.mutateBase(baseline)
			}
			if tt.mutateNext != nil {
				tt.mutateNext(checkpoint)
			}

			require.Error(t, baseline.Merge(checkpoint))
		})
	}
}

func TestLoadSyncStateRejectsMalformedState(t *testing.T) {
	tests := []struct {
		name       string
		blob       string
		wantDetail string
	}{
		{name: "invalid JSON", blob: "{not json"},
		{name: "missing version", blob: `{"containers":{}}`},
		{name: "unsupported version", blob: `{"version":2,"containers":{},"thread_catalog":{}}`},
		{name: "trailing JSON", blob: `{"version":1} {"version":1}`},
		{
			name:       "invalid high-water digits",
			blob:       `{"version":1,"containers":{"channel":{"high_water":"12x"}}}`,
			wantDetail: `containers["channel"].high_water`,
		},
		{
			name:       "high-water uint64 overflow",
			blob:       `{"version":1,"containers":{"channel":{"high_water":"18446744073709551616"}}}`,
			wantDetail: `containers["channel"].high_water`,
		},
		{
			name:       "invalid backfill-before digits",
			blob:       `{"version":1,"containers":{"channel":{"backfill_before":"-1"}}}`,
			wantDetail: `containers["channel"].backfill_before`,
		},
		{
			name:       "backfill-upper uint64 overflow",
			blob:       `{"version":1,"containers":{"channel":{"backfill_upper":"18446744073709551616"}}}`,
			wantDetail: `containers["channel"].backfill_upper`,
		},
		{
			name:       "invalid public archive timestamp",
			blob:       `{"version":1,"thread_catalog":{"channel":{"public_archive_watermark":"not-a-time"}}}`,
			wantDetail: `thread_catalog["channel"].public_archive_watermark`,
		},
		{
			name:       "invalid private archive timestamp",
			blob:       `{"version":1,"thread_catalog":{"channel":{"private_archive_watermark":"2026-99-99"}}}`,
			wantDetail: `thread_catalog["channel"].private_archive_watermark`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadSyncState(tt.blob)
			require.Error(t, err)
			if tt.wantDetail != "" {
				assert.Contains(t, err.Error(), tt.wantDetail)
			}
		})
	}
}

func TestSyncStateMarshalRejectsMalformedState(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*SyncState)
		wantDetail string
	}{
		{
			name: "invalid high-water digits",
			mutate: func(state *SyncState) {
				state.Containers["channel"] = ContainerState{HighWater: "12x"}
			},
			wantDetail: `containers["channel"].high_water`,
		},
		{
			name: "backfill-before uint64 overflow",
			mutate: func(state *SyncState) {
				state.Containers["channel"] = ContainerState{BackfillBefore: "18446744073709551616"}
			},
			wantDetail: `containers["channel"].backfill_before`,
		},
		{
			name: "invalid backfill-upper digits",
			mutate: func(state *SyncState) {
				state.Containers["channel"] = ContainerState{BackfillUpper: "-1"}
			},
			wantDetail: `containers["channel"].backfill_upper`,
		},
		{
			name: "invalid public archive timestamp",
			mutate: func(state *SyncState) {
				state.ThreadCatalog["channel"] = ThreadCatalogState{PublicArchiveWatermark: "not-a-time"}
			},
			wantDetail: `thread_catalog["channel"].public_archive_watermark`,
		},
		{
			name: "invalid private archive timestamp",
			mutate: func(state *SyncState) {
				state.ThreadCatalog["channel"] = ThreadCatalogState{PrivateArchiveWatermark: "2026-99-99"}
			},
			wantDetail: `thread_catalog["channel"].private_archive_watermark`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := NewSyncState()
			tt.mutate(state)

			_, err := state.Marshal()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantDetail)
		})
	}
}
