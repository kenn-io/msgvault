package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestConversationParticipantMergeSkipsUnchangedUpserts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	conv, err := f.Store.EnsureConversationWithType(f.Source.ID, "synthetic-roster", "group_chat", "Synthetic roster")
	require.NoError(err)
	f.ConvID = conv
	first := f.EnsureParticipant("first@example.test", "Synthetic First", "example.test")
	second := f.EnsureParticipant("second@example.test", "Synthetic Second", "example.test")
	refs := []store.ConversationParticipantRef{{ParticipantID: first, Role: "member"}, {ParticipantID: second, Role: "member"}}
	persist := func(key string, members []store.ConversationParticipantRef) {
		t.Helper()
		_, err := f.Store.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:      f.NewMessage().WithSourceMessageID(key).Build(),
			Conversation: &store.ConversationPersistData{SourceConversationID: "synthetic-roster", ConversationType: "group_chat", Participants: members, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: "Synthetic body", Valid: true},
		})
		require.NoError(err)
	}
	persist("initial", refs)
	_, err = f.Store.DB().Exec(`CREATE TABLE synthetic_merge_attempts (attempts INTEGER NOT NULL); INSERT INTO synthetic_merge_attempts VALUES (0)`)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION count_merge_attempt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN UPDATE synthetic_merge_attempts SET attempts=attempts+1; RETURN NEW; END; $$;
  CREATE TRIGGER count_merge_attempt BEFORE INSERT ON conversation_participants FOR EACH ROW EXECUTE FUNCTION count_merge_attempt()`)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER count_merge_attempt BEFORE INSERT ON conversation_participants BEGIN UPDATE synthetic_merge_attempts SET attempts=attempts+1; END`)
	}
	require.NoError(err)
	persist("unchanged", []store.ConversationParticipantRef{refs[1], {}, refs[0], refs[1]})
	var attempts int
	require.NoError(f.Store.DB().QueryRow(`SELECT attempts FROM synthetic_merge_attempts`).Scan(&attempts))
	assert.Zero(attempts, "atomic additive snapshots must skip unchanged member writes")
	refs[0].Role = "owner"
	persist("role-change", refs[:1])
	var role string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT role FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conv, first).Scan(&role))
	assert.Equal("owner", role)
	var members int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), conv).Scan(&members))
	assert.Equal(2, members, "an additive snapshot retains unmentioned historical members")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE conversation_participants SET role=NULL WHERE conversation_id=? AND participant_id=?`), conv, first)
	require.NoError(err)
	refs[0].Role = ""
	persist("empty-role", refs[:1])
	var nullableRole sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT role FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conv, first).Scan(&nullableRole))
	assert.Equal(sql.NullString{String: "", Valid: true}, nullableRole)
	third := f.EnsureParticipant("third@example.test", "Synthetic Third", "example.test")
	_, err = f.Store.DB().Exec(`UPDATE synthetic_merge_attempts SET attempts=0`)
	require.NoError(err)
	persist("new-member", []store.ConversationParticipantRef{{ParticipantID: third, Role: "member"}, refs[0], refs[1]})
	require.NoError(f.Store.DB().QueryRow(`SELECT attempts FROM synthetic_merge_attempts`).Scan(&attempts))
	assert.Equal(1, attempts, "only the new member needs an insert")
}
