package mcpevents

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func preparedSubscribe(t *testing.T, in SubscribeRequest) SubscribeRequest {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	out, err := DecodeSubscribe(raw)
	require.NoError(t, err)
	return out
}

func TestEventsKeyIsCreatedOnlyBySubscribeAndExpiryCleansWithoutIt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	path := filepath.Join(service.Home, "mcp-events.key")
	assert.True(service.Available(t.Context()))
	_, err := service.Native(t.Context())
	require.NoError(err)
	assert.NoFileExists(path)
	in, err := DecodeSubscribe([]byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`))
	require.NoError(err)
	_, err = service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.Remove(path))
	assert.False(service.Available(t.Context()))
	unsubscribe, err := DecodeUnsubscribe([]byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook"}}`))
	require.NoError(err)
	require.NoError(service.Unsubscribe(t.Context(), unsubscribe))
	assert.True(service.Available(t.Context()))
	_, err = service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.Remove(path))
	now = now.Add(25 * time.Hour)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	assert.Empty(targets)
	assert.True(service.Available(t.Context()))
	assert.NoFileExists(path)
}

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
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true}
	input := SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	input = preparedSubscribe(t, input)
	first, err := service.Subscribe(t.Context(), input)
	require.NoError(err)
	key := store.MCPEventsPositionKey(first.ID)
	pos, raw, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	require.NotNil(raw)
	pos.Failures = 2
	ok, err := st.AdvanceWakeupStateKey(t.Context(), key, raw, pos)
	require.NoError(err)
	require.True(ok)
	second, err := service.Subscribe(t.Context(), input)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	got, _, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	assert.Equal(pos, got)
}

func TestExpiredSubscriptionRemovedWithoutDelivery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	in = preparedSubscribe(t, in)
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	now = now.Add(25 * time.Hour)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	assert.Empty(targets)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	require.NoError(err)
	assert.Nil(position)
}

func TestRefreshReturnsCategorizedDeliveryStatus(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true}
	in := SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	in = preparedSubscribe(t, in)
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	key := store.MCPEventsPositionKey(first.ID)
	state, raw, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	stamp := time.Now().UTC().Truncate(time.Second)
	state.Failures = 1
	state.LastError = "http_5xx"
	state.FailedSince = &stamp
	ok, err := st.AdvanceWakeupStateKey(t.Context(), key, raw, state)
	require.NoError(err)
	require.True(ok)
	refreshed, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Equal("http_5xx", refreshed.DeliveryStatus.LastError)
	require.NotNil(refreshed.DeliveryStatus.FailedSince)
	assert.Equal(stamp, *refreshed.DeliveryStatus.FailedSince)
}

func TestConcurrentFinalSubscriptionUsesSharedLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, ConfigTargets: 31}
	makeRequest := func(url string) SubscribeRequest {
		return preparedSubscribe(t, SubscribeRequest{Name: Name, Arguments: Arguments{SourceID: 1}, Delivery: Delivery{Mode: "webhook", URL: url, Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	}
	results := make(chan error, 2)
	for _, url := range []string{"https://example.com/one", "https://example.com/two"} {
		request := makeRequest(url)
		go func() { _, err := service.Subscribe(t.Context(), request); results <- err }()
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
