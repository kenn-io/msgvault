package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Count actual database insert attempts, including no-op upserts. Repeated
// per-message snapshots must not issue one SQL write per unchanged member.
func TestConversationParticipantSnapshotSkipsUnchangedUpserts(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	first := f.EnsureParticipant("first@example.test", "Synthetic First", "example.test")
	second := f.EnsureParticipant("second@example.test", "Synthetic Second", "example.test")
	refs := []store.ConversationParticipantRef{{ParticipantID: first, Role: "member"}, {ParticipantID: second, Role: "member"}}
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, refs))
	_, err := f.Store.DB().Exec(`CREATE TABLE synthetic_membership_attempts (attempts INTEGER NOT NULL); INSERT INTO synthetic_membership_attempts VALUES (0)`)
	require.NoError(err)
	if f.Store.IsPostgreSQL() {
		_, err = f.Store.DB().Exec(`CREATE FUNCTION count_membership_attempt() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN UPDATE synthetic_membership_attempts SET attempts = attempts + 1; RETURN NEW; END; $$;
		CREATE TRIGGER count_membership_attempt BEFORE INSERT ON conversation_participants
		FOR EACH ROW EXECUTE FUNCTION count_membership_attempt()`)
	} else {
		_, err = f.Store.DB().Exec(`CREATE TRIGGER count_membership_attempt BEFORE INSERT ON conversation_participants
		BEGIN UPDATE synthetic_membership_attempts SET attempts = attempts + 1; END`)
	}
	require.NoError(err)
	// Ordering, duplicate entries and zero placeholders do not change the set.
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, []store.ConversationParticipantRef{refs[1], {}, refs[0], refs[1]}))
	var attempts int
	require.NoError(f.Store.DB().QueryRow(`SELECT attempts FROM synthetic_membership_attempts`).Scan(&attempts))
	assert.Zero(attempts, "unchanged snapshots must avoid per-member SQL upserts")
	refs[0].Role = "owner"
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, refs))
	var role string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT role FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), f.ConvID, first).Scan(&role))
	assert.Equal("owner", role, "the fast path must compare roles as well as IDs")
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, refs[:1]))
	var members int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), f.ConvID).Scan(&members))
	assert.Equal(1, members, "stale members must still be removed")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE conversation_participants SET role=NULL WHERE conversation_id=? AND participant_id=?`), f.ConvID, first)
	require.NoError(err)
	refs[0].Role = ""
	require.NoError(f.Store.ReplaceConversationParticipants(f.ConvID, refs[:1]))
	var nullableRole sql.NullString
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT role FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), f.ConvID, first).Scan(&nullableRole))
	assert.Equal(sql.NullString{String: "", Valid: true}, nullableRole, "NULL and empty role are distinct snapshots")
}
