package daemonclient_test

import (
	"database/sql"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplyRelationThroughDaemon(t *testing.T) {
	for _, withEngine := range []bool{false, true} {
		t.Run(map[bool]string{false: "store", true: "engine"}[withEngine], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			opts := api.ServerOptions{Config: &config.Config{}, Store: st, Logger: slog.New(slog.DiscardHandler)}
			if withEngine {
				engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
				t.Cleanup(func() { assert.NoError(engine.Close()) })
				opts.Engine = engine
			}
			server := httptest.NewServer(api.NewServerWithOptions(opts).Router())
			t.Cleanup(server.Close)
			client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
			require.NoError(err)
			t.Cleanup(func() { assert.NoError(client.Close()) })
			src, err := st.GetOrCreateSource("slack", "synthetic-workspace")
			require.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "thread", "Synthetic thread")
			require.NoError(err)
			create := func(source int64, sourceID string) int64 {
				id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source, ConversationID: conv, SourceMessageID: sourceID, MessageType: "slack"}, BodyText: sql.NullString{String: "> Literal chat text", Valid: true}})
				require.NoError(err)
				return id
			}
			parent := create(src.ID, "parent")
			child := create(src.ID, "child")
			require.NoError(st.SetReplyTo(src.ID, "child", "parent"))
			adapter := daemonclient.NewEngineAdapter(client)
			got, err := adapter.GetMessage(t.Context(), child)
			require.NoError(err)
			require.NotNil(got)
			require.NotNil(got.ReplyToMessageID)
			assert.Equal(parent, *got.ReplyToMessageID)
			assert.Equal("> Literal chat text", got.BodyText)
			otherSrc, err := st.GetOrCreateSource("slack", "other-workspace")
			require.NoError(err)
			// The database FK alone permits a cross-source or self reference. Readers
			// must suppress it even if a damaged import wrote one.
			other := create(otherSrc.ID, "outside-parent")
			for _, target := range []int64{other, child} {
				_, err = st.DB().ExecContext(t.Context(), st.Rebind("UPDATE messages SET reply_to_message_id = ? WHERE id = ?"), target, child)
				require.NoError(err)
				got, err = adapter.GetMessage(t.Context(), child)
				require.NoError(err)
				require.NotNil(got)
				assert.Nil(got.ReplyToMessageID)
			}
		})
	}
}
