package gmail_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested scopes.
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

// The real native HTTP adapter and Syncer must persist permission evidence.
// Every synthetic listing succeeds, so successful sync alone cannot distinguish
// the allowed grant from label-only, metadata-only or legacy-unknown grants.
func TestNativeGmailSyncPersistsVerifiedPermission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scopes  []string
		allowed bool
	}{
		{"read_only", []string{oauth.ScopeGmailReadonly}, true},
		{"modify", []string{oauth.ScopeGmailModify}, true},
		{"labels_only", []string{oauth.ScopeGmailLabels}, false},
		{"metadata_only", []string{oauth.ScopeGmailMetadata}, false},
		{"legacy_unknown", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			server := syntheticNativePermissionGmail(t, func() string { return "owner@example.net" }, nil)
			client := gmail.NewNativePermissionFixtureClient(server.URL, func() []string { return tc.scopes })
			t.Cleanup(func() { require.NoError(client.Close()) })
			options := msgsync.DefaultOptions()
			options.NoResume = true
			worker := msgsync.New(client, st, options)
			_, err := worker.Full(t.Context(), "owner@example.net")
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
			require.NoError(err)
			require.NotNil(source)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal(tc.allowed, allowed)
		})
	}
}

func TestNativeGmailSyncPermissionDowngradeRetiresLifetime(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "owner:synthetic", Capabilities: []store.MCPEventCapability{{Family: "msgvault.source_message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(err)
	server := syntheticNativePermissionGmail(t, func() string { return "owner@example.net" }, nil)
	var granted atomic.Value
	granted.Store([]string{oauth.ScopeGmailReadonly})
	client := gmail.NewNativePermissionFixtureClient(server.URL, func() []string {
		scopes, ok := granted.Load().([]string)
		require.True(ok)
		return scopes
	})
	t.Cleanup(func() { require.NoError(client.Close()) })
	options := msgsync.DefaultOptions()
	options.NoResume = true
	worker := msgsync.New(client, st, options)
	_, err = worker.Full(t.Context(), "owner@example.net")
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
	require.NoError(err)
	require.NotNil(source)
	resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	_, binding, err := st.ResolveMCPSubscriptionResource(t.Context(), "msgvault.source_message_archived", "source", source.ID, []string{"message"}, resource.Reference)
	require.NoError(err)
	granted.Store([]string{oauth.ScopeGmailLabels})
	_, err = worker.Full(t.Context(), "owner@example.net")
	require.NoError(err)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
	require.Error(st.CheckMCPResourceBinding(t.Context(), binding))
	granted.Store([]string{oauth.ScopeGmailReadonly})
	_, err = worker.Full(t.Context(), "owner@example.net")
	require.NoError(err)
	allowed, err = st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.True(allowed)
	assert.Error(st.CheckMCPResourceBinding(t.Context(), binding), "native restoration cannot revive an old subscription")
}

func TestNativeGmailProfileMismatchRevokesPriorPermission(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	_, configureErr := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, Principal: "synthetic-owner", Capabilities: []store.MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}}})
	require.NoError(configureErr)
	var profile atomic.Value
	profile.Store("owner@example.net")
	server := syntheticNativePermissionGmail(t, func() string { value, _ := profile.Load().(string); return value }, nil)
	client := gmail.NewNativePermissionFixtureClient(server.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
	t.Cleanup(func() { require.NoError(client.Close()) })
	options := msgsync.DefaultOptions()
	options.NoResume = true
	worker := msgsync.New(client, st, options)
	_, err := worker.Full(t.Context(), "owner@example.net")
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
	require.NoError(err)
	require.NotNil(source)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	require.True(allowed)
	profile.Store("foreign@example.net")
	_, err = worker.Full(t.Context(), "owner@example.net")
	require.Error(err)
	allowed, err = st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
}

