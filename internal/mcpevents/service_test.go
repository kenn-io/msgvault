package mcpevents

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/fileutil"
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
			_, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			requirements.NoError(err)
			assertions.NotEmpty(result.ID)
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			assertions.NotNil(position)

			_, err = st.DB().ExecContext(t.Context(), "ALTER TABLE sources RENAME TO unavailable_sources")
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			_, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_scope", eventErr.Reason)
			var emptyID int64
			requirements.NoError(st.DB().QueryRowContext(t.Context(), `INSERT INTO persons(vcard_uid,display_name) VALUES('empty-person','Empty') RETURNING id`).Scan(&emptyID))
			in.Arguments.PersonID = emptyID
			_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			requirements.NoError(err)
			before, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			_, err = st.DB().ExecContext(t.Context(), "ALTER TABLE persons RENAME TO unavailable_persons")
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	first, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	other, err := service.Subscribe(t.Context(), otherInput, "owner", acceptCallback, nil)
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
	_, err = service.Subscribe(t.Context(), otherInput, "owner", acceptCallback, nil)
	require.NoError(err)
	targets, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(targets, 1)
	assert.Equal(other.ID, targets[0].Name)
	assert.NotContains(logs.String(), "secret_unavailable")
	second, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	assert.True(second.DeliveryStatus.Active)
	assert.Equal("secret_unavailable", second.DeliveryStatus.LastError)
	assert.Nil(second.DeliveryStatus.FailedSince)
	after, _, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
	require.NoError(err)
	assert.Equal(state, after)
	third, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	require.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}, "owner"))
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
	first, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	require.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}, "owner"))
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
	second, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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

func TestSubscriptionChallengeKeepsOwnerHeadAndRefreshesGrantHead(t *testing.T) {
	for _, delegated := range []bool{false, true} {
		t.Run(fmt.Sprintf("delegated=%t", delegated), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("test", "events@example.test")
			requirements.NoError(err)
			registry := agentgrant.NewRegistry()
			t.Cleanup(registry.Close)
			owner := "owner"
			if delegated {
				id, _, _, issueErr := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
				requirements.NoError(issueErr)
				owner = agentgrant.Owner(id)
			}
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: registry}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: source.ID}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			var beforeWrite, afterWrite time.Time
			var messageID int64
			result, err := service.Subscribe(t.Context(), in, owner, func(ctx context.Context, _ string, _ string, _ []byte) error {
				before, readErr := st.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
				requirements.NoError(readErr)
				beforeWrite = before.CompleteThrough
				_, messageID, readErr = testutil.CreateIndexedSourceMessage(ctx, st, "events@example.test", "challenge", "challenge", "")
				requirements.NoError(readErr)
				after, readErr := st.ListChangedMessages(ctx, store.ChangedMessagesCursor{}, 1)
				requirements.NoError(readErr)
				afterWrite = after.CompleteThrough
				return nil
			}, nil)
			requirements.NoError(err)
			state, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			requirements.NotNil(raw)
			if delegated {
				assertions.False(state.At.Before(afterWrite))
			} else {
				assertions.False(state.At.After(beforeWrite))
				page, readErr := st.ListChangedMessages(t.Context(), store.ChangedMessagesFrom(state.At), 100)
				requirements.NoError(readErr)
				var ids []int64
				for _, message := range page.Messages {
					ids = append(ids, message.ID)
				}
				assertions.Contains(ids, messageID)
			}
		})
	}
}

