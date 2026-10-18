package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicyTransitionDoesNotRewriteDirtyContactState(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("beeper", "contact-invalidation")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "room", "Room")
	requirements.NoError(err)
	first := mustPromotedPerson(t, s, "first@example.com", "First Contact")
	second := mustPromotedPerson(t, s, "second@example.com", "Second Contact")
	for _, id := range []int64{first.ID, second.ID} {
		_, err = s.DB().Exec(s.Rebind(`INSERT INTO person_contact_state(person_id,dirty_at) VALUES(?,NULL) ON CONFLICT(person_id) DO UPDATE SET dirty_at=NULL`), id)
		requirements.NoError(err)
	}
	// Observe real row UPDATEs, including value-identical writes. The audit
	// trigger exists only in this scratch archive, with no production seam.
	_, err = s.DB().Exec(`CREATE TABLE policy_contact_updates(person_id BIGINT NOT NULL)`)
	requirements.NoError(err)
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec(`CREATE FUNCTION audit_policy_contact_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN INSERT INTO policy_contact_updates(person_id) VALUES(NEW.person_id); RETURN NEW; END; $$;
CREATE TRIGGER audit_policy_contact_update AFTER UPDATE ON person_contact_state FOR EACH ROW EXECUTE FUNCTION audit_policy_contact_update()`)
	} else {
		_, err = s.DB().Exec(`CREATE TRIGGER audit_policy_contact_update AFTER UPDATE ON person_contact_state BEGIN INSERT INTO policy_contact_updates(person_id) VALUES(NEW.person_id); END`)
	}
	requirements.NoError(err)
	updates := func() []int64 {
		rows, err := s.DB().Query(`SELECT person_id FROM policy_contact_updates ORDER BY person_id`)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		var ids []int64
		for rows.Next() {
			var id int64
			require.NoError(t, rows.Scan(&id))
			ids = append(ids, id)
		}
		require.NoError(t, rows.Err())
		return ids
	}
	dirtyAt := func(id int64) sql.NullTime {
		var value sql.NullTime
		require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT dirty_at FROM person_contact_state WHERE person_id=?`), id).Scan(&value))
		return value
	}
	before, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Full))
	firstRevision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Greater(firstRevision, before)
	assertions.ElementsMatch([]int64{first.ID, second.ID}, updates())
	firstDirty, secondDirty := dirtyAt(first.ID), dirtyAt(second.ID)
	requirements.True(firstDirty.Valid)
	requirements.True(secondDirty.Valid)
	_, err = s.DB().Exec(`DELETE FROM policy_contact_updates`)
	requirements.NoError(err)
	requirements.NoError(s.SetConversationPolicyOverride(t.Context(), source.ID, "room", conversationpolicy.Cold))
	secondRevision, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Greater(secondRevision, firstRevision)
	assertions.Empty(updates(), "a second transition must not UPDATE already-dirty rows")
	assertions.Equal(firstDirty, dirtyAt(first.ID))
	assertions.Equal(secondDirty, dirtyAt(second.ID))
	// Missing legacy policy state still requires generic invalidation: clean
	// derived contact state must be dirtied when that policy row is initialized.
	_, err = s.DB().Exec(s.Rebind(`DELETE FROM conversation_policy WHERE conversation_id=?`), room)
	requirements.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`UPDATE person_contact_state SET dirty_at=NULL WHERE person_id=?`), first.ID)
	requirements.NoError(err)
	_, err = s.DB().Exec(`DELETE FROM policy_contact_updates`)
	requirements.NoError(err)
	derived, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	identity, err := s.IdentityRevision()
	requirements.NoError(err)
	refreshed, err := s.RefreshConversationPolicy(t.Context(), []int64{room}, time.Now())
	requirements.NoError(err)
	assertions.Equal([]int64{room}, refreshed.ChangedConversationIDs)
	assertions.Greater(refreshed.Revision, secondRevision)
	assertions.Equal([]int64{first.ID}, updates())
	assertions.True(dirtyAt(first.ID).Valid)
	assertions.Equal(secondDirty, dirtyAt(second.ID))
	afterDerived, err := s.DerivedDataRevisionContext(t.Context())
	requirements.NoError(err)
	assertions.Greater(afterDerived, derived)
	afterIdentity, err := s.IdentityRevision()
	requirements.NoError(err)
	assertions.Greater(afterIdentity, identity)
	cleanup, err := s.ConversationPolicyCleanupRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(refreshed.Revision, cleanup)
}
