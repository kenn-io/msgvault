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
		`{"name":null,"arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`,
		`{"name":"msgvault.messages.changed","arguments":{"person_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`,
		`{"name":"msgvault.messages.changed","arguments":{"person_id":null},"delivery":{}}`,
		`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{}}`,
		`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"unknown":true},"delivery":{}}`,
	} {
		_, err := DecodeSubscribe([]byte(raw))
		assert.Error(t, err, raw)
		var eventErr *Error
		require.ErrorAs(t, err, &eventErr)
		assert.Equal(t, InvalidParams, eventErr.Code)
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

func TestExpiredSubscriptionRemovedWithoutDelivery(t *testing.T) {
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(t, err)
	now = now.Add(25 * time.Hour)
	targets, err := service.Native(t.Context())
	require.NoError(t, err)
	assert.Empty(t, targets)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	require.NoError(t, err)
	assert.Nil(t, position)
}

func TestRefreshReturnsCategorizedDeliveryStatus(t *testing.T) {
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true}
	in := SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(t, err)
	key := store.MCPEventsPositionKey(first.ID)
	state, raw, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(t, err)
	stamp := time.Now().UTC().Truncate(time.Second)
	state.Failures = 1
	state.LastError = "http_5xx"
	state.FailedSince = &stamp
	ok, err := st.AdvanceWakeupStateKey(t.Context(), key, raw, state)
	require.NoError(t, err)
	require.True(t, ok)
	refreshed, err := service.Subscribe(t.Context(), in)
	require.NoError(t, err)
	assert.Equal(t, "http_5xx", refreshed.DeliveryStatus.LastError)
	require.NotNil(t, refreshed.DeliveryStatus.FailedSince)
	assert.Equal(t, stamp, *refreshed.DeliveryStatus.FailedSince)
}

func TestConcurrentFinalSubscriptionUsesSharedLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, ConfigTargets: 31}
	makeRequest := func(url string) SubscribeRequest {
		return SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: url, Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	}
	results := make(chan error, 2)
	for _, url := range []string{"https://example.com/one", "https://example.com/two"} {
		go func() { _, err := service.Subscribe(t.Context(), makeRequest(url)); results <- err }()
	}
	var success, exhausted int
	for range 2 {
		err := <-results
		if err == nil {
			success++
			continue
		}
		var eventErr *Error
		require.ErrorAs(t, err, &eventErr)
		if eventErr.Code == ResourceExhausted {
			exhausted++
		}
	}
	assert.Equal(t, 1, success)
	assert.Equal(t, 1, exhausted)
}
