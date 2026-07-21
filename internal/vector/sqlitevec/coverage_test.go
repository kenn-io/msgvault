//go:build fts5 && sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector"
)

// TestCoverageSplit_EmbeddedBlankMissing proves the full coverage readout —
// live / embedded / blank / missing — is computed from real state, not
// stubbed. It builds a generation where:
//
//   - two messages are EMBEDDED: they have an actual vector row (Upsert) and
//     are stamped embed_gen=gen (the worker's DONE mark).
//   - two messages are BLANK: stamped embed_gen=gen (terminal DONE) but with
//     NO vector — the empty/unembeddable case the blank count exists to
//     surface (body-extraction-regression detector).
//   - one message is MISSING: never stamped (embed_gen NULL).
//
// It then asserts each leg is the exact expected number, computing the split
// exactly as the CLI does:
//
//	stamped  = CoverageCounts(gen) 2nd value (embed_gen=gen, incl. blanks)
//	embedded = backend.EmbeddedMessageCount(gen)  (COUNT(DISTINCT message_id))
//	blank    = stamped - embedded
//	missing  = CoverageCounts(gen) 4th value
//
// and verifies the load-bearing invariant live == embedded + blank + missing.
func TestCoverageSplit_EmbeddedBlankMissing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	// A sqlitevec test must use a SQLite main store regardless of
	// MSGVAULT_TEST_DB: the backend's Open-time probes run SQLite-dialect SQL
	// (sqlite_master) against this handle, and in production sqlitevec is only
	// ever paired with a SQLite main store.
	st := testutil.NewSQLiteTestStore(t)

	// Open a sqlitevec backend over the SAME main DB handle. MainPath is
	// only needed for FusedSearch (ATTACH); EmbeddedMessageCount/Upsert work
	// off MainDB + vectors.db alone.
	b, err := Open(ctx, Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 8,
		MainDB:    st.DB(),
	})
	require.NoError(err, "Open backend")
	t.Cleanup(func() { _ = b.Close() })

	source, err := st.GetOrCreateSource("gmail", "me@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "email_thread", "Subject")
	require.NoError(err, "EnsureConversationWithType")

	// Create 5 live messages.
	makeMsg := func(srcMsgID string) int64 {
		m := &store.Message{
			SourceID:        source.ID,
			SourceMessageID: srcMsgID,
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: "s-" + srcMsgID, Valid: true},
		}
		id, err := st.UpsertMessage(m)
		require.NoErrorf(err, "UpsertMessage %s", srcMsgID)
		return id
	}
	embeddedA := makeMsg("emb-a")
	embeddedB := makeMsg("emb-b")
	blankA := makeMsg("blank-a")
	blankB := makeMsg("blank-b")
	missing := makeMsg("missing") // never stamped

	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err, "CreateGeneration")

	// Embedded messages: real vector rows + stamp.
	vec := func(seed float32) []float32 {
		v := make([]float32, 8)
		v[0] = seed
		return v
	}
	require.NoError(b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: embeddedA, Vector: vec(1)},
		{MessageID: embeddedB, Vector: vec(2)},
	}), "Upsert embedded vectors")
	require.NoError(st.SetEmbedGen(ctx, []int64{embeddedA, embeddedB}, int64(gen)), "stamp embedded")

	// Blank messages: stamped terminal DONE but NO vector row.
	require.NoError(st.SetEmbedGen(ctx, []int64{blankA, blankB}, int64(gen)), "stamp blank")

	// missing: left with embed_gen NULL — nothing to do.
	_ = missing

	// --- Compute the split exactly as the CLI does. ---
	live, stamped, blankFromStore, missingCount, err := st.CoverageCounts(ctx, int64(gen))
	require.NoError(err, "CoverageCounts")
	embedded, err := b.EmbeddedMessageCount(ctx, gen)
	require.NoError(err, "EmbeddedMessageCount")
	blank := max(stamped-embedded, 0)

	// CoverageCounts' 3rd return is the legacy always-0 stub; the real blank
	// is the display-layer computation. Pin both facts.
	assert.Equal(int64(0), blankFromStore, "CoverageCounts blank stays the 0 stub")

	assert.Equal(int64(5), live, "live = all 5 messages")
	assert.Equal(int64(4), stamped, "stamped = 4 (2 embedded + 2 blank)")
	assert.Equal(int64(2), embedded, "embedded = 2 (distinct message_ids with a vector)")
	assert.Equal(int64(2), blank, "blank = stamped - embedded = 2")
	assert.Equal(int64(1), missingCount, "missing = 1 (never stamped)")

	// The load-bearing invariant.
	assert.Equal(live, embedded+blank+missingCount,
		"invariant: live == embedded + blank + missing")
}

