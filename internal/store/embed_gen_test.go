package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// embedScopeFixture builds two sources with three live messages each (plus
// one soft-deleted message in the first source) so scoped scan/coverage
// tests can prove the source filter neither leaks across accounts nor
// counts dead rows.
type embedScopeFixture struct {
	srcA     *store.Source
	srcB     *store.Source
	msgsA    []int64
	msgsB    []int64
	deletedA int64
}

func seedEmbedScopeFixture(t *testing.T, st *store.Store) embedScopeFixture {
	t.Helper()
	var fx embedScopeFixture
	var err error
	fx.srcA, err = st.GetOrCreateSource("gmail", "alice@example.com")
	require.NoError(t, err, "GetOrCreateSource alice")
	fx.srcB, err = st.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(t, err, "GetOrCreateSource bob")

	seed := func(src *store.Source, smidPrefix string, n int) []int64 {
		convID, err := st.EnsureConversationWithType(src.ID, "conv-"+src.Identifier, "email_thread", "Subject")
		require.NoError(t, err, "EnsureConversationWithType")
		ids := make([]int64, 0, n)
		for i := range n {
			id, err := st.UpsertMessage(&store.Message{
				SourceID:        src.ID,
				SourceMessageID: fmt.Sprintf("%s-%d", smidPrefix, i),
				ConversationID:  convID,
				MessageType:     "email",
				Subject:         sql.NullString{String: "s", Valid: true},
			})
			require.NoError(t, err, "UpsertMessage")
			ids = append(ids, id)
		}
		return ids
	}
	fx.msgsA = seed(fx.srcA, "alice", 3)
	fx.msgsB = seed(fx.srcB, "bob", 3)

	// One soft-deleted message in source A: live predicates must exclude it.
	fx.deletedA = seed(fx.srcA, "alice-deleted", 1)[0]
	_, err = st.DB().Exec(st.Rebind(
		`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`), fx.deletedA)
	require.NoError(t, err, "soft-delete message")
	return fx
}

func TestScanForEmbeddingScopedFiltersBySource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := seedEmbedScopeFixture(t, st)
	ctx := context.Background()

	scoped, err := st.ScanForEmbeddingScoped(ctx, 1, 0, 100, nil, []int64{fx.srcA.ID})
	require.NoError(err, "ScanForEmbeddingScoped")
	assert.Equal(fx.msgsA, scoped,
		"only source A's live messages need embedding for gen 1")

	both, err := st.ScanForEmbeddingScoped(ctx, 1, 0, 100, nil, []int64{fx.srcA.ID, fx.srcB.ID})
	require.NoError(err, "ScanForEmbeddingScoped two sources")
	assert.Equal(append(append([]int64(nil), fx.msgsA...), fx.msgsB...), both)

	unscoped, err := st.ScanForEmbedding(ctx, 1, 0, 100)
	require.NoError(err, "ScanForEmbedding")
	assert.Len(unscoped, 6, "unscoped scan covers both sources")

	none, err := st.ScanForEmbeddingScoped(ctx, 1, 0, 100, nil, []int64{9999})
	require.NoError(err, "ScanForEmbeddingScoped unknown source")
	assert.Empty(none, "a source with no messages scans empty")
}

func TestCoverageCountsScopedFiltersBySource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	fx := seedEmbedScopeFixture(t, st)
	ctx := context.Background()

	require.NoError(st.SetEmbedGen(ctx, fx.msgsA[:2], 7), "stamp two of A's messages")

	live, stamped, _, missing, err := st.CoverageCountsScoped(ctx, 7, nil, []int64{fx.srcA.ID})
	require.NoError(err, "CoverageCountsScoped")
	assert.Equal(int64(3), live, "live within source A (deleted row excluded)")
	assert.Equal(int64(2), stamped, "stamped within source A")
	assert.Equal(int64(1), missing, "missing within source A")

	missingB, err := st.MissingCountScoped(ctx, 7, nil, []int64{fx.srcB.ID})
	require.NoError(err, "MissingCountScoped")
	assert.Equal(int64(3), missingB, "all of source B's messages are missing for gen 7")
}

func TestScopedFiltersComposeMessageTypesAndSources(t *testing.T) {
	st := testutil.NewTestStore(t)
	fx := seedEmbedScopeFixture(t, st)
	ctx := context.Background()

	// Reclassify one of A's messages as a different type.
	_, err := st.DB().Exec(st.Rebind(
		`UPDATE messages SET message_type = 'sms' WHERE id = ?`), fx.msgsA[0])
	require.NoError(t, err, "reclassify message type")

	emailA, err := st.ScanForEmbeddingScoped(ctx, 1, 0, 100, []string{"email"}, []int64{fx.srcA.ID})
	require.NoError(t, err, "ScanForEmbeddingScoped type+source")
	assert.Equal(t, fx.msgsA[1:], emailA,
		"the sms-typed message drops out of the email×A intersection")
}
