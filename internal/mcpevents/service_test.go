package mcpevents

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDecodeSubscribeRejectsInvalidArgumentsAndNulls(t *testing.T) {
	for _, raw := range []string{
		`{"name":"msgvault.messages.changed","arguments":{"person_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`,
		`{"name":"msgvault.messages.changed","arguments":{"person_id":null},"delivery":{}}`,
		`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{}}`,
		`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"unknown":true},"delivery":{}}`,
	} {
		_, err := DecodeSubscribe([]byte(raw))
		assert.Error(t, err, raw)
	}
}

func TestTTLClampAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{{`{}`, 24 * time.Hour}, {`{"ttlMs":null}`, 30 * 24 * time.Hour}, {`{"ttlMs":1}`, time.Minute}, {`{"ttlMs":999999999999}`, 30 * 24 * time.Hour}} {
		var request SubscribeRequest
		require.NoError(t, json.Unmarshal([]byte(tc.raw), &request))
		assert.Equal(t, tc.want, request.TTL.Duration())
	}
}

func TestSubscribeRefreshPreservesPosition(t *testing.T) {
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true}
	input := SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	first, err := service.Subscribe(t.Context(), input)
	require.NoError(t, err)
	key := store.MCPEventsPositionKey(first.ID)
	pos, raw, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(t, err)
	require.NotNil(t, raw)
	pos.Failures = 2
	ok, err := st.AdvanceWakeupStateKey(t.Context(), key, raw, pos)
	require.NoError(t, err)
	require.True(t, ok)
	second, err := service.Subscribe(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)
	got, _, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(t, err)
	assert.Equal(t, pos, got)
}
