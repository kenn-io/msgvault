package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func removeTikTokMediaFixtureSource(ctx context.Context, st *store.Store, id int64, serialized bool) error {
	if serialized {
		_, _, err := st.RemoveSourceSerialized(ctx, id)
		return err
	}
	return st.RemoveSource(id)
}

func TestSourceRemovalClearsOnlyItsTikTokMediaSweep(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		for _, identifier := range []string{"account-example", "replacement-example"} {
			t.Run(fmt.Sprintf("serialized=%t/recreate=%s", serialized, identifier), func(t *testing.T) {
				st := testutil.NewSQLiteTestStore(t)
				other, err := st.GetOrCreateSource("tiktok_messages", "other-example")
				require.NoError(t, err)
				src, err := st.GetOrCreateSource("tiktok_messages", "account-example")
				require.NoError(t, err)
				require.Greater(t, src.ID, other.ID, "remove the highest ID so SQLite reuses it")
				state := store.TikTokMediaSweep{Version: 1, Source: src.Identifier, After: 10, Through: 20}
				require.NoError(t, st.SaveTikTokMediaSweepContext(t.Context(), src.ID, state))
				key := fmt.Sprintf("tiktok.media_sweep:%d", src.ID)
				retained := []string{
					fmt.Sprintf("tiktok.media_sweep:%d", other.ID),
					key + "0",
					key + ":extra",
					"unrelated-metadata",
				}
				for _, retainedKey := range retained {
					_, err := st.DB().ExecContext(t.Context(), `INSERT INTO archive_metadata (key, value) VALUES (?, ?)`, retainedKey, "keep")
					require.NoError(t, err)
				}
				repairKey := "rederive:tiktok_messages:account-example:fixture-v1#p"
				require.NoError(t, st.MarkMigrationApplied(repairKey))
				require.NoError(t, st.MarkMigrationApplied("unrelated-migration"))
				require.NoError(t, removeTikTokMediaFixtureSource(t.Context(), st, src.ID, serialized))
				_, err = st.GetSourceByID(src.ID)
				require.ErrorIs(t, err, store.ErrSourceNotFound)
				var value string
				err = st.DB().QueryRowContext(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, key).Scan(&value)
				assert.ErrorIs(t, err, sql.ErrNoRows)
				for _, retainedKey := range retained {
					require.NoError(t, st.DB().QueryRowContext(t.Context(), `SELECT value FROM archive_metadata WHERE key = ?`, retainedKey).Scan(&value))
					assert.Equal(t, "keep", value, retainedKey)
				}
				applied, err := st.IsMigrationApplied(repairKey)
				require.NoError(t, err)
				assert.False(t, applied, "existing repair-ledger cleanup still runs")
				applied, err = st.IsMigrationApplied("unrelated-migration")
				require.NoError(t, err)
				assert.True(t, applied)
				replacement, err := st.GetOrCreateSource("tiktok_messages", identifier)
				require.NoError(t, err)
				require.Equal(t, src.ID, replacement.ID)
				got, err := st.LoadTikTokMediaSweepContext(t.Context(), replacement.ID, replacement.Identifier)
				require.NoError(t, err, "a different identifier must not inherit invalid progress")
				assert.Equal(t, store.TikTokMediaSweep{Version: 1, Source: identifier}, got, "the same identifier must not resume an old sweep")
			})
		}
	}
}

func TestSourceRemovalRollsBackTikTokMediaSweepCleanup(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		for _, table := range []string{"sources", "archive_metadata"} {
			t.Run(fmt.Sprintf("serialized=%t/failure=%s", serialized, table), func(t *testing.T) {
				st := testutil.NewSQLiteTestStore(t)
				src, err := st.GetOrCreateSource("tiktok_messages", "account-example")
				require.NoError(t, err)
				state := store.TikTokMediaSweep{Version: 1, Source: src.Identifier, After: 10, Through: 20}
				require.NoError(t, st.SaveTikTokMediaSweepContext(t.Context(), src.ID, state))
				repairKey := "rederive:tiktok_messages:account-example:fixture-v1#p"
				require.NoError(t, st.MarkMigrationApplied(repairKey))
				_, err = st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER fail_removal BEFORE DELETE ON %s BEGIN SELECT RAISE(ABORT, 'removal fixture failure'); END`, table))
				require.NoError(t, err)
				require.ErrorContains(t, removeTikTokMediaFixtureSource(t.Context(), st, src.ID, serialized), "removal fixture failure")
				_, err = st.GetSourceByID(src.ID)
				require.NoError(t, err)
				got, err := st.LoadTikTokMediaSweepContext(t.Context(), src.ID, src.Identifier)
				require.NoError(t, err)
				assert.Equal(t, state, got)
				applied, err := st.IsMigrationApplied(repairKey)
				require.NoError(t, err)
				assert.True(t, applied, "repair cleanup rolls back with the failed removal")
			})
		}
	}
}
