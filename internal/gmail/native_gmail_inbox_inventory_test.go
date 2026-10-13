package gmail_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// Native inventory, not the archive label's mutable name association, determines
// inbox authority. The real Gmail parser and Syncer own every observation.
func TestNativeGmailInboxInventoryLifetimes(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	labels := []map[string]string{{"id": "label-a", "name": "Alpha", "type": "user"}, {"id": "label-b", "name": "Beta", "type": "user"}}
	var fail atomic.Bool
	var snapshot atomic.Value
	snapshot.Store(labels)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			value = map[string]any{"emailAddress": "owner@example.net", "historyId": "10", "messagesTotal": 0, "threadsTotal": 0}
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if fail.Load() {
				http.Error(w, "synthetic incomplete inventory", http.StatusBadRequest)
				return
			}
			value = map[string]any{"labels": snapshot.Load()}
		case strings.HasSuffix(r.URL.Path, "/messages"):
			value = map[string]any{"messages": []any{}, "resultSizeEstimate": 0}
		default:
			http.Error(w, "unknown synthetic route", http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(provider.Close)
	native := gmail.NewNativePermissionFixtureClient(provider.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
	t.Cleanup(func() { assert.NoError(native.Close()) })
	opts := msgsync.DefaultOptions()
	opts.NoResume = true
	worker := msgsync.New(native, st, opts)
	sync := func() { _, err := worker.Full(t.Context(), "owner@example.net"); require.NoError(err) }
	sync()
	source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
	require.NoError(err)
	require.NotNil(source)
	page := func() map[string]store.MCPResource {
		p, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: source.ID})
		require.NoError(err)
		found := map[string]store.MCPResource{}
		for _, r := range p.Resources {
			assert.Equal(source.ID, r.SourceID)
			found[r.ProviderID] = r
		}
		return found
	}
	first := page()
	require.Len(first, 2)
	assert.Equal("Alpha", first["label-a"].DisplayName)
	assert.Equal("Beta", first["label-b"].DisplayName)
	labels = []map[string]string{{"id": "label-a", "name": "Beta", "type": "user"}, {"id": "label-b", "name": "Alpha", "type": "user"}}
	snapshot.Store(labels)
	sync()
	renamed := page()
	require.Len(renamed, 2)
	assert.Equal(first["label-a"].Reference, renamed["label-a"].Reference)
	assert.Equal(first["label-b"].Reference, renamed["label-b"].Reference)
	assert.Equal("Beta", renamed["label-a"].DisplayName)
	fail.Store(true)
	_, err = worker.Full(t.Context(), "owner@example.net")
	require.Error(err)
	partial := page()
	require.Len(partial, 2)
	assert.Equal(renamed["label-a"].Reference, partial["label-a"].Reference, "failed enumeration must not assert native absence")
	fail.Store(false)
	labels = []map[string]string{{"id": "label-c", "name": "Beta", "type": "user"}}
	snapshot.Store(labels)
	sync()
	replacement := page()
	require.Len(replacement, 1)
	assert.Contains(replacement, "label-c")
	assert.NotEqual(first["label-a"].Reference, replacement["label-c"].Reference, "same name does not preserve identity")
	_, err = st.GetMCPResource(t.Context(), "inbox", first["label-a"].ID)
	require.Error(err, "retained archive labels cannot authorize a removed inbox")
	labels = []map[string]string{}
	snapshot.Store(labels)
	sync()
	assert.Empty(page())
	var archived int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM labels WHERE source_id=?`), source.ID).Scan(&archived))
	assert.Positive(archived, "native removal keeps ordinary archive labels")
	labels = []map[string]string{{"id": "label-a", "name": "Alpha", "type": "user"}}
	snapshot.Store(labels)
	sync()
	restored := page()
	require.Len(restored, 1)
	assert.NotEqual(first["label-a"].Reference, restored["label-a"].Reference, "observed removal and restoration cannot revive an old reference")
}

func TestNativeGmailInboxNoopSyncRefreshesInventory(t *testing.T) {
	assert, require := Assert.New(t), Require.New(t)
	st := testutil.NewTestStore(t)
	labels := []map[string]string{{"id": "label-a", "name": "Alpha", "type": "user"}, {"id": "label-b", "name": "Beta", "type": "user"}}
	var fail atomic.Bool
	var snapshot atomic.Value
	snapshot.Store(labels)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var value any
		switch {
		case strings.HasSuffix(r.URL.Path, "/profile"):
			value = map[string]any{"emailAddress": "owner@example.net", "historyId": "10", "messagesTotal": 0, "threadsTotal": 0}
		case strings.HasSuffix(r.URL.Path, "/labels"):
			if fail.Load() {
				http.Error(w, "synthetic incomplete inventory", http.StatusBadRequest)
				return
			}
			value = map[string]any{"labels": snapshot.Load()}
		case strings.HasSuffix(r.URL.Path, "/messages"):
			value = map[string]any{"messages": []any{}, "resultSizeEstimate": 0}
		default:
			http.Error(w, "unknown synthetic route", http.StatusNotFound)
			return
		}
		if err := json.NewEncoder(w).Encode(value); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(provider.Close)
	native := gmail.NewNativePermissionFixtureClient(provider.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
	t.Cleanup(func() { assert.NoError(native.Close()) })
	opts := msgsync.DefaultOptions()
	opts.NoResume = true
	worker := msgsync.New(native, st, opts)
	sync := func() { _, err := worker.Full(t.Context(), "owner@example.net"); require.NoError(err) }
	sync()
	source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
	require.NoError(err)
	require.NotNil(source)
	page := func() map[string]store.MCPResource {
		p, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: source.ID})
		require.NoError(err)
		found := map[string]store.MCPResource{}
		for _, r := range p.Resources {
			assert.Equal(source.ID, r.SourceID)
			found[r.ProviderID] = r
		}
		return found
	}
	require.Len(page(), 2)
	snapshot.Store([]map[string]string{})
	source, err = st.GetSourceByID(source.ID)
	require.NoError(err)
	_, err = worker.Incremental(t.Context(), source)
	require.NoError(err)
	assert.Empty(page(), "an unchanged message history does not prove label inventory is unchanged")
}

func TestNativeGmailInboxListAuthorizationBoundary(t *testing.T) {
	for _, tc := range []struct {
		status  int
		allowed bool
	}{{400, true}, {401, false}, {403, false}, {404, true}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			assert, require := Assert.New(t), Require.New(t)
			st := testutil.NewTestStore(t)
			labels := []map[string]string{{"id": "label-a", "name": "Alpha", "type": "user"}, {"id": "label-b", "name": "Beta", "type": "user"}}
			var status atomic.Int64
			var snapshot atomic.Value
			snapshot.Store(labels)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var value any
				switch {
				case strings.HasSuffix(r.URL.Path, "/profile"):
					value = map[string]any{"emailAddress": "owner@example.net", "historyId": "10", "messagesTotal": 0, "threadsTotal": 0}
				case strings.HasSuffix(r.URL.Path, "/labels"):
					if status.Load() != 0 {
						http.Error(w, "synthetic inventory failure", int(status.Load()))
						return
					}
					value = map[string]any{"labels": snapshot.Load()}
				case strings.HasSuffix(r.URL.Path, "/messages"):
					value = map[string]any{"messages": []any{}, "resultSizeEstimate": 0}
				default:
					http.Error(w, "unknown synthetic route", http.StatusNotFound)
					return
				}
				if err := json.NewEncoder(w).Encode(value); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			t.Cleanup(provider.Close)
			native := gmail.NewNativePermissionFixtureClient(provider.URL, func() []string { return []string{oauth.ScopeGmailReadonly} })
			t.Cleanup(func() { assert.NoError(native.Close()) })
			opts := msgsync.DefaultOptions()
			opts.NoResume = true
			worker := msgsync.New(native, st, opts)
			sync := func() { _, err := worker.Full(t.Context(), "owner@example.net"); require.NoError(err) }
			sync()
			source, err := st.GetSourceByTypeAndIdentifier("gmail", "owner@example.net")
			require.NoError(err)
			require.NotNil(source)
			page := func() map[string]store.MCPResource {
				p, err := st.ListMCPResourcePage(t.Context(), store.MCPResourcePageOptions{Principal: "synthetic-owner", Kind: "inbox", SourceID: source.ID})
				require.NoError(err)
				found := map[string]store.MCPResource{}
				for _, r := range p.Resources {
					assert.Equal(source.ID, r.SourceID)
					found[r.ProviderID] = r
				}
				return found
			}
			require.Len(page(), 2)
			before, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			status.Store(int64(tc.status))
			source, err = st.GetSourceByID(source.ID)
			require.NoError(err)
			_, err = worker.Incremental(t.Context(), source)
			require.Error(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal(tc.allowed, allowed, "native denial must revoke retained authority; other inventory failures cannot assert removal")
			after, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			if tc.allowed {
				assert.Equal(before.Reference, after.Reference)
				assert.Len(page(), 2)
			} else {
				assert.NotEqual(before.Reference, after.Reference)
				assert.Empty(page())
			}
		})
	}
}
