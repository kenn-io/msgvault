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
			_, err := service.Subscribe(t.Context(), in, acceptCallback)
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
			result, err := service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			assertions.NotEmpty(result.ID)
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			assertions.NotNil(position)

			_, err = st.DB().ExecContext(t.Context(), "ALTER TABLE sources RENAME TO unavailable_sources")
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, acceptCallback)
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
			_, err := service.Subscribe(t.Context(), in, acceptCallback)
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_scope", eventErr.Reason)
			var emptyID int64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons(vcard_uid,display_name) VALUES('empty-person','Empty') RETURNING id`).Scan(&emptyID))
			in.Arguments.PersonID = emptyID
			_, err = service.Subscribe(t.Context(), in, acceptCallback)
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
			_, err = service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			before, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), "ALTER TABLE persons RENAME TO unavailable_persons")
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, acceptCallback)
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
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/path-token?token=x", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in, acceptCallback)
	require.NoError(err)
	catalogBytes, err := st.LoadMCPEventsCatalog(t.Context())
	require.NoError(err)
	assert.NotContains(string(catalogBytes), "path-token")
	assert.NotContains(string(catalogBytes), "token=x")
	assert.NotContains(string(catalogBytes), in.Delivery.Secret)
	assert.Contains(string(catalogBytes), "https://example.com")
	assert.Empty(first.DeliveryStatus.LastError)
	otherInput := in
	otherInput.Delivery.URL = "https://example.com/path-token?token=y"
	other, err := service.Subscribe(t.Context(), otherInput, acceptCallback)
	require.NoError(err)
	assert.NotEqual(first.ID, other.ID)
	state, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	stamp := time.Now().UTC().Add(-time.Hour)
	state.Failures, state.LastError, state.FailedSince = 1, "network_error", &stamp
	ok, err := st.AdvanceWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID), raw, state)
	require.NoError(err)
	require.True(ok)
	require.NoError(os.Remove(filepath.Join(service.TokensDir, providercredentials.EventsFilename)))
	_, err = service.Subscribe(t.Context(), otherInput, acceptCallback)
	require.NoError(err)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 1)
	assert.Equal(other.ID, targets[0].Name)
	assert.NotContains(logs.String(), "secret_unavailable")
	second, err := service.Subscribe(t.Context(), in, acceptCallback)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	assert.True(second.DeliveryStatus.Active)
	assert.Equal("secret_unavailable", second.DeliveryStatus.LastError)
	assert.Nil(second.DeliveryStatus.FailedSince)
	after, _, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	assert.Equal(state, after)
	third, err := service.Subscribe(t.Context(), in, acceptCallback)
	require.NoError(err)
	assert.Equal("network_error", third.DeliveryStatus.LastError)
	require.NotNil(third.DeliveryStatus.FailedSince)
	assert.Equal(stamp, *third.DeliveryStatus.FailedSince)
	after, _, err = st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	assert.Equal(state, after)
	targets, err = service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 2)
	assert.Equal(first.ID, targets[0].Name)
	assert.Equal(in.Delivery.URL, targets[0].URL)
	assert.Equal(bytes.Repeat([]byte("a"), 24), targets[0].Secret)
	assert.Equal(other.ID, targets[1].Name)
	require.NoError(os.Remove(filepath.Join(service.TokensDir, providercredentials.EventsFilename)))
	require.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}))
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	assert.Equal(other.ID, entries[0].ID)
}

func preparedSubscribe(t *testing.T, in MCPEventsSubscribeRequest) MCPEventsSubscribeRequest {
	t.Helper()
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	out, err := DecodeSubscribe(raw)
	require.NoError(t, err)
	return out
}

func TestDecodeSubscribeAndUnsubscribeArguments(t *testing.T) {
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
		{`{"name":"msgvault.messages.changed","arguments":{"person_id":-1,"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, true},
		{`{"name":"msgvault.messages.changed","arguments":{"person_id":1,"source_id":-1},"delivery":{"mode":"webhook","url":"https://example.com","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}`, true},
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
	_, err := DecodeUnsubscribe([]byte(`{"name":"invalid","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook"}}`))
	var eventErr *Error
	requirements.ErrorAs(err, &eventErr)
	assertions.Equal(NotFound, eventErr.Code)
	assertions.Equal("event", eventErr.Reason)
	raw := []byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"url":"https://example.com/hook"}}`)
	unsubscribe, err := DecodeUnsubscribe(raw)
	requirements.NoError(err)
	assertions.Equal("https://example.com/hook", unsubscribe.Delivery.URL)
	assertions.Equal(int64(1), unsubscribe.Arguments.SourceID)
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

func TestDisabledUnsubscribeThenSubscribeReusesIDAndStartsAtCurrentBound(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	first, err := service.Subscribe(t.Context(), in, acceptCallback)
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
	targets, err := (&Service{}).Native(t.Context())
	require.NoError(err)
	assert.Empty(targets)
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
	second, err := service.Subscribe(t.Context(), in, acceptCallback)
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
			result, err := service.Subscribe(t.Context(), in, acceptCallback)
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
			now = now.Add(23 * time.Hour)
			in.Delivery.URL = "https://example.com/live"
			live, err := service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			now = now.Add(2 * time.Hour)
			targets, err := service.Native(t.Context())
			requirements.NoError(err)
			assertions.Len(targets, 1)
			entries, _, err := service.catalog(t.Context())
			requirements.NoError(err)
			requirements.Len(entries, 1)
			assertions.Equal(live.ID, entries[0].ID)
			credentials, err := providercredentials.ReadEvents(service.TokensDir)
			requirements.NoError(err)
			assertions.False(credentials.Stored(providercredentials.EventsID(result.ID)))
			assertions.True(credentials.Stored(providercredentials.EventsID(live.ID)))
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			assertions.Nil(position)
			_, livePosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(live.ID))
			requirements.NoError(err)
			assertions.NotNil(livePosition)
		})
	}
	t.Run("missing credential", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		st := newEventsTestStore(t)
		now := time.Now().UTC()
		service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
		in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
		result, err := service.Subscribe(t.Context(), in, acceptCallback)
		requirements.NoError(err)
		requirements.NoError(os.Remove(filepath.Join(service.TokensDir, providercredentials.EventsFilename)))
		now = now.Add(25 * time.Hour)
		targets, err := service.Native(t.Context())
		requirements.NoError(err)
		assertions.Empty(targets)
		entries, _, err := service.catalog(t.Context())
		requirements.NoError(err)
		assertions.Empty(entries)
		_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
		requirements.NoError(err)
		assertions.Nil(raw)
	})
}

func TestConcurrentFinalSubscriptionUsesCAS(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, ConfigTargets: 31}
	makeRequest := func(url string) MCPEventsSubscribeRequest {
		return preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: url, Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	}
	type result struct {
		request MCPEventsSubscribeRequest
		err     error
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	results := make(chan result, 2)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	verify := func(ctx context.Context, _ string, _ string, _ []byte) error {
		arrived <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for _, url := range []string{"https://example.com/one", "https://example.com/two"} {
		request := makeRequest(url)
		go func() {
			_, err := service.Subscribe(ctx, request, verify)
			results <- result{request, err}
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-ctx.Done():
			require.NoError(ctx.Err())
		}
	}
	close(release)
	var success, refused int
	var winningRequest MCPEventsSubscribeRequest
	for range 2 {
		got := <-results
		if got.err == nil {
			success++
			winningRequest = got.request
			continue
		}
		var eventErr *Error
		require.ErrorAs(got.err, &eventErr)
		assert.Equal(Unavailable, eventErr.Code)
		assert.Equal("store_unavailable", eventErr.Reason)
		refused++
	}
	require.Equal(1, success)
	assert.Equal(1, refused)

	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	assert.Equal(entries[0].ID, subscriptionID("owner", winningRequest.Arguments, winningRequest.Delivery.URL))
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
		result, err := service.Subscribe(t.Context(), in, acceptCallback)
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

	// Both refresh and a new subscription are refused after configured targets grow.
	service.ConfigTargets = 1
	beforeCatalog, err := st.LoadMCPEventsCatalog(t.Context())
	require.NoError(err)
	credentialPath := filepath.Join(service.TokensDir, providercredentials.EventsFilename)
	beforeCredential, err := os.ReadFile(credentialPath)
	require.NoError(err)
	_, beforePosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(ids[0]))
	require.NoError(err)
	for _, endpoint := range []string{"https://example.com/0", "https://example.com/new"} {
		in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: endpoint, Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
		_, err := service.Subscribe(t.Context(), in, acceptCallback)
		var eventErr *Error
		require.ErrorAs(err, &eventErr)
		assert.Equal(ResourceExhausted, eventErr.Code)
		assert.Equal("subscriptions", eventErr.Reason)
	}
	afterCatalog, err := st.LoadMCPEventsCatalog(t.Context())
	require.NoError(err)
	assert.Equal(beforeCatalog, afterCatalog)
	afterCredential, err := os.ReadFile(credentialPath)
	require.NoError(err)
	assert.Equal(beforeCredential, afterCredential)
	_, afterPosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(ids[0]))
	require.NoError(err)
	assert.Equal(beforePosition, afterPosition)
}

func TestSubscribeFailuresPreserveStateAndVerify(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"CredentialWriteFailurePreservesSubscriptionState", func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newEventsTestStore(t)
			tokens := filepath.Join(t.TempDir(), "tokens")
			lock := filepath.Join(tokens, ".provider-credentials.lock")
			require.NoError(os.MkdirAll(lock, 0700))
			now := time.Now().UTC()
			service := Service{Store: st, TokensDir: tokens, Enabled: true, Now: func() time.Time { return now }}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err := service.Subscribe(t.Context(), in, acceptCallback)
			var eventErr *Error
			require.ErrorAs(err, &eventErr)
			assert.Equal("credential_unavailable", eventErr.Reason)
			entries, _, err := service.catalog(t.Context())
			require.NoError(err)
			assert.Empty(entries)
			var count int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT count(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&count))
			assert.Zero(count)
			require.NoError(os.Remove(lock))
			result, err := service.Subscribe(t.Context(), in, acceptCallback)
			require.NoError(err)
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			require.NoError(err)
			require.NotNil(position)
			credentials, err := os.ReadFile(filepath.Join(tokens, providercredentials.EventsFilename))
			require.NoError(err)
			require.NoError(os.Remove(lock))
			require.NoError(os.Mkdir(lock, 0700))
			now = now.Add(time.Hour)
			_, err = service.Subscribe(t.Context(), in, acceptCallback)
			require.ErrorAs(err, &eventErr)
			assert.Equal("credential_unavailable", eventErr.Reason)
			entries, _, err = service.catalog(t.Context())
			require.NoError(err)
			require.Len(entries, 1)
			assert.Equal(result.ID, entries[0].ID)
			_, afterPosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			require.NoError(err)
			assert.Equal(position, afterPosition)
			afterCredentials, err := os.ReadFile(filepath.Join(tokens, providercredentials.EventsFilename))
			require.NoError(err)
			assert.Equal(credentials, afterCredentials)

		}},
		{"IncompatibleCatalogRefusedWithoutRewriting", func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/old?token=fixture", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			first, err := service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
			requirements.NoError(err)
			original, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
			draft := fmt.Sprintf(`[{"id":%q,"owner":"owner","arguments":{"source_id":1,"incoming_only":%t},"url":%q,"expires_at":%q}]`, first.ID, *in.Arguments.IncomingOnly, in.Delivery.URL, expires)
			ok, err := st.SwapMCPEventsCatalog(t.Context(), original, []byte(draft), "", nil, "")
			requirements.NoError(err)
			requirements.True(ok)
			path := filepath.Join(service.TokensDir, providercredentials.EventsFilename)
			credential, err := os.ReadFile(path)
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, acceptCallback)
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(Unavailable, eventErr.Code)
			assertions.Equal("store_unavailable", eventErr.Reason)
			after, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			assertions.Equal([]byte(draft), after)
			_, afterPosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
			requirements.NoError(err)
			assertions.Equal(position, afterPosition)
			afterCredential, err := os.ReadFile(path)
			requirements.NoError(err)
			assertions.Equal(credential, afterCredential)

		}},
		{"RefreshRejectsMissingOrCorruptPositionBeforePublication", func(t *testing.T) {
			for _, corrupted := range []bool{false, true} {
				t.Run(fmt.Sprintf("corrupt=%t", corrupted), func(t *testing.T) {
					requirements := require.New(t)
					assertions := assert.New(t)
					st := newEventsTestStore(t)
					now := time.Now().UTC()
					service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
					in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
					first, err := service.Subscribe(t.Context(), in, acceptCallback)
					requirements.NoError(err)
					key := store.MCPEventsPositionKey(first.ID)
					if corrupted {
						_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE archive_metadata SET value=? WHERE key=?`), `{`, key)
					} else {
						_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM archive_metadata WHERE key=?`), key)
					}
					requirements.NoError(err)
					before, err := st.LoadMCPEventsCatalog(t.Context())
					requirements.NoError(err)
					credentialPath := filepath.Join(service.TokensDir, providercredentials.EventsFilename)
					credential, err := os.ReadFile(credentialPath)
					requirements.NoError(err)
					now = now.Add(time.Hour)
					_, err = service.Subscribe(t.Context(), in, acceptCallback)
					var eventErr *Error
					requirements.ErrorAs(err, &eventErr)
					assertions.Equal("store_unavailable", eventErr.Reason)
					after, err := st.LoadMCPEventsCatalog(t.Context())
					requirements.NoError(err)
					assertions.Equal(before, after)
					afterCredential, err := os.ReadFile(credentialPath)
					requirements.NoError(err)
					assertions.Equal(credential, afterCredential)
					if corrupted {
						var raw string
						requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT value FROM archive_metadata WHERE key=?`), key).Scan(&raw))
						assertions.Equal("{", raw)
					} else {
						_, raw, loadErr := st.LoadWakeupStateKey(t.Context(), key)
						requirements.NoError(loadErr)
						assertions.Nil(raw)
					}
				})
			}

		}},
		{"CredentialReadFailureDoesNotChangeCatalogOrPositions", func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/one", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			result, err := service.Subscribe(t.Context(), in, acceptCallback)
			require.NoError(err)
			beforeCatalog, err := st.LoadMCPEventsCatalog(t.Context())
			require.NoError(err)
			var beforeCount int
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&beforeCount))
			key := store.MCPEventsPositionKey(result.ID)
			_, beforePosition, err := st.LoadWakeupStateKey(t.Context(), key)
			require.NoError(err)
			credentialPath := filepath.Join(service.TokensDir, providercredentials.EventsFilename)
			invalidCredential := []byte("invalid credential store")
			require.NoError(os.WriteFile(credentialPath, invalidCredential, 0600))
			now = now.Add(time.Hour)
			for _, endpoint := range []string{"https://example.com/one", "https://example.com/two"} {
				in.Delivery.URL = endpoint
				_, err = service.Subscribe(t.Context(), in, acceptCallback)
				var eventErr *Error
				require.ErrorAs(err, &eventErr)
				assert.Equal("credential_unavailable", eventErr.Reason)
				afterCatalog, err := st.LoadMCPEventsCatalog(t.Context())
				require.NoError(err)
				assert.Equal(beforeCatalog, afterCatalog)
				var afterCount int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%").Scan(&afterCount))
				assert.Equal(beforeCount, afterCount)
				_, afterPosition, err := st.LoadWakeupStateKey(t.Context(), key)
				require.NoError(err)
				assert.Equal(beforePosition, afterPosition)
				afterCredential, err := os.ReadFile(credentialPath)
				require.NoError(err)
				assert.Equal(invalidCredential, afterCredential)
			}

		}},
		{"SubscribeVerifiesBeforeWritingAndOnRefresh", func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			tokens := t.TempDir()
			service := &Service{Store: st, TokensDir: tokens, Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			calls := 0
			verify := func(_ context.Context, endpoint, id string, secret []byte) error {
				calls++
				assertions.Equal(in.Delivery.URL, endpoint)
				assertions.Equal(subscriptionID("owner", in.Arguments, endpoint), id)
				assertions.Equal(in.secret, secret)
				if calls == 1 {
					return &Error{Code: CallbackEndpoint, Reason: "dns_error"}
				}
				return nil
			}
			_, err := service.Subscribe(t.Context(), in, verify)
			var callbackErr *Error
			requirements.ErrorAs(err, &callbackErr)
			assertions.Equal(CallbackEndpoint, callbackErr.Code)
			assertions.Equal("dns_error", callbackErr.Reason)
			catalog, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			assertions.Empty(catalog)
			_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(subscriptionID("owner", in.Arguments, in.Delivery.URL)))
			requirements.NoError(err)
			assertions.Nil(raw)
			_, err = os.Stat(filepath.Join(tokens, providercredentials.EventsFilename))
			assertions.ErrorIs(err, os.ErrNotExist)
			_, err = service.Subscribe(t.Context(), in, verify)
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, verify)
			requirements.NoError(err)
			assertions.Equal(3, calls)

		}},
		{"SubscribeRejectsLocalFailuresBeforeVerification", func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			service := &Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			calls := 0
			verify := func(context.Context, string, string, []byte) error { calls++; return nil }
			bad := in
			bad.Arguments.SourceID = 999999
			_, err := service.Subscribe(t.Context(), bad, verify)
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_scope", eventErr.Reason)
			assertions.Zero(calls)
			service.ConfigTargets = 32
			_, err = service.Subscribe(t.Context(), in, verify)
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(ResourceExhausted, eventErr.Code)
			assertions.Equal("subscriptions", eventErr.Reason)
			assertions.Zero(calls)
			service.ConfigTargets = 0
			result, err := service.Subscribe(t.Context(), in, verify)
			requirements.NoError(err)
			assertions.Equal(1, calls)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM archive_metadata WHERE key = ?`), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, verify)
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(Unavailable, eventErr.Code)
			assertions.Equal("store_unavailable", eventErr.Reason)
			assertions.Equal(1, calls)

		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestOwnerServiceRejectsOtherCatalogOwners(t *testing.T) {
	for _, owner := range []string{"", "grant:fixture", "unknown"} {
		t.Run(owner, func(t *testing.T) {
			require := require.New(t)
			st := newEventsTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err := service.Subscribe(t.Context(), in, acceptCallback)
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

func TestCredentialCleanupFailuresRemoveEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"UnsubscribeCredentialCleanupFailureRemovesEntry", func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			var logs bytes.Buffer
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			result, err := service.Subscribe(t.Context(), in, acceptCallback)
			require.NoError(err)
			require.NoError(os.WriteFile(filepath.Join(service.TokensDir, providercredentials.EventsFilename), []byte("invalid"), 0600))
			err = service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery})
			require.NoError(err)
			entries, _, err := service.catalog(t.Context())
			require.NoError(err)
			assert.Empty(entries)
			_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			require.NoError(err)
			assert.Nil(raw)
			assert.Contains(logs.String(), "credential_unavailable")
			assert.NotContains(logs.String(), service.TokensDir)
			assert.NotContains(logs.String(), in.Delivery.URL)

		}},
		{"NativeContinuesAfterExpiredCredentialCleanupFails", func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			var logs bytes.Buffer
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/expired?sensitive-marker", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			expired, err := service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			now = now.Add(23 * time.Hour)
			in.Delivery.URL = "https://example.com/live"
			live, err := service.Subscribe(t.Context(), in, acceptCallback)
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

		}},
	} {
		t.Run(tc.name, tc.run)
	}
}

func TestNativeRejectsInvalidPrivateCallbackPairs(t *testing.T) {
	for _, tc := range []struct {
		name, value, recordOrigin string
	}{
		{"unknown member", `{"url":"https://example.com/one","secret":"YQ==","future":true}`, "https://example.com"},
		{"different URL", `{"url":"https://example.com/wrong","secret":"YQ=="}`, "https://example.com"},
		{"private origin mismatch", `{"url":"https://example.com/one","secret":"YQ=="}`, "https://other.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/one", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			first, err := service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			in.Delivery.URL = "https://example.com/two"
			second, err := service.Subscribe(t.Context(), in, acceptCallback)
			requirements.NoError(err)
			snapshot, err := providercredentials.ReadEvents(service.TokensDir)
			requirements.NoError(err)
			revision, err := snapshot.Revision(providercredentials.EventsID(first.ID))
			requirements.NoError(err)
			_, err = providercredentials.PutIfRevision(service.TokensDir, revision, providercredentials.EventsID(first.ID), tc.recordOrigin, tc.value)
			requirements.NoError(err)
			if tc.recordOrigin != "https://example.com" {
				raw, loadErr := st.LoadMCPEventsCatalog(t.Context())
				requirements.NoError(loadErr)
				var entries []map[string]any
				requirements.NoError(json.Unmarshal(raw, &entries))
				entries[0]["origin"] = tc.recordOrigin
				modified, marshalErr := json.Marshal(entries)
				requirements.NoError(marshalErr)
				ok, swapErr := st.SwapMCPEventsCatalog(t.Context(), raw, modified, "", nil, "")
				requirements.NoError(swapErr)
				requirements.True(ok)
			}
			targets, err := service.Native(t.Context())
			requirements.NoError(err)
			requirements.Len(targets, 1)
			assertions.Equal(second.ID, targets[0].Name)
		})
	}
}

func acceptCallback(context.Context, string, string, []byte) error { return nil }

func TestRemovalKeepsCredentialsWhenCatalogCASFails(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	result, err := service.Subscribe(t.Context(), in, acceptCallback)
	requirements.NoError(err)
	entries, original, err := service.catalog(t.Context())
	requirements.NoError(err)
	credentialPath := filepath.Join(service.TokensDir, providercredentials.EventsFilename)
	credential, err := os.ReadFile(credentialPath)
	requirements.NoError(err)
	changed := append(bytes.Clone(original), ' ')
	ok, err := st.SwapMCPEventsCatalog(t.Context(), original, changed, "", nil, "")
	requirements.NoError(err)
	requirements.True(ok)
	ok, err = service.removeEntryAndCredential(t.Context(), entries, original, 0)
	requirements.NoError(err)
	assertions.False(ok)
	after, err := os.ReadFile(credentialPath)
	requirements.NoError(err)
	assertions.Equal(credential, after)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	requirements.NoError(err)
	assertions.NotNil(position)
	requirements.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}))
	_, position, err = st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	requirements.NoError(err)
	assertions.Nil(position)
	credentials, err := providercredentials.ReadEvents(service.TokensDir)
	requirements.NoError(err)
	assertions.False(credentials.Stored(providercredentials.EventsID(result.ID)))
}
