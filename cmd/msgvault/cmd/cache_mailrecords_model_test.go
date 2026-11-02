package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMailRecordsCacheLocalAutoKinds(t *testing.T) {
	for _, mode := range []string{"platform", "csv"} {
		t.Run(mode, func(t *testing.T) {
			force := ""
			if mode == "csv" {
				force = "1"
			}
			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", force)
			root := t.TempDir()
			dbPath, analytics := filepath.Join(root, "archive.db"), filepath.Join(root, "analytics")
			st := testutil.NewSQLiteTestStoreAt(t, dbPath)
			type label struct {
				name, kind string
				provider   bool
			}
			insert := func(key, text, html string, labels ...label) int64 {
				t.Helper()
				source, id, err := testutil.CreateIndexedSourceMessage(st, "kinds@example.test", key, "Purchase notice", text)
				require.NoError(t, err)
				_, err = st.DB().Exec("UPDATE messages SET sent_at = ? WHERE id = ?", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), id)
				require.NoError(t, err)
				require.NoError(t, st.UpsertMessageBody(id, sql.NullString{String: text, Valid: true}, sql.NullString{String: html, Valid: html != ""}))
				ids := []int64{}
				for _, item := range labels {
					var sourceID any
					if item.provider {
						sourceID = source.ID
					}
					var labelID int64
					require.NoError(t, st.DB().QueryRow(`INSERT INTO labels (source_id,name,label_type) VALUES (?, ?, ?) RETURNING id`, sourceID, item.name, item.kind).Scan(&labelID))
					ids = append(ids, labelID)
				}
				require.NoError(t, st.ReplaceMessageLabels(id, ids))
				return id
			}
			local := label{name: "mv:kind/receipt", kind: "auto"}
			first := insert("model", "Paid for purchase", "", local)
			later := insert("later", "Paid for another purchase", "")
			deterministic := insert("deterministic", "Order B-42", `<script type="application/ld+json">{"@context":"https://schema.org","@type":"Order","orderNumber":"B-42"}</script>`, local)
			insert("user", "Paid for purchase", "", label{name: "mv:kind/receipt", kind: "user"})
			insert("provider", "Paid for purchase", "", label{name: "mv:kind/receipt", kind: "auto", provider: true})
			insert("conflicting", "Paid for purchase", "", local, label{name: "mv:kind/invoice", kind: "auto"})
			insert("credential", "Your sign-in verification code is 654321", "", local)
			_, err := buildCache(dbPath, analytics, true)
			require.NoError(t, err)
			read := func() *query.QueryResult {
				t.Helper()
				engine, err := query.NewArchiveDuckDBEngine(t.Context(), analytics)
				require.NoError(t, err)
				defer func() { require.NoError(t, engine.Close()) }()
				result, err := engine.QuerySQL(t.Context(), "SELECT message_id, kind, basis, order_number, amount, json_extract_string(extra, '$.kind_trust') FROM message_records ORDER BY message_id")
				require.NoError(t, err)
				return result
			}
			result := read()
			require.Len(t, result.Rows, 2)
			assert.Equal(t, []any{first, "receipt", "model_kind", nil, nil, "model_judgment"}, result.Rows[0])
			assert.Equal(t, deterministic, result.Rows[1][0])
			assert.Equal(t, "order", result.Rows[1][1])
			assert.Equal(t, "jsonld", result.Rows[1][2])
			assert.Equal(t, "B-42", result.Rows[1][3])
			assert.False(t, cacheNeedsBuild(dbPath, analytics).NeedsBuild)
			var labelID int64
			require.NoError(t, st.DB().QueryRow(`SELECT id FROM labels WHERE source_id IS NULL AND label_type = 'auto' AND name = 'mv:kind/receipt' ORDER BY id LIMIT 1`).Scan(&labelID))
			require.NoError(t, st.ReplaceMessageLabels(later, []int64{labelID}))
			assert.True(t, cacheNeedsBuild(dbPath, analytics).HasRelatedRowDrift)
			_, err = buildCacheAuto(dbPath, analytics)
			require.NoError(t, err)
			assert.Len(t, read().Rows, 3)
			// An unmarked body's change can make an old model kind ineligible.
			// It must invalidate the projection even without any JSON-LD marker.
			require.NoError(t, st.UpsertMessageBody(first, sql.NullString{String: "Your sign-in verification code is 765432", Valid: true}, sql.NullString{}))
			assert.True(t, cacheNeedsBuild(dbPath, analytics).FullRebuild)
			_, err = buildCacheAuto(dbPath, analytics)
			require.NoError(t, err)
			result = read()
			require.Len(t, result.Rows, 2)
			assert.Equal(t, later, result.Rows[0][0])
			assert.Equal(t, deterministic, result.Rows[1][0])
			require.NoError(t, st.RemoveMessageLabels(later, []int64{labelID}))
			assert.True(t, cacheNeedsBuild(dbPath, analytics).HasRelatedRowDrift)
			_, err = buildCacheAuto(dbPath, analytics)
			require.NoError(t, err)
			result = read()
			require.Len(t, result.Rows, 1)
			assert.Equal(t, deterministic, result.Rows[0][0])
		})
	}
}
