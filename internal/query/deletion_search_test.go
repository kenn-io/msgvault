package query

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestBoundedDeletionSearchCountsOnlyExecutableSources(t *testing.T) {
	env := newTestEnv(t)
	chatSourceID := env.AddSource(dbtest.SourceOpts{Type: "discord", Identifier: "chat@example.com"})
	chatConversationID := env.AddConversation(dbtest.ConversationOpts{SourceID: chatSourceID})
	for range 3 {
		env.AddMessage(dbtest.MessageOpts{
			Subject: "boundedsource", SentAt: "2024-06-02 12:00:00",
			SourceID: chatSourceID, ConversationID: chatConversationID, MessageType: "discord",
		})
	}
	mailID := env.AddMessage(dbtest.MessageOpts{Subject: "boundedsource", SentAt: "2024-06-01 12:00:00"})
	env.EnableFTS()

	for _, mode := range []DeletionSearchMode{DeletionSearchFast, DeletionSearchDeep} {
		t.Run(string(mode), func(t *testing.T) {
			t.Run("chat matches skipped before limit", func(t *testing.T) {
				assert, require := assert.New(t), require.New(t)
				targets, matched, err := env.Engine.GetDeletionTargetsBySearchBounded(
					env.Ctx, search.Parse("subject:boundedsource"), MessageFilter{}, mode, 2,
				)
				require.NoError(err)
				assert.Equal(1, matched, "chat matches must not consume the deletion search limit")
				require.Len(targets, 1, "the older Gmail match must remain eligible below the limit")
				assert.Equal(mailID, targets[0].MessageID)
			})
			t.Run("source-deleted row skipped before limit", func(t *testing.T) {
				env := newTestEnv(t)
				deletedID := env.AddMessage(dbtest.MessageOpts{Subject: "boundedneedle", SentAt: "2024-06-02 12:00:00"})
				activeID := env.AddMessage(dbtest.MessageOpts{Subject: "boundedneedle", SentAt: "2024-06-01 12:00:00"})
				env.MarkDeletedByID(deletedID)
				env.EnableFTS()
				targets, matched, err := env.Engine.GetDeletionTargetsBySearchBounded(
					env.Ctx, search.Parse("boundedneedle"), MessageFilter{}, mode, 1,
				)
				require.NoError(t, err)
				assert.Equal(t, 1, matched)
				require.Len(t, targets, 1)
				assert.Equal(t, activeID, targets[0].MessageID)
			})
			t.Run("IMAP targets", func(t *testing.T) {
				env := newTestEnv(t)
				_, err := env.DB.Exec(`UPDATE sources SET source_type = 'imap' WHERE id = 1`)
				require.NoError(t, err)
				for i := range 3 {
					id := env.AddMessage(dbtest.MessageOpts{Subject: "imapneedle"})
					_, err := env.DB.Exec(`UPDATE messages SET source_message_id = ? WHERE id = ?`, fmt.Sprintf("INBOX|%d", i+1), id)
					require.NoError(t, err)
				}
				env.EnableFTS()
				targets, matched, err := env.Engine.GetDeletionTargetsBySearchBounded(
					env.Ctx, search.Parse("subject:imapneedle"), MessageFilter{}, mode, 3,
				)
				require.NoError(t, err)
				assert.Equal(t, 3, matched)
				ids, err := deletionTargetSourceMessageIDs(targets, nil)
				require.NoError(t, err)
				assert.ElementsMatch(t, []string{"INBOX|1", "INBOX|2", "INBOX|3"}, ids)
				for _, target := range targets {
					assert.Equal(t, "imap", target.SourceType)
				}
			})
			t.Run("explicit limit", func(t *testing.T) {
				env := newTestEnv(t)
				env.EnableFTS()
				parsed := search.Parse("Hello")
				filter := MessageFilter{Sender: "alice@example.com"}
				visible, err := env.Engine.SearchFast(env.Ctx, parsed, filter, 1, 0)
				require.NoError(t, err)
				require.Len(t, visible, 1)
				targets, matched, err := env.Engine.GetDeletionTargetsBySearchBounded(env.Ctx, parsed, filter, mode, 1)
				require.NoError(t, err)
				assert.Equal(t, 1, matched)
				require.Len(t, targets, 1)
				assert.Equal(t, visible[0].SourceMessageID, targets[0].SourceMessageID)
			})
		})
	}
	t.Run("limit above maximum", func(t *testing.T) {
		_, _, err := env.Engine.GetDeletionTargetsBySearchBounded(
			env.Ctx, search.Parse("Hello"), MessageFilter{}, DeletionSearchFast, MaxDeletionTargetSearchResults+1,
		)
		require.ErrorContains(t, err, "bounded deletion search limit cannot exceed")
	})
}