// TestCoverageSplit_NonLiveEmbeddedHoldsInvariant proves the coverage
// invariant survives a message that was EMBEDDED for the generation and
// then went non-live (soft-deleted). Backend.Delete has no production
// callers, so the embedding row survives the soft-delete; an unfiltered
// EmbeddedMessageCount would then count the dead message, making
// embedded > stamped (stamped is live-only), driving blank negative
// (clamped to 0) and breaking live == embedded + blank + missing — with
// EMBEDDED able to exceed LIVE.
//
// With the live-intersected count the dead message drops out of embedded,
// so embedded <= stamped <= live, blank >= 0, and the invariant holds.
func TestCoverageSplit_NonLiveEmbeddedHoldsInvariant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	// See TestCoverageSplit_EmbeddedBlankMissing: a sqlitevec test must use a
	// SQLite main store regardless of MSGVAULT_TEST_DB.
	st := testutil.NewSQLiteTestStore(t)
	b, err := Open(ctx, Options{
		Path:      filepath.Join(t.TempDir(), "vectors.db"),
		Dimension: 8,
		MainDB:    st.DB(),
	})
	require.NoError(err, "Open backend")
	t.Cleanup(func() { _ = b.Close() })

	source, err := st.GetOrCreateSource("gmail", "me@example.com")
	require.NoError(err, "GetOrCreateSource")
	convID, err := st.EnsureConversationWithType(source.ID, "conv-1", "email_thread", "Subject")
	require.NoError(err, "EnsureConversationWithType")

	makeMsg := func(srcMsgID string) int64 {
		m := &store.Message{
			SourceID:        source.ID,
			SourceMessageID: srcMsgID,
			ConversationID:  convID,
			MessageType:     "email",
			Subject:         sql.NullString{String: "s-" + srcMsgID, Valid: true},
		}
		id, err := st.UpsertMessage(m)
		require.NoErrorf(err, "UpsertMessage %s", srcMsgID)
		return id
	}
	embeddedA := makeMsg("emb-a")
	embeddedB := makeMsg("emb-b")
	missing := makeMsg("missing")

	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err, "CreateGeneration")

	vec := func(seed float32) []float32 {
		v := make([]float32, 8)
		v[0] = seed
		return v
	}
	require.NoError(b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: embeddedA, Vector: vec(1)},
		{MessageID: embeddedB, Vector: vec(2)},
	}), "Upsert embedded vectors")
	require.NoError(st.SetEmbedGen(ctx, []int64{embeddedA, embeddedB}, int64(gen)), "stamp embedded")
	_ = missing

	// Sanity before the soft-delete: both embedded messages are live.
	embeddedBefore, err := b.EmbeddedMessageCount(ctx, gen)
	require.NoError(err, "EmbeddedMessageCount before")
	assert.Equal(int64(2), embeddedBefore, "two live embedded before soft-delete")

	// Soft-delete one EMBEDDED message (deleted_from_source_at) — its
	// embedding row stays behind, but it is no longer a live message.
	_, err = st.DB().Exec(
		st.Rebind("UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?"),
		embeddedA)
	require.NoError(err, "soft-delete embeddedA")

	// Compute the split exactly as the CLI does.
	live, stamped, _, missingCount, err := st.CoverageCounts(ctx, int64(gen))
	require.NoError(err, "CoverageCounts")
	embedded, err := b.EmbeddedMessageCount(ctx, gen)
	require.NoError(err, "EmbeddedMessageCount after")
	blank := max(stamped-embedded, 0)

	// The dead message must NOT be counted as embedded.
	assert.Equal(int64(1), embedded, "non-live embedded message excluded")
	assert.LessOrEqual(embedded, live, "embedded <= live")
	assert.LessOrEqual(embedded, stamped, "embedded <= stamped")
	assert.GreaterOrEqual(blank, int64(0), "blank >= 0")
	// live = 2 (embeddedB live-embedded + missing live-unstamped).
	assert.Equal(int64(2), live, "live excludes the soft-deleted message")
	// The load-bearing invariant survives the non-live embedded row.
	assert.Equal(live, embedded+blank+missingCount,
		"invariant: live == embedded + blank + missing")
}