func TestNativeGmailPermissionObservationPinsPreNetworkLifetime(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.net")
	require.NoError(err)
	before, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	var once atomic.Bool
	observed := make(chan error, 1)
	server := syntheticNativePermissionGmail(t, func() string { return "owner@example.net" }, func() {
		if !once.CompareAndSwap(false, true) {
			return
		}
		first := st.UpdateSourceIdentifier(source.ID, "replacement@example.net")
		second := st.UpdateSourceIdentifier(source.ID, "owner@example.net")
		observed <- errors.Join(first, second)
	})
	client := gmail.NewNativePermissionFixtureClient(server.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
	t.Cleanup(func() { require.NoError(client.Close()) })
	options := msgsync.DefaultOptions()
	options.NoResume = true
	worker := msgsync.New(client, st, options)
	_, err = worker.Full(t.Context(), "owner@example.net")
	require.Error(err)
	select {
	case mutationErr := <-observed:
		require.NoError(mutationErr)
	default:
		require.Fail("native profile request was not observed")
	}
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
	after, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.NotEqual(before.Reference, after.Reference)
}

func syntheticNativePermissionGmail(t *testing.T, profile func() string, onProfile func(), statuses ...func() int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			if len(statuses) > 0 && statuses[0]() != http.StatusOK {
				w.WriteHeader(statuses[0]())
				_, _ = w.Write([]byte(`{"error":{"message":"synthetic access unavailable","errors":[{"reason":"insufficientPermissions"}]}}`))
				return
			}
			if onProfile != nil {
				onProfile()
			}
			value = map[string]any{"emailAddress": profile(), "historyId": "10", "messagesTotal": 0, "threadsTotal": 0}
		case strings.HasSuffix(r.URL.Path, "/labels"):
			value = map[string]any{"labels": []any{}}
		case strings.HasSuffix(r.URL.Path, "/messages"):
			value = map[string]any{"messages": []any{}, "resultSizeEstimate": 0}
		case strings.HasSuffix(r.URL.Path, "/history"):
			value = map[string]any{"history": []any{}, "historyId": "10"}
		default:
			http.Error(w, "synthetic unknown native route", http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			http.Error(w, "synthetic response failure", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestNativeGmailObservedProfileDenialRetiresPermission(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		allowed bool
	}{
		{"unauthorized", http.StatusUnauthorized, false},
		{"permission_denied", http.StatusForbidden, false},
		{"unknown_missing_endpoint", http.StatusNotFound, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			var status atomic.Int64
			status.Store(http.StatusOK)
			server := syntheticNativePermissionGmail(t, func() string { return "owner@example.net" }, nil, func() int { return int(status.Load()) })
			client := gmail.NewNativePermissionFixtureClient(server.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
			t.Cleanup(func() { require.NoError(client.Close()) })
			options := msgsync.DefaultOptions()
			options.NoResume = true
			worker := msgsync.New(client, st, options)
			_, err := worker.Full(t.Context(), "owner@example.net")
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
			require.NoError(err)
			require.NotNil(source)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			require.True(allowed)
			status.Store(int64(tc.status))
			_, err = worker.Full(t.Context(), "owner@example.net")
			require.Error(err)
			allowed, err = st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal(tc.allowed, allowed)
		})
	}
}

func TestNativeGmailIncrementalNoopObservesPermissionDowngrade(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	server := syntheticNativePermissionGmail(t, func() string { return "owner@example.net" }, nil)
	var granted atomic.Value
	granted.Store([]string{oauth.ScopeGmailReadonly})
	client := gmail.NewNativePermissionFixtureClient(server.URL, func() []string {
		scopes, ok := granted.Load().([]string)
		require.True(ok)
		return scopes
	})
	t.Cleanup(func() { require.NoError(client.Close()) })
	options := msgsync.DefaultOptions()
	options.NoResume = true
	worker := msgsync.New(client, st, options)
	_, err := worker.Full(t.Context(), "owner@example.net")
	require.NoError(err)
	source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
	require.NoError(err)
	require.NotNil(source)
	allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	require.True(allowed)
	granted.Store([]string{oauth.ScopeGmailLabels})
	_, err = worker.Incremental(t.Context(), source)
	require.NoError(err)
	allowed, err = st.NativeSourceMessagePermission(t.Context(), source.ID)
	require.NoError(err)
	assert.False(allowed)
}
