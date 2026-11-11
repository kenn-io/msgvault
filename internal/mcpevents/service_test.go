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
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/wakeups"
)

func newEventsTestStore(t *testing.T) *store.Store {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "events@example.test")
	require.NoError(t, err)
	require.Equal(t, int64(1), source.ID)
	return st
}

func TestSubscribeRequiresExistingSource(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	_, err := service.Subscribe(t.Context(), in)
	var eventErr *Error
	requirements.ErrorAs(err, &eventErr)
	assertions.Equal(InvalidParams, eventErr.Code)
	assertions.Equal("invalid_scope", eventErr.Reason)
	catalog, err := st.LoadMCPEventsCatalog(t.Context())
	requirements.NoError(err)
	assertions.Nil(catalog)
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&count))
	assertions.Zero(count)
	source, err := st.GetOrCreateSource("imap", "events@example.test")
	requirements.NoError(err)
	requirements.Equal(int64(1), source.ID)
	result, err := service.Subscribe(t.Context(), in)
	requirements.NoError(err)
	assertions.NotEmpty(result.ID)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	requirements.NoError(err)
	assertions.NotNil(position)

	_, err = st.DB().ExecContext(t.Context(), "ALTER TABLE sources RENAME TO unavailable_sources")
	requirements.NoError(err)
	_, err = service.Subscribe(t.Context(), in)
	requirements.ErrorAs(err, &eventErr)
	assertions.Equal(Unavailable, eventErr.Code)
	assertions.Equal("store_unavailable", eventErr.Reason)
}

func TestNativeSkipsMissingCredentialAndRefreshRecovers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook?token=x", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(service.TokensDir, "provider-credentials.json")))
	targets, err := service.Native(t.Context())
	require.NoError(err)
	assert.Empty(targets)
	second, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	targets, err = service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 1)
	assert.Equal(first.ID, targets[0].Name)
}

func preparedSubscribe(t *testing.T, in MCPEventsSubscribeRequest) MCPEventsSubscribeRequest {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	out, err := DecodeSubscribe(raw)
	require.NoError(t, err)
	return out
}

func TestLegacyCatalogRefreshPreservesPosition(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	_, before, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	raw, err := st.LoadMCPEventsCatalog(t.Context())
	require.NoError(err)
	var entries []map[string]any
	require.NoError(json.Unmarshal(raw, &entries))
	entries[0]["sealed_secret"] = "b2xk"
	legacy, err := json.Marshal(entries)
	require.NoError(err)
	ok, err := st.SwapMCPEventsCatalog(t.Context(), raw, legacy, "", nil, "")
	require.NoError(err)
	require.True(ok)
	require.NoError(os.Remove(filepath.Join(service.TokensDir, "provider-credentials.json")))
	second, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	_, after, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	assert.Equal(before, after)
}