func TestCoverageSplit_ScopedEmbeddedHoldsInvariant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()

	st := testutil.NewSQLiteTestStore(t)
	b, err := Open(ctx, Options{
		Path:       filepath.Join(t.TempDir(), "vectors.db"),
		Dimension:  8,
		MainDB:     st.DB(),
		BuildScope: vector.NewBuildScope([]string{"sms"}),
	})
	require.NoError(err, "Open backend")
	t.Cleanup(func() { _ = b.Close() })

	source, err := st.GetOrCreateSource("gmail", "me@example.com")
	require.NoError(err, "GetOrCreateSource")
	emailConvID, err := st.EnsureConversationWithType(source.ID, "conv-email", "email_thread", "Email")
	require.NoError(err, "EnsureConversationWithType email")
	smsConvID, err := st.EnsureConversationWithType(source.ID, "conv-sms", "sms_thread", "SMS")
	require.NoError(err, "EnsureConversationWithType sms")

	makeMsg := func(srcMsgID, typ string, convID int64) int64 {
		m := &store.Message{
			SourceID:        source.ID,
			SourceMessageID: srcMsgID,
			ConversationID:  convID,
			MessageType:     typ,
			Subject:         sql.NullString{String: "s-" + srcMsgID, Valid: true},
		}
		id, err := st.UpsertMessage(m)
		require.NoErrorf(err, "UpsertMessage %s", srcMsgID)
		return id
	}
	outOfScopeEmail := makeMsg("email-stamped", "email", emailConvID)
	inScopeSMS := makeMsg("sms-stamped", "sms", smsConvID)

	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err, "CreateGeneration")
	require.NoError(b.Upsert(ctx, gen, []vector.Chunk{
		{MessageID: outOfScopeEmail, Vector: []float32{1, 0, 0, 0, 0, 0, 0, 0}},
		{MessageID: inScopeSMS, Vector: []float32{0, 1, 0, 0, 0, 0, 0, 0}},
	}), "Upsert embedded vectors")
	require.NoError(st.SetEmbedGen(ctx, []int64{outOfScopeEmail, inScopeSMS}, int64(gen)), "stamp embedded")

	live, stamped, _, missingCount, err := st.CoverageCountsScoped(ctx, int64(gen), []string{"sms"})
	require.NoError(err, "CoverageCountsScoped")
	embedded, err := b.EmbeddedMessageCount(ctx, gen)
	require.NoError(err, "EmbeddedMessageCount")
	blank := max(stamped-embedded, 0)

	assert.Equal(int64(1), live, "only sms is in scope")
	assert.Equal(int64(1), stamped, "only scoped stamped messages count")
	assert.Equal(int64(1), embedded, "out-of-scope email vector excluded")
	assert.Equal(int64(0), blank)
	assert.Equal(int64(0), missingCount)
	assert.Equal(live, embedded+blank+missingCount,
		"invariant: live == embedded + blank + missing")
}

func TestFilteredCoverageRequiresLiveGenerationStampAndVector(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	st := testutil.NewSQLiteTestStore(t)
	b, err := Open(ctx, Options{
		Path: filepath.Join(t.TempDir(), "vectors.db"), Dimension: 8, MainDB: st.DB(),
	})
	require.NoError(err)
	t.Cleanup(func() { _ = b.Close() })

	source, err := st.GetOrCreateSource("gmail", "me@example.com")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "coverage", "email_thread", "Coverage")
	require.NoError(err)
	makeMessage := func(sourceMessageID string) int64 {
		id, err := st.UpsertMessage(&store.Message{
			SourceID: source.ID, SourceMessageID: sourceMessageID,
			ConversationID: conversationID, MessageType: "email",
		})
		require.NoError(err)
		return id
	}
	valid := makeMessage("valid")
	clearedStamp := makeMessage("cleared-stamp")
	dedupLoser := makeMessage("dedup-loser")
	sourceDeleted := makeMessage("source-deleted")
	wrongGeneration := makeMessage("wrong-generation")
	noVector := makeMessage("no-vector")

	gen, err := b.CreateGeneration(ctx, "test-model", 8, "fp")
	require.NoError(err)
	chunks := make([]vector.Chunk, 0, 5)
	for i, id := range []int64{valid, clearedStamp, dedupLoser, sourceDeleted, wrongGeneration} {
		v := make([]float32, 8)
		v[i] = 1
		chunks = append(chunks, vector.Chunk{MessageID: id, Vector: v})
	}
	require.NoError(b.Upsert(ctx, gen, chunks))
	require.NoError(st.SetEmbedGen(ctx, []int64{valid, dedupLoser, sourceDeleted, noVector}, int64(gen)))
	require.NoError(st.SetEmbedGen(ctx, []int64{wrongGeneration}, int64(gen)+99))
	_, err = st.DB().Exec(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, dedupLoser)
	require.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`, sourceDeleted)
	require.NoError(err)

	count, err := b.EmbeddedMessageCountForIDs(ctx, gen,
		[]int64{valid, clearedStamp, dedupLoser, sourceDeleted, wrongGeneration, noVector})
	require.NoError(err)
	assert.Equal(int64(1), count)

	_, err = b.EmbeddedMessageCountForIDs(ctx, gen, make([]int64, vector.FilteredCoverageBatchSize+1))
	assert.ErrorIs(err, vector.ErrCoverageBatchTooLarge)
}
