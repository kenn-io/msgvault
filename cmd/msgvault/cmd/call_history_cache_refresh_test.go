package cmd

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestStoreAPIAdapterCallObservationRefreshesExistingCache(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()

	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	require.NoError(st.InitSchema())
	t.Cleanup(func() { _ = st.Close() })

	source, err := st.GetOrCreateSource(callhistory.SourceType, callhistory.Evidence{
		Provider: "zoom",
		Account:  "owner@example.test",
	}.SourceIdentifier())
	require.NoError(err)

	_, err = buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), false)
	require.NoError(err)
	var refreshCalls int
	adapter := &storeAPIAdapter{
		store: st,
		draftCacheRefresh: func(ctx context.Context, label string) error {
			refreshCalls++
			assert.Equal(callhistory.SourceType, label)
			require.NoError(ctx.Err())
			staleness := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
			if !staleness.NeedsBuild {
				return errors.New("call write did not make the analytics cache stale")
			}
			_, err := buildCache(cfg.DatabaseDSN(), cfg.AnalyticsDir(), false)
			return err
		},
	}

	observation := callhistory.Observation{
		Version:            1,
		Provider:           "zoom",
		Account:            "owner@example.test",
		Kind:               "call",
		Direction:          "incoming",
		Outcome:            "answered",
		OwnerParticipation: "joined",
		OccurredAt:         time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC),
		Precision:          "instant",
		RecordID:           "call-cache-refresh-1",
		Raw:                jsontext.Value(`{"record_id":"call-cache-refresh-1"}`),
	}
	messageID, err := adapter.PutCallObservationContext(context.Background(), source.ID, observation)
	require.NoError(err)
	require.Positive(messageID)
	require.Equal(1, refreshCalls, "successful call writes should request one cache refresh")

	staleness := cacheNeedsBuild(cfg.DatabaseDSN(), cfg.AnalyticsDir())
	assert.False(staleness.NeedsBuild, "call evidence should be published in the existing cache: %+v", staleness)
	duckDB, err := sql.Open("duckdb", "")
	require.NoError(err)
	t.Cleanup(func() { _ = duckDB.Close() })
	var cachedSourceMessageID string
	require.NoError(duckDB.QueryRow(`
		SELECT source_message_id FROM read_parquet(?, hive_partitioning=true)
		WHERE id = ?
	`, filepath.Join(cfg.AnalyticsDir(), "messages", "**", "*.parquet"), messageID).Scan(&cachedSourceMessageID))
	assert.Equal(observation.RecordID, cachedSourceMessageID)
}
