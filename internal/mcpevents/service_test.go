package mcpevents

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/wakeups"
)

func TestNativeSkipsUnreadableSubscription(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var logs bytes.Buffer
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/one", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	in.Delivery.URL = "https://example.com/two"
	second, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	entries, raw, err := service.catalog(t.Context())
	require.NoError(err)
	entries[0].Secret = []byte("corrupt")
	next, err := json.Marshal(entries)
	require.NoError(err)
	ok, err := st.SwapMCPEventsCatalog(t.Context(), raw, next, "", nil, "")
	require.NoError(err)
	require.True(ok)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 1)
	assert.Equal(second.ID, targets[0].Name)
	assert.Contains(logs.String(), first.ID)
	assert.Contains(logs.String(), "secret_unavailable")
}

func preparedSubscribe(t *testing.T, in MCPEventsSubscribeRequest) MCPEventsSubscribeRequest {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	out, err := DecodeSubscribe(raw)
	require.NoError(t, err)
	return out
}

func TestEventsKeyIsCreatedOnlyBySubscribe(t *testing.T) {
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

func TestDecodeRejectsPresentZeroSelector(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, arguments := range []string{`{"person_id":1,"source_id":0}`, `{"person_id":0,"source_id":1}`} {
		raw := `{"name":"msgvault.messages.changed","arguments":` + arguments + `,"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`
		_, err := DecodeSubscribe([]byte(raw))
		var eventErr *Error
		require.ErrorAs(err, &eventErr)
		assert.Equal(InvalidParams, eventErr.Code)
		assert.Equal("invalid_arguments", eventErr.Reason)
		_, err = DecodeUnsubscribe([]byte(raw))
		require.ErrorAs(err, &eventErr)
		assert.Equal(InvalidParams, eventErr.Code)
		assert.Equal("invalid_arguments", eventErr.Reason)
	}
}

func TestTTLClampAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{{`{}`, 24 * time.Hour}, {`{"ttlMs":null}`, 30 * 24 * time.Hour}, {`{"ttlMs":1}`, time.Minute}, {`{"ttlMs":999999999999}`, 30 * 24 * time.Hour}} {
		var request MCPEventsSubscribeRequest
		require.NoError(t, json.Unmarshal([]byte(tc.raw), &request))
		assert.Equal(t, tc.want, request.TTL.Duration())
	}
}

func TestSubscribeRefreshPreservesPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true}
	input := MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	input = preparedSubscribe(t, input)
	first, err := service.Subscribe(t.Context(), input)
	require.NoError(err)
	key := store.MCPEventsPositionKey(first.ID)
	pos, raw, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	require.NotNil(raw)
	pos.Failures = 2
	stamp := time.Now().UTC().Truncate(time.Second)
	pos.LastError = "http_5xx"
	pos.FailedSince = &stamp
	ok, err := st.AdvanceWakeupStateKey(t.Context(), key, raw, pos)
	require.NoError(err)
	require.True(ok)
	second, err := service.Subscribe(t.Context(), input)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	got, _, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	assert.Equal(pos, got)
	assert.Equal("http_5xx", second.DeliveryStatus.LastError)
	require.NotNil(second.DeliveryStatus.FailedSince)
	assert.Equal(stamp, *second.DeliveryStatus.FailedSince)
}

func TestExpiredSubscriptionRemovedWithoutDelivery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	in = preparedSubscribe(t, in)
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(service.Home, "mcp-events.key")))
	now = now.Add(25 * time.Hour)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	assert.Empty(targets)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	require.NoError(err)
	assert.Nil(position)
}

func TestExpiredGrantEntryPurgedByOwnerService(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	entries, raw, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	entries[0].Owner = "grant:expired"
	entries[0].Expires = now.Add(-time.Second)
	next, err := json.Marshal(entries)
	require.NoError(err)
	ok, err := st.SwapMCPEventsCatalog(t.Context(), raw, next, "", nil, "")
	require.NoError(err)
	require.True(ok)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	assert.Empty(targets)
	entries, _, err = service.catalog(t.Context())
	require.NoError(err)
	assert.Empty(entries)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	require.NoError(err)
	assert.Nil(position)
}

func TestConcurrentFinalSubscriptionUsesSharedLimit(t *testing.T) {
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, ConfigTargets: 31}
	makeRequest := func(url string) MCPEventsSubscribeRequest {
		return preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: url, Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
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

type countingWakeupStore struct {
	*store.Store

	loaded []string
}

func (s *countingWakeupStore) LoadWakeupStateKey(ctx context.Context, key string) (store.WakeupState, []byte, error) {
	s.loaded = append(s.loaded, key)
	return s.Store.LoadWakeupStateKey(ctx, key)
}

func TestNativeRespectsConfiguredTargetLimitAfterRestart(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var logs bytes.Buffer
	service := Service{Store: st, Home: t.TempDir(), Enabled: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	var ids []string
	for i := range 32 {
		in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: fmt.Sprintf("https://example.com/%d", i), Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
		result, err := service.Subscribe(t.Context(), in)
		require.NoError(err)
		ids = append(ids, result.ID)
	}
	service.ConfigTargets = 30
	targets, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 2)
	assert.Equal(ids[0], targets[0].Name)
	assert.Equal(ids[1], targets[1].Name)
	assert.Contains(logs.String(), ids[2])
	assert.Contains(logs.String(), "over_limit")
	counted := &countingWakeupStore{Store: st}
	engine := wakeups.Engine{Store: counted, Native: service.Native}
	require.NoError(engine.RunPass(t.Context()))
	assert.Equal([]string{store.MCPEventsPositionKey(ids[0]), store.MCPEventsPositionKey(ids[1])}, counted.loaded)
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	assert.Len(entries, 32)
}

func TestUnsubscribeWhileDisabledRemovesPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	service := Service{Store: st, Home: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	service.Enabled = false
	require.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}))
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	assert.Empty(entries)
	_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	require.NoError(err)
	assert.Nil(raw)
}
