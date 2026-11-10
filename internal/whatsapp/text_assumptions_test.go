package whatsapp

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextAssumptionsUnknownMemberRefused(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	path := textFile(t, "13/6/26, 10:00 - Mira: asters\n")
	_, err := imp.ImportText(t.Context(), path, opts)
	require.NoError(t, err)
	var id int64
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id,metadata FROM conversations").Scan(&id, &metadata))
	var outer map[string]any
	require.NoError(t, json.Unmarshal([]byte(metadata), &outer))
	assumptions, ok := outer["whatsapp_txt"].(map[string]any)
	require.True(t, ok)
	assumptions["future_policy"] = "newer"
	outer["unrelated"] = map[string]any{"kept": true}
	raw, err := json.Marshal(outer)
	require.NoError(t, err)
	require.NoError(t, st.SetConversationMetadataContext(t.Context(), id, sql.NullString{String: string(raw), Valid: true}))
	_, err = imp.ImportText(t.Context(), path, opts)
	require.ErrorContains(t, err, "upgrade")
	var after string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM conversations WHERE id = ?", id).Scan(&after))
	assert.Equal(t, string(raw), after)
	assert.Equal(t, 1, textCount(t, st, "messages"))
}

func TestTextAssumptionsLocaleChange(t *testing.T) {
	st := testutil.NewTestStore(t)
	imp := NewImporter(st, nil)
	opts := textOptions()
	_, err := imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: asters\n"), opts)
	require.NoError(t, err)
	var id int64
	var metadata string
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id,metadata FROM conversations").Scan(&id, &metadata))
	var outer map[string]any
	require.NoError(t, json.Unmarshal([]byte(metadata), &outer))
	outer["unrelated"] = map[string]any{"kept": true}
	raw, err := json.Marshal(outer)
	require.NoError(t, err)
	require.NoError(t, st.SetConversationMetadataContext(t.Context(), id, sql.NullString{String: string(raw), Valid: true}))
	opts.DateOrder = "mdy"
	summary, err := imp.ImportText(t.Context(), textFile(t, "6/13/26, 10:00 - Mira: asters\n6/13/26, 10:01 - Mira: seedlings\n"), opts)
	require.NoError(t, err)
	assert.Equal(t, 1, summary.Added)
	assert.Equal(t, 1, summary.Unchanged)
	assert.Equal(t, 2, textCount(t, st, "messages"))
	require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT metadata FROM conversations WHERE id = ?", id).Scan(&metadata))
	outer = nil
	require.NoError(t, json.Unmarshal([]byte(metadata), &outer))
	assert.Equal(t, map[string]any{"kept": true}, outer["unrelated"])
	assert.NotContains(t, outer["whatsapp_txt"], "date_order_assumption")
	for _, tc := range []struct{ body, order string }{{"asters", "dmy"}, {"seedlings", "mdy"}} {
		var messageID int64
		require.NoError(t, st.DB().QueryRowContext(t.Context(), "SELECT id FROM messages WHERE snippet = ?", tc.body).Scan(&messageID))
		provenance, err := st.GetMessageRawContext(t.Context(), messageID)
		require.NoError(t, err)
		var got map[string]any
		require.NoError(t, json.Unmarshal(provenance, &got))
		assert.Equal(t, tc.order, got["date_order_assumption"])
	}
}
