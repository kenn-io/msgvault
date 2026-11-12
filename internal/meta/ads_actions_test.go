package meta

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// adSyncedFixture completes an ad sync, which archives comment 1000 on the
// ad-only media 900.
func adSyncedFixture(t *testing.T) (*store.Store, *adFixture, *Importer, *Client, *Client, *store.Source) {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	f, page, ads := newAdFixture(t)
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(t, err)
	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
	require.NoError(t, err)
	require.False(t, sum.Partial)
	return st, f, imp, page, ads, src
}

// metadataFlags returns the stored metadata of the archived object id.
func metadataFlags(t *testing.T, st *store.Store, src *store.Source, id string) map[string]jsontext.Value {
	t.Helper()
	saved, err := st.GetMessageMetadataContext(t.Context(), message(t, st, src.ID, id).ID)
	require.NoError(t, err)
	var metadata map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(saved.String), &metadata))
	return metadata
}

func TestInstagramAdCommentModerate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, page, ads, src := adSyncedFixture(t)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "1000").ID)
	require.NoError(err)

	require.NoError(ModerateComment(t.Context(), st, page, target, ActionHide))
	assert.Equal(1, countRequests(f.sent(), "POST /v26.0/1000"), "the hide reaches Meta once, with the Page token")
	flags := metadataFlags(t, st, src, "1000")
	assert.JSONEq("true", string(flags["hidden"]))
	assert.JSONEq("true", string(flags["ad"]))
	assert.NotEqual("true", string(flags["organic"]))

	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
	require.NoError(err)
	require.False(sum.Partial)
	flags = metadataFlags(t, st, src, "1000")
	assert.JSONEq("true", string(flags["hidden"]), "sync keeps a hide made through msgvault")
	assert.JSONEq("true", string(flags["ad"]))
	assert.NotEqual("true", string(flags["organic"]))
}

func TestInstagramAdCommentReply(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, page, ads, src := adSyncedFixture(t)
	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "1000").ID)
	require.NoError(err)

	replyID, err := ReplyToComment(t.Context(), st, page, target, "Synthetic reply")
	require.NoError(err)
	assert.Equal("1001", replyID)
	assert.Equal(1, countRequests(f.sent(), "POST /v26.0/1000/replies"))
	media, reply := message(t, st, src.ID, "900"), message(t, st, src.ID, "1001")
	assert.Equal(media.ConversationID, reply.ConversationID)
	assert.Equal(message(t, st, src.ID, "1000").ID, reply.ReplyToMessageID.Int64)
	flags := metadataFlags(t, st, src, "1001")
	assert.JSONEq("true", string(flags["ad"]))
	assert.NotEqual("true", string(flags["organic"]), "a reply on ad-only media is not organic")

	f.replies1000 = `{"data":[{"id":"1001","text":"Synthetic reply","timestamp":"2000-01-03T03:04:05+0000","parent_id":"1000"}]}`
	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
	require.NoError(err)
	require.False(sum.Partial)
	flags = metadataFlags(t, st, src, "1001")
	assert.JSONEq("true", string(flags["ad"]))
	assert.NotEqual("true", string(flags["organic"]), "sync of the ad phase keeps the reply out of organic")
}

// TestInstagramAdCommentDeleteKeepsAdProgress deletes a comment while an ad
// sync is paused part-way through the ad phase. The prune must drop only the
// deleted comment's frame and keep the rest of the saved ad progress.
func TestInstagramAdCommentDeleteKeepsAdProgress(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	f, page, ads := newAdFixture(t)
	imp := NewImporter(st, page)
	src, err := imp.RegisterInstagram(t.Context(), "9", "100", "200")
	require.NoError(err)
	// Two verification reads, the organic media, its comments and replies (3),
	// the first creative page, media 900 and its comments: the next read would
	// be the replies of comment 1000.
	sum, err := imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{MaxRequests: 8})
	require.NoError(err)
	require.True(sum.Partial)

	saved := func() *syncState {
		run, err := st.GetLatestCheckpointedSyncContext(t.Context(), src.ID)
		require.NoError(err)
		var state syncState
		require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
		return &state
	}
	parents := func(p *phaseState) []string {
		var ids []string
		for _, frame := range p.Stack {
			ids = append(ids, frame.Parent)
		}
		return ids
	}
	before := saved()
	require.Equal("700", before.AdAccountID)
	adPhase := before.Phases[adCreativesEdge]
	require.NotNil(adPhase)
	require.Equal([]Object{{ID: "300"}}, adPhase.Posts, "unread ad media is saved as bare media IDs")
	require.Equal("next", adPhase.After)
	require.Contains(parents(adPhase), "1000", "a saved frame would read the replies of comment 1000")
	require.Equal(int32(1), f.firstCreativeReads.Load())

	target, err := ResolveCommentTarget(t.Context(), st, message(t, st, src.ID, "1000").ID)
	require.NoError(err)
	require.NoError(ModerateComment(t.Context(), st, page, target, ActionDelete))
	deleted := len(f.sent())
	assert.Equal(1, countRequests(f.sent(), "DELETE /v26.0/1000"))

	after := saved()
	assert.Equal("700", after.AdAccountID, "the prune keeps the ad account the progress was saved with")
	assert.Equal(adPhase.Posts, after.Phases[adCreativesEdge].Posts)
	assert.Equal(adPhase.After, after.Phases[adCreativesEdge].After)
	for _, p := range after.Phases {
		assert.NotContains(parents(p), "1000", "the deleted comment's frame is dropped")
	}

	sum, err = imp.ImportInstagramAds(t.Context(), "9", "100", "200", "700", ads, ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.NotContains(f.sent()[deleted:], "GET /v26.0/1000/replies")
	assert.Equal(int32(1), f.firstCreativeReads.Load(), "the resumed run does not re-read the first creative page")
	assert.JSONEq("true", string(metadataFlags(t, st, src, "300")["ad"]))
}

func countRequests(sent []string, request string) int {
	return len(slices.DeleteFunc(sent, func(s string) bool { return s != request }))
}
