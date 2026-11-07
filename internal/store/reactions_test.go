package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceReactions(t *testing.T) {
	for _, scenario := range []string{"timestamps", "restore removed", "write only changes"} {
		t.Run(scenario, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			var st *store.Store
			if scenario == "write only changes" {
				st = testutil.NewSQLiteTestStore(t)
			} else {
				st = testutil.NewTestStore(t)
			}
			source, err := st.GetOrCreateSource("inline", "api.inline.chat:user:42")
			require.NoError(err)
			conversationID, err := st.EnsureConversationWithType(source.ID, "chat:123", "group_chat", "Example group")
			require.NoError(err)
			messageID := insertStoreTestMessage(t, st, source.ID, conversationID, "chat:123:message:7")
			participantID, err := st.EnsureParticipantByIdentifier("inline", "api.inline.chat:user:99", "Example participant")
			require.NoError(err)
			switch scenario {
			case "timestamps":
				known := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
				require.NoError(st.ReplaceReactions(messageID, []store.ReactionRef{
					{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: known},
					{ParticipantID: participantID, Type: "emoji", Value: "👀"},
				}))
				var gotKnown, gotUnknown sql.NullTime
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👍").Scan(&gotKnown))
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👀").Scan(&gotUnknown))
				require.True(gotKnown.Valid)
				assert.Equal(known, gotKnown.Time.UTC())
				assert.False(gotUnknown.Valid, "a source without reaction time must not fabricate one")

				updated := known.Add(24 * time.Hour)
				require.NoError(st.ReplaceReactions(messageID, []store.ReactionRef{
					{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: updated},
					{ParticipantID: participantID, Type: "emoji", Value: "👀"},
				}))
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at FROM reactions WHERE message_id = ? AND reaction_value = ?`), messageID, "👍").Scan(&gotKnown))
				require.True(gotKnown.Valid)
				assert.Equal(updated, gotKnown.Time.UTC())
			case "restore removed":
				known := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
				reactions := []store.ReactionRef{{ParticipantID: participantID, Type: "emoji", Value: "👍", CreatedAt: known}}
				require.NoError(st.ReplaceReactions(messageID, reactions))
				_, err = st.DB().Exec(st.Rebind(`UPDATE reactions SET removed_at = ? WHERE message_id = ?`), known.Add(time.Hour), messageID)
				require.NoError(err)

				// The source snapshot includes the reaction again with its original time.
				require.NoError(st.ReplaceReactions(messageID, reactions))
				var createdAt, removedAt sql.NullTime
				require.NoError(st.DB().QueryRow(st.Rebind(`SELECT created_at, removed_at FROM reactions WHERE message_id = ?`), messageID).Scan(&createdAt, &removedAt))
				require.True(createdAt.Valid)
				assert.Equal(known, createdAt.Time.UTC())
				assert.False(removedAt.Valid, "a reaction in the current snapshot is active")
			case "write only changes":
				reactions := []store.ReactionRef{
					{ParticipantID: participantID, Type: "emoji", Value: "👍"},
					{ParticipantID: participantID, Type: "emoji", Value: "👀"},
				}
				require.NoError(st.ReplaceReactions(messageID, reactions))
				_, err = st.DB().Exec(`CREATE TABLE reaction_writes (operation TEXT);
		CREATE TRIGGER record_reaction_delete AFTER DELETE ON reactions BEGIN INSERT INTO reaction_writes VALUES ('delete'); END;
		CREATE TRIGGER record_reaction_insert AFTER INSERT ON reactions BEGIN INSERT INTO reaction_writes VALUES ('insert'); END;
		CREATE TRIGGER record_reaction_update AFTER UPDATE ON reactions BEGIN INSERT INTO reaction_writes VALUES ('update'); END`)
				require.NoError(err)
				require.NoError(st.ReplaceReactions(messageID, reactions))
				var writes, inserts, deletes int
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reaction_writes`).Scan(&writes))
				assert.Zero(writes, "an unchanged snapshot must leave reaction rows untouched")
				_, err = st.DB().Exec(`DELETE FROM reaction_writes`)
				require.NoError(err)
				reactions[1].Value = "❤"
				require.NoError(st.ReplaceReactions(messageID, reactions))
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reaction_writes WHERE operation = 'insert'`).Scan(&inserts))
				require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reaction_writes WHERE operation = 'delete'`).Scan(&deletes))
				assert.Equal(1, inserts)
				assert.Equal(1, deletes)
				var values string
				require.NoError(st.DB().QueryRow(`SELECT GROUP_CONCAT(reaction_value, ',') FROM (SELECT reaction_value FROM reactions ORDER BY reaction_value)`).Scan(&values))
				assert.Equal("❤,👍", values)
			}
		})
	}
}