func TestExpiredSubscriptionRemovedWithoutDelivery(t *testing.T) {
	for _, owner := range []string{"owner"} {
		t.Run(owner, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			requirements.NoError(err)

			now = now.Add(23 * time.Hour)
			in.Delivery.URL = "https://example.com/live"
			live, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
		result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			_, err := service.Subscribe(ctx, request, "owner", verify, nil)
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
		assert.Equal(ResourceExhausted, eventErr.Code)
		assert.Equal("subscriptions", eventErr.Reason)
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
		result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
		_, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	type stateSnapshot struct {
		catalog          []byte
		positions        map[string]string
		credential       []byte
		credentialExists bool
	}
	snapshot := func(t *testing.T, st *store.Store, tokens string) stateSnapshot {
		t.Helper()
		var state stateSnapshot
		var err error
		state.catalog, err = st.LoadMCPEventsCatalog(t.Context())
		require.NoError(t, err)
		state.positions = make(map[string]string)
		rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`SELECT key, value FROM archive_metadata WHERE key LIKE ?`), "mcpevents:v1:pos:%")
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		for rows.Next() {
			var key, value string
			require.NoError(t, rows.Scan(&key, &value))
			state.positions[key] = value
		}
		require.NoError(t, rows.Err())
		state.credential, err = os.ReadFile(filepath.Join(tokens, providercredentials.EventsFilename))
		if os.IsNotExist(err) {
			return state
		}
		require.NoError(t, err)
		state.credentialExists = true
		return state
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"CredentialWriteFailurePreservesSubscriptionState", func(t *testing.T) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			st := newEventsTestStore(t)
			tokens := filepath.Join(t.TempDir(), "tokens")
			lock := filepath.Join(tokens, ".provider-credentials.lock")
			require.NoError(fileutil.SecureMkdirAll(lock, 0700))
			now := time.Now().UTC()
			service := Service{Store: st, TokensDir: tokens, Enabled: true, Now: func() time.Time { return now }}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			before := snapshot(t, st, tokens)
			_, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			after := snapshot(t, st, tokens)
			// An emptied catalog is stored as [] after rollback.
			if string(after.catalog) == "[]" {
				after.catalog = nil
			}
			assert.Equal(before, after)
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
			result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			require.NoError(err)
			_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			require.NoError(err)
			require.NotNil(position)
			credentials, err := os.ReadFile(filepath.Join(tokens, providercredentials.EventsFilename))
			require.NoError(err)
			require.NoError(os.Remove(lock))
			require.NoError(fileutil.SecureMkdirAll(lock, 0700))
			assert.Equal(now.Add(24*time.Hour), result.RefreshBefore)
			now = now.Add(time.Hour)
			before = snapshot(t, st, tokens)
			_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			assert.Equal(before, snapshot(t, st, tokens))
			require.ErrorAs(err, &eventErr)
			assert.Equal("credential_unavailable", eventErr.Reason)
			refresh, err := DecodeSubscribe([]byte(`{"name":"msgvault.messages.changed","arguments":{"source_id":1},"delivery":{"mode":"webhook","url":"https://example.com/hook","secret":"whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"},"ttlMs":60000}`))
			require.NoError(err)
			before = snapshot(t, st, tokens)
			_, err = service.Subscribe(t.Context(), refresh, "owner", acceptCallback, nil)
			after = snapshot(t, st, tokens)
			assert.Equal(before, after)
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
			now = now.Add(2 * time.Minute)
			targets, err := service.Native(t.Context())
			require.NoError(err)
			if assert.Len(targets, 1) {
				assert.Equal(result.ID, targets[0].Name)
			}
			_, livePosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
			require.NoError(err)
			assert.Equal(position, livePosition)
		}},
		{"IncompatibleCatalogRefusedWithoutRewriting", func(t *testing.T) {
			t.Helper()
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/old?token=fixture", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			first, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			beforeState := snapshot(t, st, service.TokensDir)
			_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			assertions.Equal(beforeState, snapshot(t, st, service.TokensDir))
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
			t.Helper()
			for _, corrupted := range []bool{false, true} {
				t.Run(fmt.Sprintf("corrupt=%t", corrupted), func(t *testing.T) {
					requirements := require.New(t)
					assertions := assert.New(t)
					st := newEventsTestStore(t)
					now := time.Now().UTC()
					service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
					in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
					first, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
					beforeState := snapshot(t, st, service.TokensDir)
					_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
					assertions.Equal(beforeState, snapshot(t, st, service.TokensDir))
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
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/one", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
				beforeState := snapshot(t, st, service.TokensDir)
				_, err = service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
				assert.Equal(beforeState, snapshot(t, st, service.TokensDir))
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
			t.Helper()
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
			beforeState := snapshot(t, st, tokens)
			_, err := service.Subscribe(t.Context(), in, "owner", verify, nil)
			assertions.Equal(beforeState, snapshot(t, st, tokens))
			var callbackErr *Error
			requirements.ErrorAs(err, &callbackErr)
			assertions.Equal(CallbackEndpoint, callbackErr.Code)
			assertions.Equal("dns_error", callbackErr.Reason)
			_, err = service.Subscribe(t.Context(), in, "owner", func(context.Context, string, string, []byte) error { return errors.New("private transport detail") }, nil)
			requirements.ErrorAs(err, &callbackErr)
			assertions.Equal(CallbackEndpoint, callbackErr.Code)
			assertions.Equal("challenge_failed", callbackErr.Reason)
			assertions.Equal(beforeState, snapshot(t, st, tokens))
			catalog, err := st.LoadMCPEventsCatalog(t.Context())
			requirements.NoError(err)
			assertions.Empty(catalog)
			_, raw, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(subscriptionID("owner", in.Arguments, in.Delivery.URL)))
			requirements.NoError(err)
			assertions.Nil(raw)
			_, err = os.Stat(filepath.Join(tokens, providercredentials.EventsFilename))
			requirements.ErrorIs(err, os.ErrNotExist)
			_, err = service.Subscribe(t.Context(), in, "owner", verify, nil)
			requirements.NoError(err)
			_, err = service.Subscribe(t.Context(), in, "owner", verify, nil)
			requirements.NoError(err)
			assertions.Equal(3, calls)
		}},
		{"SubscribeRejectsLocalFailuresBeforeVerification", func(t *testing.T) {
			t.Helper()
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			service := &Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			calls := 0
			verify := func(context.Context, string, string, []byte) error { calls++; return nil }
			bad := in
			bad.Arguments.SourceID = 999999
			beforeState := snapshot(t, st, service.TokensDir)
			_, err := service.Subscribe(t.Context(), bad, "owner", verify, nil)
			assertions.Equal(beforeState, snapshot(t, st, service.TokensDir))
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(InvalidParams, eventErr.Code)
			assertions.Equal("invalid_scope", eventErr.Reason)
			assertions.Zero(calls)
			service.ConfigTargets = 32
			beforeState = snapshot(t, st, service.TokensDir)
			_, err = service.Subscribe(t.Context(), in, "owner", verify, nil)
			assertions.Equal(beforeState, snapshot(t, st, service.TokensDir))
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(ResourceExhausted, eventErr.Code)
			assertions.Equal("subscriptions", eventErr.Reason)
			assertions.Zero(calls)
			service.ConfigTargets = 0
			result, err := service.Subscribe(t.Context(), in, "owner", verify, nil)
			requirements.NoError(err)
			assertions.Equal(1, calls)
			_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM archive_metadata WHERE key = ?`), store.MCPEventsPositionKey(result.ID))
			requirements.NoError(err)
			beforeState = snapshot(t, st, service.TokensDir)
			_, err = service.Subscribe(t.Context(), in, "owner", verify, nil)
			assertions.Equal(beforeState, snapshot(t, st, service.TokensDir))
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
	for _, owner := range []string{"", "unknown"} {
		t.Run(owner, func(t *testing.T) {
			require := require.New(t)
			st := newEventsTestStore(t)
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			assert.Equal(t, -1, find(entries, "owner", in.Arguments, in.Delivery.URL))
		})
	}
}

func TestCredentialCleanupFailuresRemoveEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"UnsubscribeCredentialCleanupFailureRemovesEntry", func(t *testing.T) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			var logs bytes.Buffer
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			require.NoError(err)
			require.NoError(os.WriteFile(filepath.Join(service.TokensDir, providercredentials.EventsFilename), []byte("invalid"), 0600))
			err = service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}, "owner")
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
			t.Helper()
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newEventsTestStore(t)
			now := time.Now().UTC()
			var logs bytes.Buffer
			service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/expired?sensitive-marker", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			expired, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			requirements.NoError(err)
			now = now.Add(23 * time.Hour)
			in.Delivery.URL = "https://example.com/live"
			live, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
			first, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
			requirements.NoError(err)
			in.Delivery.URL = "https://example.com/two"
			second, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	result, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
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
	requirements.NoError(service.Unsubscribe(t.Context(), MCPEventsUnsubscribeRequest{Name: in.Name, Arguments: in.Arguments, Delivery: in.Delivery}, "owner"))
	_, position, err = st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(result.ID))
	requirements.NoError(err)
	assertions.Nil(position)
	credentials, err := providercredentials.ReadEvents(service.TokensDir)
	requirements.NoError(err)
	assertions.False(credentials.Stored(providercredentials.EventsID(result.ID)))
}

func TestNativeRemovesSubscriptionsWhenGrantEnds(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	reg := agentgrant.NewRegistry()
	id, _, _, err := reg.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "events@example.test"}}, time.Time{})
	require.NoError(err)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: reg}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	created, err := service.Subscribe(t.Context(), in, agentgrant.Owner(id), func(context.Context, string, string, []byte) error { return nil }, nil)
	require.NoError(err)
	subs, err := service.Native(t.Context())
	require.NoError(err)
	require.Len(subs, 1)
	assert.Equal(id, subs[0].GrantID)
	assert.Same(reg, subs[0].Grants)
	require.True(reg.Revoke(id))
	subs, err = service.Native(t.Context())
	require.NoError(err)
	assert.Empty(subs)
	_, position, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(created.ID))
	require.NoError(err)
	assert.Nil(position)
}

func TestDelegatedCatalogChangesDuringChallenge(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *store.Store, *agentgrant.Registry, string, *Service, *time.Time)
	}{
		{"grant revoked", func(t *testing.T, st *store.Store, registry *agentgrant.Registry, id string, service *Service, now *time.Time) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			var err error
			verify := func(context.Context, string, string, []byte) error { registry.Revoke(id); return nil }
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err = service.Subscribe(t.Context(), in, agentgrant.Owner(id), verify, nil)
			var eventErr *Error
			require.ErrorAs(err, &eventErr)
			assert.Equal(Forbidden, eventErr.Code)
			assert.Equal("permission_denied", eventErr.Reason)
			raw, err := st.LoadMCPEventsCatalog(t.Context())
			require.NoError(err)
			assert.Empty(raw)
		}},
		{"unrelated catalog change", func(t *testing.T, st *store.Store, registry *agentgrant.Registry, id string, service *Service, now *time.Time) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			var err error
			calls := 0
			verify := func(ctx context.Context, _ string, _ string, _ []byte) error {
				calls++
				ok, err := st.SwapMCPEventsCatalog(ctx, nil, []byte(`[]`), "", nil, "")
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("catalog changed twice")
				}
				return nil
			}
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err = service.Subscribe(t.Context(), in, agentgrant.Owner(id), verify, nil)
			require.NoError(err)
			assert.Equal(1, calls)
		}},
		{"concurrent verified pair", func(t *testing.T, st *store.Store, registry *agentgrant.Registry, id string, service *Service, now *time.Time) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			var err error
			owner := agentgrant.Owner(id)
			request := func(secret string) MCPEventsSubscribeRequest {
				return preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: secret}})
			}
			first := request("whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh")
			second := request("whsec_YmJiYmJiYmJiYmJiYmJiYmJiYmJiYmJi")
			var challenged string
			var winner MCPEventsSubscribeResult
			verifyLoser := func(ctx context.Context, _ string, id string, secret []byte) error {
				assert.Equal(first.secret, secret)
				winner, err = service.Subscribe(ctx, second, owner, func(_ context.Context, _ string, id string, secret []byte) error {
					assert.Equal(second.secret, secret)
					challenged = id
					return nil
				}, nil)
				require.NoError(err)
				assert.NotEqual(id, challenged)
				return nil
			}
			_, err = service.Subscribe(t.Context(), first, owner, verifyLoser, nil)
			var failure *Error
			require.ErrorAs(err, &failure)
			assert.Equal(Unavailable, failure.Code)
			assert.Equal("verification_changed", failure.Reason)
			assert.Equal(challenged, winner.ID)
			targets, err := service.Native(t.Context())
			require.NoError(err)
			require.Len(targets, 1)
			assert.Equal(winner.ID, targets[0].Name)
			credentials, err := providercredentials.ReadEvents(service.TokensDir)
			require.NoError(err)
			origin, err := callbackOrigin(first.Delivery.URL)
			require.NoError(err)
			encoded, _, err := credentials.Resolve(providercredentials.EventsID(winner.ID), origin, "", nil)
			require.NoError(err)
			var private privateCallback
			require.NoError(json.Unmarshal([]byte(encoded), &private))
			secret, err := base64.StdEncoding.DecodeString(private.Secret)
			require.NoError(err)
			assert.Equal(second.secret, secret)
			var retryID string
			retry, err := service.Subscribe(t.Context(), first, owner, func(_ context.Context, _ string, id string, secret []byte) error {
				retryID = id
				assert.Equal(first.secret, secret)
				return nil
			}, nil)
			require.NoError(err)
			assert.Equal(winner.ID, retryID)
			assert.Equal(winner.ID, retry.ID)
			credentials, err = providercredentials.ReadEvents(service.TokensDir)
			require.NoError(err)
			encoded, _, err = credentials.Resolve(providercredentials.EventsID(retry.ID), origin, "", nil)
			require.NoError(err)
			require.NoError(json.Unmarshal([]byte(encoded), &private))
			secret, err = base64.StdEncoding.DecodeString(private.Secret)
			require.NoError(err)
			assert.Equal(first.secret, secret)
		}},
		{"expiry during challenge", func(t *testing.T, st *store.Store, registry *agentgrant.Registry, id string, service *Service, now *time.Time) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			var err error
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			owner := agentgrant.Owner(id)
			verify := func(context.Context, string, string, []byte) error { return nil }
			first, err := service.Subscribe(t.Context(), in, owner, verify, nil)
			require.NoError(err)
			_, err = service.Subscribe(t.Context(), in, owner, func(_ context.Context, _ string, challenged string, _ []byte) error {
				assert.Equal(first.ID, challenged)
				*now = first.RefreshBefore
				return nil
			}, nil)
			var failure *Error
			require.ErrorAs(err, &failure)
			assert.Equal(Unavailable, failure.Code)
			assert.Equal("verification_changed", failure.Reason)
			_, oldPosition, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(first.ID))
			require.NoError(err)
			assert.Nil(oldPosition)
			_, gapID, err := testutil.CreateIndexedSourceMessage(t.Context(), st, "gap@example.test", "gap", "gap", "")
			require.NoError(err)
			checks := 0
			var retryID string
			retry, err := service.Subscribe(t.Context(), in, owner, func(_ context.Context, _ string, challenged string, _ []byte) error {
				retryID = challenged
				return nil
			}, func(context.Context, bool) error { checks++; return nil })
			require.NoError(err)
			assert.NotEqual(first.ID, retry.ID)
			assert.Equal(retryID, retry.ID)
			assert.GreaterOrEqual(checks, 1)
			state, _, err := st.LoadWakeupStateKey(t.Context(), store.MCPEventsPositionKey(retry.ID))
			require.NoError(err)
			assert.False(state.At.IsZero())
			page, err := st.ListChangedMessages(t.Context(), store.ChangedMessagesFrom(state.At), 100)
			require.NoError(err)
			for _, message := range page.Messages {
				assert.NotEqual(gapID, message.ID)
			}
		}},
		{"expiry between requests", func(t *testing.T, st *store.Store, registry *agentgrant.Registry, id string, service *Service, now *time.Time) {
			t.Helper()
			require := require.New(t)
			assert := assert.New(t)
			var err error
			owner := agentgrant.Owner(id)
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/private?token=fixture", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			first, err := service.Subscribe(t.Context(), in, owner, acceptCallback, nil)
			require.NoError(err)
			raw, err := st.LoadMCPEventsCatalog(t.Context())
			require.NoError(err)
			assert.NotContains(string(raw), in.Delivery.URL)
			*now = first.RefreshBefore.Add(time.Second)
			next, err := service.Subscribe(t.Context(), in, owner, acceptCallback, nil)
			require.NoError(err)
			assert.NotEqual(first.ID, next.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newEventsTestStore(t)
			registry := agentgrant.NewRegistry()
			t.Cleanup(registry.Close)
			id, _, _, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "events@example.test"}}, time.Time{})
			require.NoError(t, err)
			now := time.Now().UTC()
			service := &Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: registry, Now: func() time.Time { return now }}
			tc.run(t, st, registry, id, service, &now)
		})
	}
}

func TestGrantLeaseRefreshAndOwnership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := newEventsTestStore(t)
	registry := agentgrant.NewRegistry()
	expiry := time.Now().Add(3 * time.Hour).UTC()
	a, _, _, err := registry.Issue("a", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "events@example.test"}}, expiry)
	require.NoError(err)
	b, _, _, err := registry.Issue("b", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "events@example.test"}}, time.Time{})
	require.NoError(err)
	now := time.Now().UTC()
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: registry, Now: func() time.Time { return now }}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	verify := func(context.Context, string, string, []byte) error { return nil }
	in.TTL = TTL{set: true, ms: int64(2 * time.Hour / time.Millisecond)}
	first, err := service.Subscribe(t.Context(), in, agentgrant.Owner(a), verify, nil)
	require.NoError(err)
	now = now.Add(90 * time.Minute)
	second, err := service.Subscribe(t.Context(), in, agentgrant.Owner(a), verify, nil)
	require.NoError(err)
	assert.Equal(first.ID, second.ID)
	assert.True(second.RefreshBefore.After(first.RefreshBefore))
	assert.Equal(expiry, second.RefreshBefore)
	entries, _, err := service.catalog(t.Context())
	require.NoError(err)
	require.Len(entries, 1)
	assert.Equal(expiry, entries[0].Expires)
	unsub := MCPEventsUnsubscribeRequest{Name: Name, Arguments: in.Arguments, Delivery: in.Delivery}
	err = service.Unsubscribe(t.Context(), unsub, agentgrant.Owner(b))
	var eventErr *Error
	require.ErrorAs(err, &eventErr)
	assert.Equal(NotFound, eventErr.Code)
	entries, _, err = service.catalog(t.Context())
	require.NoError(err)
	assert.Len(entries, 1)
}

func TestCreationAdmissionBeforeAndAfterVerification(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	registry := agentgrant.NewRegistry()
	id, _, _, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "test", Identifier: "reader@example.test"}}, time.Time{})
	require.NoError(err)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: registry}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	verified, allowed := false, false
	var steps []string
	verify := func(context.Context, string, string, []byte) error {
		verified = true
		steps = append(steps, "verify")
		return nil
	}
	check := func(_ context.Context, creating bool) error {
		if !creating {
			steps = append(steps, "refresh")
			return nil
		}
		steps = append(steps, "creation")
		if !allowed {
			return failure(Forbidden, "person_scope_exceeds_grant")
		}
		return nil
	}
	_, err = service.Subscribe(t.Context(), in, agentgrant.Owner(id), verify, check)
	var eventErr *Error
	require.ErrorAs(err, &eventErr)
	assert.Equal("person_scope_exceeds_grant", eventErr.Reason)
	assert.False(verified)
	assert.Equal([]string{"creation"}, steps)
	allowed, verified, steps = true, false, nil
	first, err := service.Subscribe(t.Context(), in, agentgrant.Owner(id), verify, check)
	require.NoError(err)
	assert.Equal([]string{"creation", "verify", "creation"}, steps)
	allowed, verified, steps = false, false, nil
	second, err := service.Subscribe(t.Context(), in, agentgrant.Owner(id), verify, check)
	require.NoError(err)
	assert.True(verified)
	assert.Equal([]string{"verify", "refresh"}, steps)
	assert.Equal(first.ID, second.ID)
}

func TestGrantSubscriptionCap(t *testing.T) {
	t.Run("before verification", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		ctx := t.Context()
		st := newEventsTestStore(t)
		registry := agentgrant.NewRegistry()
		id, _, _, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "events@example.test"}}, time.Time{})
		require.NoError(err)
		service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: registry}
		for i := range 4 {
			in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: fmt.Sprintf("https://example.com/%d", i), Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
			_, err = service.Subscribe(ctx, in, agentgrant.Owner(id), acceptCallback, nil)
			require.NoError(err)
		}
		_, err = service.Subscribe(ctx, preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/fifth", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}}), agentgrant.Owner(id), nil, nil)
		var failure *Error
		require.ErrorAs(err, &failure)
		assert.Equal(ResourceExhausted, failure.Code)
	})
	t.Run("concurrent after verification", func(t *testing.T) {
		requirements := require.New(t)
		assertions := assert.New(t)
		st := newEventsTestStore(t)
		registry := agentgrant.NewRegistry()
		id, _, _, err := registry.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: 1, Type: "imap", Identifier: "events@example.test"}}, time.Time{})
		requirements.NoError(err)
		service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true, Grants: registry}
		owner := agentgrant.Owner(id)
		request := func(url string) MCPEventsSubscribeRequest {
			return preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: url, Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
		}
		verify := func(context.Context, string, string, []byte) error { return nil }
		for i := range 3 {
			_, err = service.Subscribe(t.Context(), request(fmt.Sprintf("https://example.com/%d", i)), owner, verify, nil)
			requirements.NoError(err)
		}
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		racingVerify := func(ctx context.Context, _ string, _ string, _ []byte) error {
			ready <- struct{}{}
			<-release
			return ctx.Err()
		}
		results := make(chan error, 2)
		for i := 3; i < 5; i++ {
			in := request(fmt.Sprintf("https://example.com/%d", i))
			go func() { _, err := service.Subscribe(t.Context(), in, owner, racingVerify, nil); results <- err }()
		}
		<-ready
		<-ready
		close(release)
		success, exhausted := 0, 0
		for range 2 {
			err := <-results
			if err == nil {
				success++
				continue
			}
			var eventErr *Error
			requirements.ErrorAs(err, &eventErr)
			if eventErr.Code == ResourceExhausted {
				exhausted++
			}
		}
		assertions.Equal(1, success)
		assertions.Equal(1, exhausted)
		entries, _, err := service.catalog(t.Context())
		requirements.NoError(err)
		assertions.Len(entries, 4)
	})
}

func TestCatalogIdentityRequiresExactWidth(t *testing.T) {
	incoming := true
	args := MCPEventsArguments{SourceID: 1, IncomingOnly: &incoming}
	endpoint := "https://example.com/hook"
	for _, tc := range []struct {
		owner, suffix string
		match         bool
	}{
		{"owner", "", true},
		{"owner", "0123456789abcdef0123456789abcdef", false},
		{"grant:fixture", "0123456789abcdef0123456789abcdef", true},
		{"grant:fixture", "", false},
	} {
		t.Run(tc.owner+tc.suffix, func(t *testing.T) {
			id := subscriptionID(tc.owner, args, endpoint) + tc.suffix
			entries := []entry{{ID: id, Owner: tc.owner, Arguments: args}}
			assert.Equal(t, tc.match, find(entries, tc.owner, args, endpoint) == 0)
		})
	}
}

func TestNativeRejectsSuffixedOwnerIdentity(t *testing.T) {
	requirements := require.New(t)
	st := newEventsTestStore(t)
	service := Service{Store: st, TokensDir: t.TempDir(), Enabled: true}
	in := preparedSubscribe(t, MCPEventsSubscribeRequest{Name: Name, Arguments: MCPEventsArguments{SourceID: 1}, Delivery: MCPEventsDelivery{Mode: "webhook", URL: "https://example.com/hook", Secret: "whsec_YWFhYWFhYWFhYWFhYWFhYWFhYWFhYWFh"}})
	created, err := service.Subscribe(t.Context(), in, "owner", acceptCallback, nil)
	requirements.NoError(err)
	credentials, err := providercredentials.ReadEvents(service.TokensDir)
	requirements.NoError(err)
	value, _, err := credentials.Resolve(providercredentials.EventsID(created.ID), "https://example.com", "", nil)
	requirements.NoError(err)
	badID := created.ID + "0123456789abcdef0123456789abcdef"
	revision, err := credentials.Revision(providercredentials.EventsID(badID))
	requirements.NoError(err)
	_, err = providercredentials.PutIfRevision(service.TokensDir, revision, providercredentials.EventsID(badID), "https://example.com", value)
	requirements.NoError(err)
	entries, raw, err := service.catalog(t.Context())
	requirements.NoError(err)
	requirements.Len(entries, 1)
	entries[0].ID = badID
	next, err := json.Marshal(entries)
	requirements.NoError(err)
	ok, err := st.SwapMCPEventsCatalog(t.Context(), raw, next, "", nil, "")
	requirements.NoError(err)
	requirements.True(ok)
	targets, err := service.Native(t.Context())
	requirements.NoError(err)
	assert.Empty(t, targets)
}
