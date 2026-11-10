package cmd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestViewsPinExcludeRoundTripAndRetry(t *testing.T) {
	f := storetest.New(t)
	id := f.CreateMessage("cli-pin")
	view, err := f.Store.CreateSavedView(t.Context(), store.SavedViewInput{Name: "CLI view", CanonicalState: []byte(`{}`), SchemaVersion: 2})
	require.NoError(t, err)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "synthetic-views-key"}}
	logger := slog.New(slog.DiscardHandler)
	srv := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: &storeAPIAdapter{store: f.Store, config: cfg, logger: logger}, SavedViewStore: f.Store, Logger: logger})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context()))) })
	var conflict atomic.Bool
	conflict.Store(true)
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && conflict.CompareAndSwap(true, false) {
			_, err := f.Store.UpdateSavedView(r.Context(), view.ID, 1, store.SavedViewInput{Name: view.Name, CanonicalState: []byte(`{}`), SchemaVersion: 2})
			assert.NoError(t, err)
		}
		srv.Router().ServeHTTP(w, r)
	}))
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	key := fmt.Sprintf("source:%d:message:cli-pin", f.Source.ID)
	run := func(args ...string) (string, error) {
		cmd := newViewsCommand(func(context.Context) (viewsClient, func(), error) { return client, func() {}, nil })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	pinOutput, err := run("pin", "CLI view", key)
	require.NoError(t, err)
	assert.Contains(t, pinOutput, "include +1 -0; exclude +0 -0")
	pinOutput, err = run("pin", "CLI view", key)
	require.NoError(t, err)
	assert.Contains(t, pinOutput, "include +0 -0; exclude +0 -0")
	items, err := f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	require.Len(t, items.Include, 1)
	assert.Equal(t, id, items.Include[0].MessageID)
	_, err = run("exclude", "CLI view", key)
	require.NoError(t, err)
	items, err = f.Store.GetSavedViewItems(t.Context(), view.ID)
	require.NoError(t, err)
	require.Len(t, items.Exclude, 1)
	out, err := run("show", "CLI view", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, "cli-pin")
	_, err = run("pin", "CLI view", "source:999999:message:missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saved_view_items_rejected")
}

// The daemon wire fixture isolates the CLI's text rendering contract.
func TestViewsRunMarksPinnedFiles(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = fmt.Fprint(w, `{"api_schema_version":"3.45.0","status":"ok"}`)
		case "/api/v1/saved-views/7":
			_, _ = fmt.Fprint(w, `{"id":7,"name":"Files","canonical_state":{"presentation":"files"},"schema_version":2,"revision":1}`)
		case "/api/v1/saved-views/7/run":
			_, _ = fmt.Fprint(w, `{"saved_view":{"id":7,"name":"Files","canonical_state":{"presentation":"files"},"schema_version":2,"revision":1,"items":{"include":[{"entry_key":"source:1:message:pinned","message_id":1,"source_id":1,"kind":"message","added_by":"owner"}],"exclude":[]}},"result_kind":"files","files":[{"filename":"pinned.pdf","entry_key":"source:1:message:pinned"},{"filename":"ordinary.pdf","entry_key":"source:1:message:ordinary"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, APIKey: "synthetic-key", AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(t, err)
	defer func() { assert.NoError(t, client.Close()) }()
	command := newViewsCommand(func(context.Context) (viewsClient, func(), error) { return client, func() {}, nil })
	var out bytes.Buffer
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"run", "7"})
	require.NoError(t, command.Execute())
	assert.Contains(t, out.String(), "+ pinned.pdf  source:1:message:pinned")
	assert.Contains(t, out.String(), "  ordinary.pdf  source:1:message:ordinary")
}
