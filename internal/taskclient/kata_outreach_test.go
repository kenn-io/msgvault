package taskclient_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
)

func outreachClient(t *testing.T, handler http.Handler) (*taskclient.KataClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: server.URL, APIKey: katatest.Token, DefaultProject: "example", HTTPClient: server.Client()})
	Require.NoError(t, err)
	return client, server
}

func TestOutreachRequiresCompleteKataVisibility(t *testing.T) {
	native := katatest.NewScoped(t)
	client, _ := outreachClient(t, native.Handler())
	_, _, err := client.FindActionTaskComplete(t.Context(), "absent-cycle")
	Require.ErrorIs(t, err, taskclient.ErrNotFound, "a scoped negative lookup must not authorize another create")
}

func TestOutreachNativeInboxAndReceiptAreAtomic(t *testing.T) {
	require := Require.New(t)

	native := katatest.New(t)
	var failAfterCommit atomic.Bool
	var injectOnce atomic.Bool
	injectOnce.Store(true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failAfterCommit.Load() && r.Method == http.MethodGet && r.URL.Path != "/api/v1/projects" && r.URL.Path != "/api/v1/health" {
			http.Error(w, "synthetic failed read", http.StatusServiceUnavailable)
			return
		}
		native.Service.Handler().ServeHTTP(w, r)
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/metadata") && injectOnce.CompareAndSwap(true, false) {
			failAfterCommit.Store(true)
		}
	})
	client, server := outreachClient(t, handler)
	issue, err := client.CreateTask(t.Context(), "example", "outreach-cycle", taskclient.KataCreate{Title: "Contact Example", Owner: "operator"})
	require.NoError(err)
	_, err = client.NotifyTaskOnce(t.Context(), issue, "cycle", "operator")
	require.Error(err, "the POST committed but the client's follow-up GET failed")
	failAfterCommit.Store(false)
	saved, err := client.GetTask(t.Context(), "example", issue.UID)
	require.NoError(err)
	key := "notify." + base64.RawURLEncoding.EncodeToString([]byte("operator"))
	require.Contains(saved.Metadata, key)
	require.Equal("operator", saved.Owner)
	// Clear the native inbox slot exactly as a recipient would. The durable receipt remains.
	_, err = client.MutateMetadata(t.Context(), "example", saved.UID, saved.Revision, map[string]any{key: nil})
	require.NoError(err)
	failAfterCommit.Store(false)
	saved, err = client.GetTask(t.Context(), "example", issue.UID)
	require.NoError(err)
	_, err = client.NotifyTaskOnce(t.Context(), saved, "cycle", "operator")
	require.NoError(err)
	saved, err = client.GetTask(t.Context(), "example", issue.UID)
	require.NoError(err)
	Assert.NotContains(t, saved.Metadata, key, "retry after acknowledgement must not re-notify")
	native.Endpoint(server).CloseIssue(t, issue.UID)
	saved, err = client.GetTask(t.Context(), "example", issue.UID)
	require.NoError(err)
	_, err = client.NotifyTaskOnce(t.Context(), saved, "cycle", "operator")
	require.NoError(err)
}

func TestOutreachPreservesUnrelatedInboxAttention(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	native := katatest.New(t)
	client, _ := outreachClient(t, native.Service.Handler())
	issue, err := client.CreateTask(t.Context(), "example", "other-attention", taskclient.KataCreate{Title: "Example"})
	require.NoError(err)
	key := "notify." + base64.RawURLEncoding.EncodeToString([]byte("operator"))
	issue, err = client.MutateMetadata(t.Context(), "example", issue.UID, issue.Revision, map[string]any{key: map[string]any{"from": "someone", "message": "Review another task"}})
	require.NoError(err)
	_, err = client.NotifyTaskOnce(context.Background(), issue, "cycle", "operator")
	require.ErrorIs(err, taskclient.ErrNotificationOccupied)
	require.NotErrorIs(err, taskclient.ErrConflict)
	saved, err := client.GetTask(t.Context(), "example", issue.UID)
	require.NoError(err)
	assert.Equal(issue.Metadata[key], saved.Metadata[key])
}

func TestOutreachCloseBeforeDeliveryIsProtectedByRevision(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)

	native := katatest.New(t)
	client, server := outreachClient(t, native.Service.Handler())
	issue, err := client.CreateTask(t.Context(), "example", "close-before-notify", taskclient.KataCreate{Title: "Example"})
	require.NoError(err)
	native.Endpoint(server).CloseIssue(t, issue.UID)
	saved, err := client.NotifyTaskOnce(t.Context(), issue, "cycle", "operator")
	require.NoError(err)
	assert.Equal("closed", saved.Status)
	assert.NotContains(saved.Metadata, taskclient.OutreachDeliveryMetadataKey)
}
