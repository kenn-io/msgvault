package mcpevents

import (
	"bytes"
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
)

func newEventsTestStore(t *testing.T) *store.Store {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "events@example.test")
	require.NoError(t, err)
	require.Equal(t, int64(1), source.ID)
	return st
}

func TestSubscribeClassifiesScopeFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"source", func(t *testing.T) {
			t.Helper()
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
		}},
		{"person", func(t *testing.T) {
			t.Helper()
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{PersonID: 999999}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err := service.Subscribe(t.Context(), in)
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_scope", eventErr.Reason)
			var emptyID int64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons(vcard_uid,display_name) VALUES('empty-person','Empty') RETURNING id`).Scan(&emptyID))
			in.Arguments.PersonID = emptyID
			_, err = service.Subscribe(t.Context(), in)
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_scope", eventErr.Reason)
			catalog, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			assertions.Nil(catalog)
			var count int
			requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&count))
			assertions.Zero(count)
			participant, err := st.EnsureParticipant("person@example.test", "Person", "example.test")
			requirements.NoError(err)
			person, _, err := st.CreatePersonFromParticipant(participant)
			requirements.NoError(err)
			in.Arguments.PersonID = person.ID
			_, err = service.Subscribe(t.Context(), in)
			requirements.NoError(err)
			before, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), "ALTER TABLE persons RENAME TO unavailable_persons")
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in)
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(Unavailable, eventErr.Code)
			assertions.Equal("store_unavailable", eventErr.Reason)
			after, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			assertions.Equal(before, after)
		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestNativeSkipsMissingCredentialAndRefreshRecovers(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newEventsTestStore(t)
	var logs bytes.Buffer
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook?token=x", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Empty(first.DeliveryStatus.LastError)
	otherInput := in
	otherInput.Delivery.URL = "https://example.com/other"
	other, err := service.Subscribe(t.Context(), otherInput)
	require.NoError(err)
	state, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	stamp := time.Now().UTC().Add(-time.Hour)
	state.Failures, state.LastError, state.FailedSince = 1, "network_error", &stamp
	ok, err := st.AdvanceWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID), raw, state)
	require.NoError(err)
	require.True(ok)
	require.NoError(os.Remove(filepath.Join(service.TokensDir, providercredentials.EventsFilename)))
	_, err = service.Subscribe(t.Context(), otherInput)
	require.NoError(err)
	for range 3 {
		targets, err := service.Native(t.Context())
		require.NoError(err)
		require.Len(targets, 1)
		assert.Equal(other.ID, targets[0].Name)
	}
	assert.NotContains(logs.String(), "secret_unavailable")
	second, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	assert.True(second.DeliveryStatus.Active)
	assert.Equal("secret_unavailable", second.DeliveryStatus.LastError)
	assert.Nil(second.DeliveryStatus.FailedSince)
	after, _, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	assert.Equal(state, after)
	third, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	assert.Equal("network_error", third.DeliveryStatus.LastError)
	require.NotNil(third.DeliveryStatus.FailedSince)
	assert.Equal(stamp, *third.DeliveryStatus.FailedSince)
	after, _, err = st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	assert.Equal(state, after)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 2)
	assert.Equal(first.ID, targets[0].Name)
	assert.Equal(other.ID, targets[1].Name)
}

func preparedSubscribe(t *testing.T, in MCPEventsSubscribeRequest) MCPEventsSubscribeRequest {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	out, err := DecodeSubscribe(raw)
	require.NoError(t, err)
	return out
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
	service.Enabled = false
	require.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}))
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	assert.Empty(entries)
	_, raw, err = st.LoadWakeupStateKey(t.Context(), key)
	require.NoError(err)
	assert.Nil(raw)
	service.Enabled = true
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
	for _, owner := range []string{"owner", "grant:expired"} {
		t.Run(owner, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			result, err := service.Subscribe(t.Context(), in)
			requirements.NoError(err)
			if owner != "owner" {
				entries, raw, err := service.catalog(t.Context())
				requirements.NoError(err)
				requirements.Len(entries, 1)
				entries[0].Owner = owner
				next, err := json.Marshal(entries)
				requirements.NoError(err)
				ok, err := st.SwapMCPEventsCatalog(t.Context(), raw, next, "", nil, "")
				requirements.NoError(err)
				requirements.True(ok)
			}
			now = now.Add(25 * time.Hour)
			targets, err := service.Native(t.Context())
			requirements.NoError(err)
			assertions.Empty(targets)
			entries, _, err := service.catalog(t.Context())
			requirements.NoError(err)
			assertions.Empty(entries)
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			assertions.Nil(position)
		})
	}
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
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	assert.Len(entries, 32)
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

func TestUnsubscribeCredentialCleanupFailureRetainsEntry(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	now := time.Now().UTC()
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	result, err := service.Subscribe(t.Context(), in)
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(service.TokensDir, providercredentials.EventsFilename), []byte("invalid"), 0600))
	err = service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery})
	require.Error(err)
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	assert.Equal(result.ID, entries[0].ID)
}

func TestNativeContinuesAfterExpiredCredentialCleanupFails(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newEventsTestStore(t)
	now := time.Now().UTC()
	var logs bytes.Buffer
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	previous := slog.Default()
	slog.SetDefault(service.Logger)
	t.Cleanup(func() { slog.SetDefault(previous) })
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/expired?sensitive-marker", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	expired, err := service.Subscribe(t.Context(), in)
	requirements.NoError(err)
	now = now.Add(23 * time.Hour)
	in.Delivery.URL = "https://example.com/live"
	live, err := service.Subscribe(t.Context(), in)
	requirements.NoError(err)
	now = now.Add(2 * time.Hour)
	lock := filepath.Join(service.TokensDir, ".provider-credentials.lock")
	requirements.NoError(os.Remove(lock))
	requirements.NoError(os.Mkdir(lock, 0700))
	targets, err := service.Native(t.Context())
	requirements.NoError(err)
	requirements.Len(targets, 1)
	assertions.Equal(live.ID, targets[0].Name)
	entries, _, err := service.catalog(t.Context())
	requirements.NoError(err)
	requirements.Len(entries, 1)
	assertions.Equal(live.ID, entries[0].ID)
	_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(expired.ID))
	requirements.NoError(err)
	assertions.Nil(raw)
	assertions.Contains(logs.String(), "credential_unavailable")
	assertions.NotContains(logs.String(), "sensitive-marker")
	assertions.NotContains(logs.String(), in.Delivery.Secret)
	assertions.NotContains(logs.String(), service.TokensDir)
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
	require.NoError(os.WriteFile(filepath.Join(service.TokensDir, providercredentials.EventsFilename), []byte("invalid credential store"), 0600))
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
