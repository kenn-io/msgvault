package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// catalogStore lets a test fail or block the daemon's virtual account
// catalog refresh while everything else reads the real archive.
type catalogStore struct {
	*store.Store

	mu      sync.Mutex
	fail    bool
	release chan struct{}
}

func (s *catalogStore) ListVirtualAccountsContext(ctx context.Context) (map[int64][]store.VirtualAccount, error) {
	s.mu.Lock()
	fail, release := s.fail, s.release
	s.mu.Unlock()
	if fail {
		return nil, errors.New("synthetic catalog failure")
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.Store.ListVirtualAccountsContext(ctx)
}

func TestStaleAccountCatalogNeverRejectsConfirmedAlias(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	require.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "thread", "")
	require.NoError(err)
	require.NoError(st.AddAccountIdentity(src.ID, "work@example.org", "manual"))
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "work", MessageType: "email"},
		RawMIME: []byte("X-Delivered-To: work@example.org\r\n\r\nbody"),
	})
	require.NoError(err)

	catalog := &catalogStore{Store: st}
	cfg := &config.Config{}
	cfg.Data.DataDir = t.TempDir()
	router := api.NewServerWithOptions(api.ServerOptions{Config: cfg, Store: catalog, Logger: slog.New(slog.DiscardHandler)}).Router()
	daemon := httptest.NewServer(router)
	t.Cleanup(daemon.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	adapter := daemonclient.NewEngineAdapter(client)
	h := &handlers{engine: adapter}

	_, err = h.resolveAccount(t.Context(), "work@example.org")
	require.NoError(err, "the warm catalog knows work@")
	require.NoError(st.AddAccountIdentity(src.ID, "alias@example.org", "manual"))

	workChild := func() bool {
		t.Helper()
		accounts, err := adapter.ListAccounts(t.Context())
		require.NoError(err)
		require.Len(accounts, 1)
		for _, v := range accounts[0].VirtualAccounts {
			if v.AccountAddress == "work@example.org" {
				return true
			}
		}
		return false
	}
	checkUnavailable := func(label string) {
		t.Helper()
		_, err := adapter.ListVirtualAccounts(t.Context())
		require.Error(err, label+": the stale catalog is flagged unavailable")
		assert.True(workChild(), label+": the last known children stay on the accounts")
		_, err = h.resolveAccount(t.Context(), "alias@example.org")
		require.Error(err, label)
		assert.NotContains(err.Error(), "account not found", label+": a stale catalog never claims the alias is unknown")
	}

	catalog.mu.Lock()
	catalog.fail = true
	catalog.mu.Unlock()
	checkUnavailable("failed refresh")

	release := make(chan struct{})
	catalog.mu.Lock()
	catalog.fail, catalog.release = false, release
	catalog.mu.Unlock()
	checkUnavailable("blocked refresh")

	catalog.mu.Lock()
	catalog.release = nil
	catalog.mu.Unlock()
	close(release)
	require.Eventually(func() bool {
		selection, err := h.resolveAccount(t.Context(), "alias@example.org")
		return err == nil && selection.scope != nil
	}, 10*time.Second, 100*time.Millisecond, "the refreshed catalog resolves the alias")
}
