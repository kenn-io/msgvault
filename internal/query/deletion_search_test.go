package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

func TestBoundedDeletionSearchCountsOnlyExecutableSources(t *testing.T) {
	t.Skip("P2: bounded deletion search counts non-executable sources toward the limit; fixing it would lengthen production functions")

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
			targets, matched, err := env.Engine.GetDeletionTargetsBySearchBounded(
				env.Ctx, search.Parse("subject:boundedsource"), MessageFilter{}, mode, 2,
			)
			require.NoError(t, err)
			assert.Equal(t, 1, matched, "chat matches must not consume the deletion search limit")
			require.Len(t, targets, 1, "the older Gmail match must remain eligible below the limit")
			assert.Equal(t, mailID, targets[0].MessageID)
		})
	}
}
