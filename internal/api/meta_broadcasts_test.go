package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type broadcastRouteStore struct {
	mockStore

	archive *store.Store
}

func (s *broadcastRouteStore) ListBroadcastsContext(ctx context.Context, id int64) ([]store.Broadcast, error) {
	return s.archive.ListBroadcastsContext(ctx, id)
}
func (s *broadcastRouteStore) GetBroadcastContext(ctx context.Context, id string) (store.Broadcast, []store.BroadcastRecipient, error) {
	return s.archive.GetBroadcastContext(ctx, id)
}
func (s *broadcastRouteStore) ListMarketingSubscriptionsContext(ctx context.Context, id int64, f store.SubscriptionFilter) ([]store.MarketingSubscription, error) {
	return s.archive.ListMarketingSubscriptionsContext(ctx, id, f)
}
func (s *broadcastRouteStore) ListSourcesContext(ctx context.Context, kind string) ([]*store.Source, error) {
	return s.archive.ListSourcesContext(ctx, kind)
}
func (s *broadcastRouteStore) GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error) {
	return s.archive.GetSourceByIDContext(ctx, id)
}
func broadcastRouteFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(meta.SourceTypeFacebookMessages, "app:9:page:100")
	require.NoError(t, err)
	sub, err := st.ApplyConsentWebhookContext(t.Context(), src.ID, store.ConsentWebhook{Token: "synthetic-mm-token-1", PSID: "300", At: time.Now(), EvidenceRef: "fixture:0"})
	require.NoError(t, err)
	b, err := st.CreateBroadcastContext(t.Context(), store.Broadcast{SourceID: src.ID, AdAccountID: "700", Name: "Offers", MessageJSON: []byte(`{"text":"Example offer"}`), DailyBudgetCents: 500, CampaignDays: 7}, []int64{sub.ID})
	require.NoError(t, err)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{AgentAccess: true}, MessengerBroadcasts: []config.MessengerBroadcastProfile{{Identifier: "example", AppID: "9", PageID: "100", AdAccountID: "700", Enabled: true}}}
	adapter := &broadcastRouteStore{archive: st}
	adapter.stats = &StoreStats{}
	srv := NewServer(cfg, adapter, nil, testLogger())
	srv.agentGrants = agentgrant.NewRegistry()
	_, secret, _, err := srv.agentGrants.Issue("fixture", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: src.ID, Type: meta.SourceTypeFacebookMessages, Identifier: src.Identifier}}, time.Time{})
	require.NoError(t, err)
	return srv, b.ID, secret
}
func TestBroadcastRoutesOwnerOnly(t *testing.T) {
	srv, id, secret := broadcastRouteFixture(t)
	for _, path := range []string{"/api/v1/meta/broadcasts?profile=example", "/api/v1/meta/broadcasts/" + id, "/api/v1/meta/broadcast-subscriptions?profile=example"} {
		for _, delegated := range []bool{false, true} {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "127.0.0.1:4242"
			if delegated {
				req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			if delegated {
				assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			} else {
				assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
			}
		}
	}
}
func TestBroadcastRouteNoTokens(t *testing.T) {
	assertions := assert.New(t)

	srv, id, _ := broadcastRouteFixture(t)
	for _, path := range []string{"/api/v1/meta/broadcasts?profile=example", "/api/v1/meta/broadcasts/" + id, "/api/v1/meta/broadcast-subscriptions?profile=example"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:4242"
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), "synthetic-mm-token")
		if path == "/api/v1/meta/broadcast-subscriptions?profile=example" {
			assertions.Contains(w.Body.String(), store.TokenFingerprint("synthetic-mm-token-1"))
		} else {
			assertions.Contains(w.Body.String(), `"message_json":{"text":"Example offer"}`)
		}
	}
}