func TestDecodeSubscribeRejectsInvalidArgumentsAndNulls(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, tc := range []struct {
		raw         string
		unsubscribe bool
	}{
		{`{"name":null,"arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, false},
		{`{"name":"msgvault.messages.changed","arguments":{"person_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, false},
		{`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"incoming_only":null},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, false},
		{`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"source_id":2},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, false},
		{`{"name":"msgvault.messages.changed","arguments":{"source_id":1,"unknown":true},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, false},
		{`{"name":"msgvault.messages.changed","arguments":{"person_id":1,"source_id":0},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, true},
		{`{"name":"msgvault.messages.changed","arguments":{"person_id":0,"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, true},
	} {
		_, err := DecodeSubscribe([]byte(tc.raw))
		var eventErr *Error
		requirements.ErrorAs(err, &eventErr)
		assertions.Equal(InvalidParams, eventErr.Code, tc.raw)
		if tc.unsubscribe {
			assertions.Equal("invalid_arguments", eventErr.Reason)
			_, err = DecodeUnsubscribe([]byte(tc.raw))
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_arguments", eventErr.Reason)
		}
	}
}

func TestTTLClampAndDefaults(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, tc := range []struct {
		raw  string
		want time.Duration
		wire string
	}{{`{}`, 24 * time.Hour, ""}, {`{"ttlMs":null}`, 30 * 24 * time.Hour, `"ttlMs":null`}, {`{"ttlMs":1}`, time.Minute, `"ttlMs":1`}, {`{"ttlMs":999999999999}`, 30 * 24 * time.Hour, `"ttlMs":999999999999`}} {
		var request MCPEventsSubscribeRequest
		requirements.NoError(json.Unmarshal([]byte(tc.raw), &request))
		assertions.Equal(tc.want, request.TTL.Duration())
		wire, err := json.Marshal(request)
		requirements.NoError(err)
		if tc.wire == "" {
			assertions.NotContains(string(wire), `"ttlMs"`)
		} else {
			assertions.Contains(string(wire), tc.wire)
		}
	}
}

func TestSubscribeRefreshPreservesPosition(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
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

func TestResubscribeReusesIdentityAndStartsAtCurrentBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	key := store.MCPEventsPositionKey(first.ID)
	_, raw, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	require.NotNil(raw)
	old := store.WakeupState{At: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC), Failures: 2}
	ok, err := st.AdvanceWakeupStateKey(t.Context(), key, raw, old)
	require.NoError(err)
	require.True(ok)
	require.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}))
	before, err := st.ListChangedMessages(t.Context(), store.ChangedMessagesCursor{}, 1)
	require.NoError(err)
	require.False(before.CompleteThrough.IsZero())
	second, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	after, err := st.ListChangedMessages(t.Context(), store.ChangedMessagesCursor{}, 1)
	require.NoError(err)
	state, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(second.ID))
	require.NoError(err)
	require.NotNil(raw)
	assert.Equal(first.ID, second.ID)
	assert.False(state.At.Before(before.CompleteThrough))
	assert.False(state.At.After(after.CompleteThrough))
	assert.Equal(store.WakeupState{At: state.At}, state)
}

func TestExpiredSubscriptionRemovedWithoutDelivery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newEventsTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}
	in = preparedSubscribe(t, in)
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.Remove(filepath.Join(service.TokensDir, "provider-credentials.json")))
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
	st := newEventsTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
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
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, ConfigTargets: 31}
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
		require.ErrorAs(err, &eventErr)
		if eventErr.Code == ResourceExhausted {
			exhausted++
		}
	}
	assert.Equal(1, success)
	assert.Equal(1, exhausted)

	// A restarted daemon with more configured targets must not claim delivery.
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	key := store.MCPEventsPositionKey(entries[0].ID)
	before, _, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	service.ConfigTargets = 32
	_, err = service.Subscribe(t.Context(), makeRequest(entries[0].URL))
	var eventErr *Error
	require.ErrorAs(err, &eventErr)
	assert.Equal(ResourceExhausted, eventErr.Code)
	after, _, err := st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	assert.Equal(before, after)
	entriesAfter, _, err := service.catalog(t.Context())
	require.NoError(err)
	assert.Equal(entries, entriesAfter)
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
	st := newEventsTestStore(t)
	var logs bytes.Buffer
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
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
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
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

func TestCredentialWriteFailureLeavesRecoverableCatalog(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	dir := t.TempDir()
	tokens := filepath.Join(dir, "tokens")
	require.NoError(os.MkdirAll(filepath.Join(tokens, ".provider-credentials.lock"), 0700))
	service := Service{Store: st, TokensDir: tokens, Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	_, err := service.Subscribe(t.Context(), in)
	require.Error(err)
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	require.NoError(os.Remove(filepath.Join(tokens, ".provider-credentials.lock")))
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Equal(entries[0].ID, result.ID)
}

func TestOwnerServiceRejectsOtherCatalogOwners(t *testing.T) {
	for _, owner := range []string{"", "grant:fixture", "unknown"} {
		t.Run(owner, func(t *testing.T) {
			require := require.New(t)
			st := newEventsTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err := service.Subscribe(t.Context(), in)
			require.NoError(err)
			entries, raw, err := service.catalog(t.Context())
			require.NoError(err)
			entries[0].Owner = owner
			next, err := json.Marshal(entries)
			require.NoError(err)
			ok, err := st.SwapMCPEventsCatalog(t.Context(), raw, next, "", nil, "")
			require.NoError(err)
			require.True(ok)
			targets, err := service.Native(t.Context())
			require.NoError(err)
			assert.Empty(t, targets)
			assert.Equal(t, -1, find(entries, in.Arguments, in.Delivery.URL))
		})
	}
}

func TestCredentialCleanupFailureRetainsLiveButExpiresOldEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(service.TokensDir, "provider-credentials.json"), []byte("invalid"), 0600))
	err = service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery})
	require.Error(err)
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	assert.Equal(result.ID, entries[0].ID)
	now = now.Add(25 * time.Hour)
	_, err = service.Native(t.Context())
	require.Error(err)
	entries, _, err = service.catalog(t.Context())
	require.NoError(err)
	assert.Empty(entries)
	_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	require.NoError(err)
	assert.Nil(raw)
}

func TestNativeSkipsOriginMismatchWithoutBlockingOtherEntries(t *testing.T) {
	require := require.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/one?token=x", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	in.Delivery.URL = "https://example.com/two"
	second, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	snapshot, err := providercredentials.Read(service.TokensDir)
	require.NoError(err)
	id := providercredentials.EventsID(first.ID)
	revision, err := snapshot.Revision(id)
	require.NoError(err)
	_, err = providercredentials.PutIfRevision(service.TokensDir, revision, id, "https://other.example.com", "YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh")
	require.NoError(err)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 1)
	assert.Equal(t, second.ID, targets[0].Name)
}

func TestCredentialReadFailureDoesNotChangeCatalogOrPositions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/one", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	_, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	beforeCatalog, err := st.LoadMCPEventsCatalog(t.Context())
	require.NoError(err)
	var beforeCount int
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&beforeCount))
	require.NoError(os.WriteFile(filepath.Join(service.TokensDir, "provider-credentials.json"), []byte("invalid credential store"), 0600))
	now = now.Add(time.Hour)
	for _, endpoint := range []string{"https://example.com/one", "https://example.com/two"} {
		in.Delivery.URL = endpoint
		_, err = service.Subscribe(t.Context(), in)
		var eventErr *Error
		require.ErrorAs(err, &eventErr)
		assert.Equal("credential_unavailable", eventErr.Reason)
		afterCatalog, err := st.LoadMCPEventsCatalog(t.Context())
		require.NoError(err)
		assert.Equal(beforeCatalog, afterCatalog)
		var afterCount int
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&afterCount))
		assert.Equal(beforeCount, afterCount)
	}
}
