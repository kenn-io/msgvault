package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestServeDMCacheTracksActualChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	subscriptionFixture(t, func(w http.ResponseWriter, r *http.Request) {
		var response string
		switch r.URL.Path {
		case "/v26.0/me":
			response = `{"id":"100","name":"Example Page","category":"Community"}`
		case "/v26.0/100/conversations":
			response = `{"data":[{"id":"t_1","updated_time":"2026-11-01T00:00:00+0000","participants":{"data":[{"id":"300","name":"Example Customer"}]}}]}`
		case "/v26.0/t_1":
			response = `{"messages":{"data":[{"id":"m_1"}]}}`
		case "/v26.0/m_1":
			response = `{"id":"m_1","from":{"id":"300","name":"Example Customer"},"to":{"data":[{"id":"100"}]},"message":"original inbox","created_time":"2026-11-01T00:00:00+0000"}`
		default:
			assert.Fail("unexpected Graph fixture request", "%s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, response)
	})
	src := meta.DMSource{AppID: "9", PageID: "100", Platform: meta.PlatformMessenger}
	client, err := meta.NewClient("synthetic-page-token")
	require.NoError(err)
	imp := meta.NewDMImporter(st, client)
	_, err = imp.Register(t.Context(), src)
	require.NoError(err)
	_, err = imp.Import(t.Context(), src, meta.ImportOptions{})
	require.NoError(err)
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	_, err = imp.Import(t.Context(), src, meta.ImportOptions{})
	require.NoError(err)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "unchanged inbox polling must preserve the published cache")
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.FacebookMessages = []config.MetaMessagesSource{{Identifier: "example", AppID: "9", PageID: "100", Enabled: true}}
	editTimestamp := time.Now().Add(time.Hour).UnixMilli()
	edit := fmt.Sprintf(`{"sender":{"id":"300"},"recipient":{"id":"100"},"timestamp":%d,"message_edit":{"mid":"m_1","text":"updated inbox","num_edit":1}}`, editTimestamp)
	result := dmHandler(cfg, st)(t.Context(), "9", []metawebhook.Item{{Object: "page", EntryID: "100", Field: "message_edits", Value: []byte(edit)}})
	require.Len(result, 1)
	require.Equal(metawebhook.Applied, result[0].Class)
	assert.True(cacheNeedsBuild(dbPath, analyticsDir).FullRebuild, "an inbox edit must refresh an already published snippet")
	_, err = buildCacheAuto(dbPath, analyticsDir)
	require.NoError(err)
	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	var snippet string
	require.NoError(duckDB.QueryRowContext(t.Context(), `SELECT snippet FROM read_parquet(?, hive_partitioning=true)
		WHERE source_message_id = 'm_1'`, filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&snippet))
	assert.Equal("updated inbox", snippet)
	assert.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "the refreshed inbox cache must converge")
}

func TestServeRegistersDMHandlers(t *testing.T) {
	assert := assert.New(t)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewSQLiteTestStore(t)
	cfg.FacebookMessages = []config.MetaMessagesSource{{Identifier: "example", AppID: "9", PageID: "100", Enabled: true}}
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	for _, pair := range [][2]string{{"page", "messages"}, {"page", "message_echoes"}, {"page", "message_reactions"}, {"page", "message_edits"}, {"page", "standby"}, {"instagram", "messages"}, {"instagram", "message_reactions"}} {
		_, ok := reg.Lookup(pair[0], pair[1])
		assert.True(ok, pair)
	}
	for _, field := range []string{"messaging_optins", "message_reads", "message_deliveries", "messaging_postbacks", "messaging_referrals", "messaging_handovers"} {
		_, ok := reg.Lookup("page", field)
		assert.False(ok, field)
	}
}
func TestServeDMRouteRequiresRegisteredEnabledProfile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	st := testutil.NewSQLiteTestStore(t)
	cfg.FacebookMessages = []config.MetaMessagesSource{{Identifier: "example", AppID: "9", PageID: "100", Enabled: true}}
	cfg.InstagramMessages = []config.MetaMessagesSource{{Identifier: "example-ig", AppID: "9", PageID: "100", InstagramAccountID: "1784", Enabled: true}}
	for _, p := range []struct{ object, entry, kind, id string }{{"page", "100", meta.SourceTypeFacebookMessages, "app:9:page:100"}, {"instagram", "1784", meta.SourceTypeInstagramMessages, "app:9:page:100:instagram:1784"}} {
		item := metawebhook.Item{Object: p.object, EntryID: p.entry, Field: "messages", Value: []byte(`{"sender":{"id":"300"},"recipient":{"id":"` + p.entry + `"},"timestamp":1793577600123,"message":{"mid":"m_1","text":"synthetic message"}}`)}
		handler, ok := func() (metawebhook.ItemHandler, bool) {
			reg := metawebhook.NewRegistry()
			registerMetaWebhookHandlers(reg, cfg, st)
			return reg.Lookup(p.object, "messages")
		}()
		require.True(ok)
		result := handler(context.Background(), "9", []metawebhook.Item{item})
		require.Len(result, 1)
		assert.Equal(metawebhook.Unrouted, result[0].Class)
		_, err := st.GetOrCreateSource(p.kind, p.id)
		require.NoError(err)
		result = handler(t.Context(), "9", []metawebhook.Item{item})
		require.Len(result, 1)
		assert.Equal(metawebhook.Applied, result[0].Class)
		result = handler(t.Context(), "10", []metawebhook.Item{item})
		assert.Equal(metawebhook.Unrouted, result[0].Class)
	}
	cfg.FacebookMessages[0].Enabled = false
	reg := metawebhook.NewRegistry()
	registerMetaWebhookHandlers(reg, cfg, st)
	handler, ok := reg.Lookup("page", "messages")
	require.True(ok)
	result := handler(t.Context(), "9", []metawebhook.Item{{Object: "page", EntryID: "100", Field: "messages", Value: []byte(`{}`)}})
	assert.Equal(metawebhook.Unrouted, result[0].Class)
}

func TestServeDMWebhookSignedDelivery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	cfg := webhookTestConfig(t)
	cfg.FacebookMessages = []config.MetaMessagesSource{{Identifier: "example", AppID: "9", PageID: "100", Enabled: true}}
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(meta.SourceTypeFacebookMessages, "app:9:page:100")
	require.NoError(err)
	runtime, err := startMetaWebhooks(t.Context(), cfg, st, nil, nil)
	require.NoError(err)
	defer stopWebhook(t, runtime)
	body := `{"object":"page","entry":[{"id":"100","messaging":[{"sender":{"id":"300"},"recipient":{"id":"100"},"timestamp":1793577600123,"message":{"mid":"m_signed","text":"signed inbox fixture"}}]}]}`
	postWebhook(t, runtime, body)
	require.Eventually(func() bool {
		rows, err := st.MessageExistsBatch(src.ID, []string{"m_signed"})
		return err == nil && rows["m_signed"] != 0
	}, 8*time.Second, 20*time.Millisecond)
	rows, err := st.MessageExistsBatch(src.ID, []string{"m_signed"})
	require.NoError(err)
	text, err := st.GetMessageBodyText(rows["m_signed"])
	require.NoError(err)
	assert.Equal("signed inbox fixture", text)
}
