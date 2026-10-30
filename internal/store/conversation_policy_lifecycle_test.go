package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicySourceRemovalInvalidatesWork(t *testing.T) {
	for _, serialized := range []bool{false, true} {
		t.Run(strconv.FormatBool(serialized), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s := testutil.NewTestStore(t)
			src, err := s.GetOrCreateSource("beeper", "removed@example.test")
			requirements.NoError(err)
			_, err = s.EnsureConversation(src.ID, "!removed:example.test", "Removed")
			requirements.NoError(err)
			requirements.NoError(s.SetConversationPolicyOverride(t.Context(), src.ID, "!removed:example.test", conversationpolicy.Full))
			before, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			s.DB().SetMaxOpenConns(1)
			if serialized {
				_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
			} else {
				err = s.RemoveSource(src.ID)
			}
			requirements.NoError(err)
			after, err := s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			assertions.Greater(after, before)
			_, pending, err := s.ConversationPolicyCleanupPending(t.Context())
			requirements.NoError(err)
			assertions.True(pending, "removed rooms leave derived data to clean up")
		})
	}
}

func TestConversationPolicySerializedRemovalWaitsForPublication(t *testing.T) {
	requirements := require.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "locked@example.test")
	requirements.NoError(err)
	var release func()
	if s.IsPostgreSQL() {
		tx, err := s.DB().BeginTx(t.Context(), nil)
		requirements.NoError(err)
		requirements.NoError(s.LockPostgresConversationPolicyTx(t.Context(), tx, store.ConversationPolicyShared))
		release = func() { _ = tx.Rollback() }
	} else {
		var seq int
		var name, path string
		requirements.NoError(s.DB().QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &path))
		other := policyFenceStore(t, path)
		release, err = other.AcquireSQLiteConversationPolicyFence(t.Context(), store.ConversationPolicyShared)
		requirements.NoError(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond) //nolint:kennlint // deadlinetest: the deadline is the expected result; the held policy read fence blocks serialized source removal
	defer cancel()
	_, _, err = s.RemoveSourceSerialized(ctx, src.ID)
	requirements.Error(err)
	release()
	_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
	requirements.NoError(err)
}

func TestConversationPolicySerializedRemovalRollsBackInvalidationFailure(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource("beeper", "rollback@example.test")
	requirements.NoError(err)
	_, err = s.EnsureConversation(src.ID, "!rollback:example.test", "Rollback")
	requirements.NoError(err)
	before, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec(`CREATE FUNCTION fail_policy_cleanup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.key='conversation_policy_cleanup_revision' THEN RAISE EXCEPTION 'synthetic cleanup failure'; END IF; RETURN NEW; END $$`)
		requirements.NoError(err)
		_, err = s.DB().Exec(`CREATE TRIGGER fail_policy_cleanup BEFORE INSERT OR UPDATE ON archive_metadata FOR EACH ROW EXECUTE FUNCTION fail_policy_cleanup()`)
	} else {
		_, err = s.DB().Exec(`CREATE TRIGGER fail_policy_cleanup BEFORE INSERT ON archive_metadata WHEN NEW.key='conversation_policy_cleanup_revision' BEGIN SELECT RAISE(ABORT,'synthetic cleanup failure'); END`)
	}
	requirements.NoError(err)
	s.DB().SetMaxOpenConns(1)
	_, _, err = s.RemoveSourceSerialized(t.Context(), src.ID)
	requirements.ErrorContains(err, "synthetic cleanup failure")
	var id int64
	requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT id FROM sources WHERE id=?`), src.ID).Scan(&id))
	assertions.Equal(src.ID, id)
	after, err := s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Equal(before, after)
}
