package cmd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func broadcastWebhookConfig(t *testing.T) (*config.Config, *store.Store, int64) {
	t.Helper()
	cfg := webhookTestConfig(t)
	cfg.MessengerBroadcasts = []config.MessengerBroadcastProfile{{Identifier: "example-offers", AppID: "9", PageID: "100", Enabled: true}}
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource(meta.SourceTypeFacebookMessages, "app:9:page:100")
	require.NoError(t, err)
	return cfg, st, source.ID
}
func TestOptInForUnconfiguredPageUnrouted(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	cfg, st, _ := broadcastWebhookConfig(t)
	_, err := st.GetOrCreateSource(meta.SourceTypeFacebookMessages, "app:9:page:101")
	requirements.NoError(err)
	reg := metawebhook.NewRegistry()
	registerBroadcastWebhookHandlers(reg, cfg, st)
	h, ok := reg.Lookup("page", "messaging_optins")
	requirements.True(ok)
	out := h(t.Context(), "9", []metawebhook.Item{{AppID: "9", Object: "page", EntryID: "101", Field: "messaging_optins", Value: []byte(`{}`)}})
	requirements.Len(out, 1)
	assertions.Equal(metawebhook.Unrouted, out[0].Class)
	cfg.MessengerBroadcasts[0].Enabled = false
	out = h(t.Context(), "9", []metawebhook.Item{{AppID: "9", Object: "page", EntryID: "100", Field: "messaging_optins", Value: []byte(`{}`)}})
	assertions.Equal(metawebhook.Unrouted, out[0].Class)
}
func TestMarketingEventSignedReceiver(t *testing.T) {
	for _, shape := range []string{"messaging", "changes"} {
		t.Run(shape, func(t *testing.T) {
			assertions := assert.New(t)

			requirements := require.New(t)

			cfg, st, id := broadcastWebhookConfig(t)
			reg := metawebhook.NewRegistry()
			registerBroadcastWebhookHandlers(reg, cfg, st)
			_, ok := reg.Lookup("page", "messaging_optins")
			requirements.True(ok)
			runtime, err := startMetaWebhooks(t.Context(), cfg, st, nil, nil)
			requirements.NoError(err)
			defer stopWebhook(t, runtime)
			value := `{"sender":{"id":"300"},"recipient":{"id":"100"},"timestamp":1793577600123,"optin":{"type":"notification_messages","notification_messages_token":"synthetic-mm-token-1","title":"Example offers"}}`
			part := `"messaging":[` + value + `]`
			if shape == "changes" {
				part = `"changes":[{"field":"messaging_optins","value":` + value + `}]`
			}
			postWebhook(t, runtime, fmt.Sprintf(`{"object":"page","entry":[{"id":"100",%s}]}`, part))
			requirements.Eventually(func() bool {
				rows, err := st.ListMarketingSubscriptionsContext(context.Background(), id, store.SubscriptionFilter{})
				return err == nil && len(rows) == 1 && rows[0].ParticipantID.Valid
			}, 8*time.Second, 20*time.Millisecond)
			rows, err := st.ListMarketingSubscriptionsContext(t.Context(), id, store.SubscriptionFilter{})
			requirements.NoError(err)
			requirements.Len(rows, 1)
			events, err := st.LatestConsentEventsContext(t.Context(), []int64{rows[0].ID})
			requirements.NoError(err)
			requirements.Len(events[rows[0].ID], 1)
			assertions.Regexp(`^.+:0$`, events[rows[0].ID][0].EvidenceRef)
			assertions.Equal("active", rows[0].Status)
		})
	}
}
