package query_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSearchRowsHiddenTextPresent(t *testing.T) {
	f := storetest.New(t)
	for _, tc := range []struct {
		name, html string
		hidden     bool
	}{
		{"hidden", `<p>Visible</p><div hidden>hiddenneedle</div>`, true},
		{"visible", `<p>hiddenneedle</p>`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			id := f.CreateMessage(tc.name)
			require.NoError(f.Store.UpsertMessageBody(id,
				sql.NullString{String: "hiddenneedle", Valid: true},
				sql.NullString{String: tc.html, Valid: true}))
			_, err := f.Store.BackfillFTS(nil)
			require.NoError(err)
			engine := query.NewEngine(f.Store.DB(), f.Store.IsPostgreSQL())
			q := &search.Query{TextTerms: []string{"hiddenneedle"}, ConversationIDs: []int64{f.ConvID}}
			rows, err := engine.Search(t.Context(), q, 50, 0)
			require.NoError(err)
			bodySearcher, ok := engine.(query.MessageBodySearcher)
			require.True(ok)
			bodyRows, err := bodySearcher.SearchMessageBodies(t.Context(), q, 50, 0)
			require.NoError(err)
			for _, results := range [][]query.MessageSummary{rows, bodyRows.Messages} {
				var found bool
				for _, row := range results {
					if row.ID != id {
						continue
					}
					found = true
					data, err := json.Marshal(row)
					require.NoError(err)
					var result struct {
						HiddenTextPresent *bool `json:"hidden_text_present"`
					}
					require.NoError(json.Unmarshal(data, &result))
					require.NotNil(result.HiddenTextPresent, "search rows always emit the stored flag")
					assert.Equal(tc.hidden, *result.HiddenTextPresent)
				}
				assert.True(found, "search must return the body hit")
			}
		})
	}
}

func TestSQLiteTextHiddenTextRows(t *testing.T) {
	testutil.SkipIfPostgres(t, "SQLite timeline snapshots and FTS5 text search")
	f := storetest.New(t)
	id := f.CreateMessage("hidden")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET message_type = 'teams', snippet = 'hiddenneedle' WHERE id = ?`), id)
	require.NoError(t, err)
	require.NoError(t, f.Store.UpsertMessageBody(id, sql.NullString{String: "hiddenneedle", Valid: true}, sql.NullString{String: `<div hidden>hiddenneedle</div>`, Valid: true}))
	_, err = f.Store.BackfillFTS(nil)
	require.NoError(t, err)
	engine := query.NewSQLiteEngine(f.Store.DB())
	for _, tc := range []struct {
		name string
		read func(context.Context) ([]query.MessageSummary, error)
	}{
		{"timeline", func(ctx context.Context) ([]query.MessageSummary, error) {
			return engine.ListConversationMessages(ctx, f.ConvID, query.TextFilter{})
		}},
		{"snapshot", func(ctx context.Context) ([]query.MessageSummary, error) {
			rows, _, err := engine.ListConversationMessagesSnapshot(ctx, f.ConvID, query.TextFilter{})
			return rows, err
		}},
		{"text search", func(ctx context.Context) ([]query.MessageSummary, error) {
			return engine.TextSearch(ctx, "hiddenneedle", nil, 10, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := tc.read(t.Context())
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.True(t, rows[0].HiddenTextPresent)
		})
	}
}
