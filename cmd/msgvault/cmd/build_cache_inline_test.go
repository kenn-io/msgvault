package cmd

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inline"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type inlineCacheBackfillClient struct {
	inlineProbeClient

	media inline.Media
}

func (c *inlineCacheBackfillClient) Files(context.Context, int64, []int64) ([]inline.Media, error) {
	return []inline.Media{c.media}, nil
}

func TestBuildCache_RefreshesInlineBackfillSize(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "msgvault.db")
	analyticsDir := filepath.Join(t.TempDir(), "analytics")
	st, err := store.Open(dbPath)
	must.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	must.NoError(st.InitSchema())
	account := inline.Account{UserID: 42, Origin: inline.ProductionOrigin}
	_, err = st.GetOrCreateSource(inline.SourceType, account.Identifier())
	must.NoError(err)
	media := inline.Media{ID: "document:1", ChatID: 7, MessageID: 1, Role: "document", Size: 3, URL: "https://api.inline.chat/file"}
	const body = "deferred document"
	client := &inlineCacheBackfillClient{
		account: account,
		page: inline.Page{Messages: []inline.Message{{
			ID: 1, ChatID: 7, SenderID: 43, Text: body,
			SentAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Media: []inline.Media{media},
			Raw: jsontext.Value(`{"id":1,"chat_id":7,"text":"deferred document"}`), RawFormat: inline.RawMCPFormat,
		}}},
		media: media,
	}
	imp := inline.NewImporter(st, client)
	opts := inline.ImportOptions{Account: account, ChatIDs: []int64{7}, AttachmentsDir: t.TempDir(), NoMedia: true}
	imported, err := imp.Import(t.Context(), opts)
	must.NoError(err)
	must.Equal(1, imported.AttachmentsPending)
	_, err = buildCacheAuto(dbPath, analyticsDir)
	must.NoError(err)
	duckDB, err := sql.Open("duckdb", "")
	must.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	messagePattern := filepath.Join(analyticsDir, "messages", "**", "*.parquet")
	var size int64
	must.NoError(duckDB.QueryRow(`SELECT size_estimate FROM read_parquet(?, hive_partitioning=true)`, messagePattern).Scan(&size))
	must.Equal(int64(len(body))+media.Size, size)

	// Only the external provider/download are synthetic; persistence and normal
	// cache publication run through their production paths.
	const content = "actual archived document bytes"
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = testTransport(func(request *http.Request) (*http.Response, error) {
		checks.Equal("https://api.inline.chat/file", request.URL.String())
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(content)), ContentLength: int64(len(content)), Request: request}, nil
	})
	backfill, err := imp.BackfillMedia(t.Context(), opts)
	must.NoError(err)
	must.Equal(1, backfill.AttachmentsDownloaded)
	_, err = buildCacheAuto(dbPath, analyticsDir)
	must.NoError(err)
	must.NoError(duckDB.QueryRow(`SELECT size_estimate FROM read_parquet(?, hive_partitioning=true)`, messagePattern).Scan(&size))
	wantSize := int64(len(body) + len(content))
	checks.Equal(wantSize, size, "normal refresh must replace the previously cached provider size")
	engine, err := query.NewDuckDBEngine(analyticsDir, "", nil)
	must.NoError(err)
	t.Cleanup(func() { _ = engine.Close() })
	stats, err := engine.GetTotalStats(t.Context(), query.StatsOptions{SearchQuery: "message_type:inline larger:40", SearchScope: true})
	must.NoError(err)
	checks.Equal(int64(1), stats.MessageCount, "cached size filters must use actual backfilled bytes")
	checks.Equal(wantSize, stats.TotalSize)
	state, err := query.ReadCacheSyncState(analyticsDir)
	must.NoError(err)
	checks.Equal(int64(1), state.LastCacheUpdateCount)
	checks.False(cacheNeedsBuild(dbPath, analyticsDir).NeedsBuild, "the refreshed cache must converge")
}
