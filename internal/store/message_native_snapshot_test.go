package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPersistNativeLiveReplyRequiresParent(t *testing.T) {
	for _, mode := range []store.IngestMode{store.IngestLive, store.IngestBackfill} {
		t.Run(string(mode), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("slack", "synthetic-team:synthetic-owner")
			require.NoError(err)
			conversation, err := st.EnsureConversationWithType(source.ID, "CSYNTHETIC", "channel", "Synthetic channel")
			require.NoError(err)
			view := st.WithIngestContext(store.IngestContext{Mode: mode})
			_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{
				Message:  &store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "CSYNTHETIC:reply", MessageType: "slack"},
				BodyText: sql.NullString{String: "Synthetic reply", Valid: true},
				RawMIME:  []byte(`{"type":"message"}`), RawFormat: "slack_json",
				ReplyToSourceMessageID: "CSYNTHETIC:missing-parent",
			})
			want := 1
			if mode == store.IngestLive {
				require.Error(err, "a live reply cannot promise a readable parent that disappeared before persistence")
				want = 0
			} else {
				require.NoError(err, "historical orphaned replies retain existing archival behavior")
			}
			var count int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count))
			assert.Equal(want, count)
		})
	}
}
