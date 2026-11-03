package meta

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestFacebookPublicReplyWithoutAuthorRetainsOwnership(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, src, id, actions, graph := actionFixture(t)
	graph.response = `{"id":"100_200_301","message":"Example reply","created_time":"2026-11-01T12:00:00+0000","parent":{"id":"100_200_300"}}`
	got, err := replyRun(t, actions, id, ActionPublicReply, "Example reply")
	require.NoError(err)
	require.True(got.Receipt.ArchiveComplete)
	replyID := got.Receipt.ReplyMessageID
	assert.True(message(t, st, src.ID, "100_200_301").IsFromMe, "a succeeded receipt proves immediate archival is ours")
	fixture, client := newGraphFixture(t)
	fixture.comments["100_200"] = []Object{*comment("100_200_300", "100_200", "Incoming comment")}
	reply := comment("100_200_301", "100_200_300", "Example reply")
	reply.From = nil
	fixture.comments["100_200_300"] = []Object{*reply}
	// A poll repairs older ownership and does not depend on a cached author.
	_, err = st.DB().Exec(`UPDATE messages SET is_from_me=FALSE,source_is_from_me=FALSE WHERE id=?`, replyID)
	require.NoError(err)
	_, err = NewImporter(st, client).Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.True(message(t, st, src.ID, "100_200_301").IsFromMe)
	candidates, err := st.LoadActivityCandidatesByIDContext(t.Context(), []int64{replyID})
	require.NoError(err)
	require.Len(candidates, 1)
	assert.Equal(store.DirectionOutbound, store.ClassifyActivityCandidate(candidates[0], store.DefaultMaxDirectActivityCounterparts).Direction)
	_, err = actions.Run(t.Context(), ActionRequest{MessageID: replyID, Action: ActionPrivateReply, Body: "Private reply", DryRun: true})
	_ = actionCode(t, err, "private_reply_not_allowed")
	assert.Equal(1, graph.count())
}
