package telegram

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSyncStateRejectsCatalogPeerMismatch(t *testing.T) {
	state := NewSyncState()
	state.Dialogs["-123"] = DialogState{Frontier: 42, History: &HistoryProgress{Ceiling: 100, Offset: 80, MinID: 42}, Peer: &DialogPeer{NativeID: 123, Kind: "group", Title: "Synthetic", TopMessage: 100, EnabledByDefault: true}}
	state.Dialogs["456"] = state.Dialogs["-123"]
	_, err := state.Marshal()
	require.ErrorContains(t, err, "peer")
}

func TestSyncStateRejectsInvalidHistoryRanges(t *testing.T) {
	for _, progress := range []*HistoryProgress{
		{Ceiling: 10, Ranges: []HistoryRange{{MinID: 4, MaxID: 4}}},
		{Ceiling: 10, Ranges: []HistoryRange{{MinID: -1, MaxID: 4}}},
		{Ceiling: 10, Ranges: []HistoryRange{{MinID: 1, MaxID: 4}}, RangeIndex: 1},
		{Ceiling: 10, RangeIndex: 1},
	} {
		state := NewSyncState()
		state.Dialogs["-123"] = DialogState{History: progress}
		_, err := state.Marshal()
		require.Error(t, err)
	}
}

func TestSyncStateRejectsInvalidOrUnknownCheckpoints(t *testing.T) {
	for _, blob := range []string{
		`{"version":2,"dialogs":{}}`,
		`{"version":1,"dialogs":{"-123":{"frontier":42,"history":{"ceiling":100,"offset":101}}}}`,
		`{"version":1,"dialogs":{},"unknown":true}`,
		`{"version":1,"dialogs":{}} {}`,
	} {
		_, err := LoadSyncState(blob)
		require.Error(t, err, blob)
	}
}

func TestSyncStateRejectsOutOfRangeCommonPositions(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("int cannot represent out-of-range Telegram positions")
	}
	tooLarge := int64(1) << 31
	for _, common := range []*CommonUpdateState{
		{Pts: int(tooLarge), Date: 1}, {Qts: int(tooLarge), Date: 1},
		{Date: int(tooLarge)}, {Date: 1, Seq: int(tooLarge)},
	} {
		state := NewSyncState()
		state.Common = common
		_, err := state.Marshal()
		require.Error(t, err)
	}
}

func TestSyncStateRecordsRecoveryGapsWithoutMutatingPriorState(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	state := NewSyncState()
	now := time.Unix(200, 0).UTC()
	since := time.Unix(100, 0).UTC()
	next, err := state.WithRecoveryGap("common", now, since)
	require.NoError(err)
	assert.Empty(state.Gaps)
	encoded, err := next.Marshal()
	require.NoError(err)
	loaded, err := LoadSyncState(encoded)
	require.NoError(err)
	assert.Equal(next.Gaps, loaded.Gaps)
	updated, err := loaded.WithRecoveryGap("common", now.Add(time.Second), since.Add(time.Second))
	require.NoError(err)
	assert.Equal(1, loaded.Gaps["common"].Count)
	assert.Equal(2, updated.Gaps["common"].Count)
	assert.Equal(now, updated.Gaps["common"].FirstDetected)
	_, err = updated.WithRecoveryGap("common", now, since)
	require.Error(err)
	_, err = state.WithRecoveryGap("456", now, since)
	require.Error(err)
}
