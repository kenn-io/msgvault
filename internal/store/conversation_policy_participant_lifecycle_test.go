package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
)

func TestConversationPolicyParticipantMergeRefreshesSelfActivity(t *testing.T) {
	for _, replaceSelf := range []bool{false, true} {
		name := "self survives"
		if replaceSelf {
			name = "self replaced"
		}
		t.Run(name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newActivityFixture(t)
			alias, err := f.s.EnsureParticipantByIdentifier("slack", "team:alias", "Synthetic Alias")
			requirements.NoError(err)
			now := time.Now().UTC()
			_, err = f.s.UpsertMessage(&store.Message{SourceID: f.source, ConversationID: f.room, SourceMessageID: "alias-activity", MessageType: "slack", SenderID: sql.NullInt64{Int64: alias, Valid: true}, SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}})
			requirements.NoError(err)
			before, err := f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.DefaultSettings(), now)
			requirements.NoError(err)
			requirements.Equal(conversationpolicy.Cold, f.tier(t))
			var fingerprint string
			requirements.NoError(f.s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&fingerprint))
			work, err := f.s.CaptureConversationPolicyWork(t.Context())
			requirements.NoError(err)
			oldID, newID := alias, f.self
			if replaceSelf {
				oldID, newID = f.self, alias
			}
			requirements.NoError(f.s.MergeParticipantsContext(t.Context(), oldID, newID))
			assertions.Equal(conversationpolicy.Full, f.tier(t), "merge itself must refresh current policy")
			requirements.ErrorIs(store.CheckConversationPolicyAdmission(work), store.ErrConversationPolicyStale)
			var refreshedFingerprint string
			requirements.NoError(f.s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&refreshedFingerprint))
			if replaceSelf {
				assertions.NotEqual(fingerprint, refreshedFingerprint)
			} else {
				assertions.Equal(fingerprint, refreshedFingerprint)
			}
			after, err := f.s.ConversationPolicyRevision(t.Context())
			requirements.NoError(err)
			assertions.Greater(after, before.Revision)
			var cleanup int64
			requirements.NoError(f.s.DB().QueryRow(`SELECT CAST(value AS BIGINT) FROM archive_metadata WHERE key='conversation_policy_cleanup_revision'`).Scan(&cleanup))
			assertions.Equal(after, cleanup)
			steady, err := f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.DefaultSettings(), time.Now().UTC())
			requirements.NoError(err)
			assertions.Equal(after, steady.Revision, "a later configure should have no missed merge work")
			assertions.Empty(steady.ChangedConversationIDs)
		})
	}
}

func TestConversationPolicyParticipantMergeLeavesUnrelatedActivity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	a, err := f.s.EnsureParticipant("peer-a@example.test", "Peer A", "example.test")
	requirements.NoError(err)
	b, err := f.s.EnsureParticipant("peer-b@example.test", "Peer B", "example.test")
	requirements.NoError(err)
	_, err = f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.DefaultSettings(), time.Now().UTC())
	requirements.NoError(err)
	// A sentinel exposes an unnecessary reattribution of unrelated history.
	sentinel := time.Now().UTC().Add(-365 * 24 * time.Hour).Truncate(time.Second)
	_, err = f.s.DB().Exec(f.s.Rebind(`UPDATE conversation_policy SET last_activity_at=? WHERE conversation_id=?`), sentinel, f.room)
	requirements.NoError(err)
	before, err := f.s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	requirements.NoError(f.s.MergeParticipantsContext(t.Context(), a, b))
	rows, err := f.s.ListConversationPolicies(t.Context())
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.True(sentinel.Equal(rows[0].LastActivity))
	after, err := f.s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	assertions.Greater(after, before, "identity work still needs revocation")
}

func TestConversationPolicyParticipantMergeWithoutChatSupportDoesNotBootstrap(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t) // unrelated chat source and confirmed self already exist
	a, err := f.s.EnsureParticipant("unrelated-a@example.test", "Unrelated A", "example.test")
	requirements.NoError(err)
	b, err := f.s.EnsureParticipant("unrelated-b@example.test", "Unrelated B", "example.test")
	requirements.NoError(err)
	before, err := f.s.IdentityRevision()
	requirements.NoError(err)
	requirements.NoError(f.s.MergeParticipantsContext(t.Context(), a, b))
	after, err := f.s.IdentityRevision()
	requirements.NoError(err)
	assertions.Equal(before+1, after)
	var bootstrapped bool
	requirements.NoError(f.s.DB().QueryRow(`SELECT EXISTS(SELECT 1 FROM archive_metadata WHERE key='conversation_policy_people_bootstrapped')`).Scan(&bootstrapped))
	assertions.False(bootstrapped, "an unrelated contact merge does not own chat-policy bootstrap")
}

func TestConversationPolicyParticipantMergeRefreshesSelfWithoutRoomSupport(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newActivityFixture(t)
	alias, err := f.s.EnsureParticipantByIdentifier("slack", "team:self-alias", "")
	requirements.NoError(err)
	_, err = f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.DefaultSettings(), time.Now().UTC())
	requirements.NoError(err)
	var before string
	requirements.NoError(f.s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&before))
	requirements.NoError(f.s.MergeParticipantsContext(t.Context(), f.self, alias))
	var after string
	requirements.NoError(f.s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&after))
	assertions.NotEqual(before, after, "self mapping must refresh even without a message or roster edge")
	revision, err := f.s.ConversationPolicyRevision(t.Context())
	requirements.NoError(err)
	steady, err := f.s.ConfigureConversationPolicy(t.Context(), conversationpolicy.DefaultSettings(), time.Now().UTC())
	requirements.NoError(err)
	assertions.Equal(revision, steady.Revision)
}
