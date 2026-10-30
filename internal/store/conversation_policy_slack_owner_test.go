package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestConversationPolicySlackMentionUsesExactSourceOwner(t *testing.T) {
	for _, provider := range []string{"slack", "slackdump"} {
		t.Run(provider, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			s := testutil.NewTestStore(t)
			s.DB().SetMaxOpenConns(1)
			source, err := s.GetOrCreateSource(provider, "T_OWNER:U_SELF")
			requirements.NoError(err)
			room, err := s.EnsureConversation(source.ID, "C_OWNER", "Synthetic")
			requirements.NoError(err)
			wrongWorkspace, err := s.EnsureParticipantByIdentifier("slack", "T_OTHER:U_SELF", "")
			requirements.NoError(err)
			wrongOwner, err := s.EnsureParticipantByIdentifier("slack", "T_OWNER:U_OTHER", "")
			requirements.NoError(err)
			owner, err := s.EnsureParticipantByIdentifier("slack", "T_OWNER:U_SELF", "")
			requirements.NoError(err)
			now := time.Now().UTC().Truncate(time.Second)
			message, err := s.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "owner-mention", MessageType: "slack", SentAt: sql.NullTime{Time: now, Valid: true}})
			requirements.NoError(err)
			requirements.NoError(s.ReplaceMessageRecipients(message, "mention", []int64{wrongWorkspace, wrongOwner}, []string{"", ""}))
			activity, err := s.ConversationLastActivityContext(t.Context(), room, now)
			requirements.NoError(err)
			assertions.True(activity.IsZero(), "same user in another workspace or credential user is not source owner")
			requirements.NoError(s.ReplaceMessageRecipients(message, "mention", []int64{owner}, []string{""}))
			activity, err = s.ConversationLastActivityContext(t.Context(), room, now)
			requirements.NoError(err)
			assertions.Equal(now, activity, "source owner does not require rich contact promotion")
			var email sql.NullString
			requirements.NoError(s.DB().QueryRow(s.Rebind(`SELECT email_address FROM participants WHERE id=?`), owner).Scan(&email))
			assertions.False(email.Valid)
			_, err = s.ConfigureConversationPolicy(t.Context(), conversationpolicy.RecommendedSettings(), now)
			requirements.NoError(err)
			var before, after string
			requirements.NoError(s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&before))
			requirements.NoError(s.MergeParticipantsContext(t.Context(), owner, wrongOwner))
			requirements.NoError(s.DB().QueryRow(`SELECT value FROM archive_metadata WHERE key='conversation_policy_self_attribution_fingerprint'`).Scan(&after))
			assertions.NotEqual(before, after, "self fingerprint follows the same canonical provider owner mapping")
			activity, err = s.ConversationLastActivityContext(t.Context(), room, now)
			requirements.NoError(err)
			assertions.Equal(now, activity, "merging the same identity preserves owner mention attribution")
		})
	}
}
