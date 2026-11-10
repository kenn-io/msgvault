package whatsapp

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestTextImportChatAssumptions(t *testing.T) {
	for _, tc := range []struct {
		name             string
		change           func(*TextOptions)
		future           bool
		input, wantError string
	}{
		{"newer-field", func(*TextOptions) {}, true, "13/6/26, 10:00 - Mira: asters\n", "upgrade"},
		{"date-order", func(o *TextOptions) { o.DateOrder = "mdy" }, false, "6/13/26, 10:00 - Mira: asters\n6/13/26, 10:01 - Mira: seedlings\n", ""},
		{"timezone", func(o *TextOptions) { o.Timezone = "Europe/Berlin" }, false, "13/6/26, 10:00 - Mira: asters\n", "assumptions conflict"},
		{"chat-type", func(o *TextOptions) { o.ChatType = "direct" }, false, "13/6/26, 10:00 - Mira: asters\n", "assumptions conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			imp := NewImporter(st, nil)
			opts := textOptions()
			_, err := imp.ImportText(t.Context(), textFile(t, "13/6/26, 10:00 - Mira: asters\n"), opts)
			require.NoError(err)
			var id int64
			var metadata string
			require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT id,metadata FROM conversations").Scan(&id, &metadata))
			var outer map[string]any
			require.NoError(json.Unmarshal([]byte(metadata), &outer))
			if tc.future {
				assumptions, ok := outer["whatsapp_txt"].(map[string]any)
				require.True(ok)
				assumptions["future_policy"] = "newer"
			}
			outer["unrelated"] = map[string]any{"kept": true}
			raw, err := json.Marshal(outer)
			require.NoError(err)
			require.NoError(st.SetConversationMetadataContext(t.Context(), id, sql.NullString{String: string(raw), Valid: true}))
			tc.change(&opts)
			summary, err := imp.ImportText(t.Context(), textFile(t, tc.input), opts)
			if tc.wantError != "" {
				require.ErrorContains(err, tc.wantError)
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT metadata FROM conversations WHERE id = ?"), id).Scan(&metadata))
				assert.Equal(string(raw), metadata)
				assert.Equal(1, textCount(t, st, "messages"))
				return
			}
			require.NoError(err)
			assert.Equal(1, summary.Added)
			assert.Equal(1, summary.Unchanged)
			assert.Equal(2, textCount(t, st, "messages"))
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT metadata FROM conversations WHERE id = ?"), id).Scan(&metadata))
			outer = nil
			require.NoError(json.Unmarshal([]byte(metadata), &outer))
			assert.Equal(map[string]any{"kept": true}, outer["unrelated"])
			assert.NotContains(outer["whatsapp_txt"], "date_order_assumption")
			for _, provenanceCase := range []struct{ body, order string }{{"asters", "dmy"}, {"seedlings", "mdy"}} {
				var messageID int64
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT id FROM messages WHERE snippet = ?"), provenanceCase.body).Scan(&messageID))
				provenance, err := st.GetMessageRawContext(t.Context(), messageID)
				require.NoError(err)
				var got map[string]any
				require.NoError(json.Unmarshal(provenance, &got))
				assert.Equal(provenanceCase.order, got["date_order_assumption"])
			}
		})
	}
}
