package store_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func threadsStore(t *testing.T) (*store.Store, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(t, st.IsPostgreSQL())
	}
	source, err := st.GetOrCreateSource("threads", "threads:100:200")
	require.NoError(t, err)
	return st, source.ID
}

func threadsRun(t *testing.T, st *store.Store, source int64, mode, fingerprint, status, state string) int64 {
	t.Helper()
	execution, err := st.AcquireSyncExecutionContext(t.Context(), source)
	require.NoError(t, err)
	defer func() { require.NoError(t, execution.Release()) }()
	id, err := execution.StartSyncWithRequestContext(t.Context(), mode, "", fingerprint)
	require.NoError(t, err)
	cp := &store.Checkpoint{PageToken: state}
	switch status {
	case store.SyncStatusCompleted:
		require.NoError(t, st.CompleteSyncContext(t.Context(), id, state))
	case store.SyncStatusPaused:
		require.NoError(t, st.PauseSyncWithCheckpointContext(t.Context(), id, cp))
	case store.SyncStatusFailed:
		require.NoError(t, st.FailSyncWithCheckpointContext(t.Context(), id, "synthetic failure", cp))
	case store.SyncStatusCancelled:
		require.NoError(t, st.UpdateSyncCheckpointContext(t.Context(), id, cp))
		// A cancelled terminal row has the same durable contract as scheduler handoff.
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sync_runs SET status='cancelled' WHERE id=?`), id)
		require.NoError(t, err)
	default:
		require.FailNow(t, "unexpected test run status")
	}
	return id
}

func TestThreadsCompatibleResume(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := threadsStore(t)
	ctx := t.Context()
	q := store.ThreadsResumeQuery{BaselineFingerprint: "floor-A-g1-baseline", FullFingerprint: "floor-A-g1-full"}
	post := store.ThreadsResumeQuery{PostFingerprint: "floor-A-g1-post-800"}
	check := func(q store.ThreadsResumeQuery, wantBase, wantCheckpoint int64, state string) {
		t.Helper()
		base, cp, err := st.GetThreadsSyncResumeContext(ctx, source, q)
		require.NoError(err)
		if wantBase == 0 {
			assert.Nil(base)
		} else {
			require.NotNil(base)
			assert.Equal(wantBase, base.ID)
		}
		if wantCheckpoint == 0 {
			assert.Nil(cp)
		} else {
			require.NotNil(cp)
			assert.Equal(wantCheckpoint, cp.ID)
			assert.Equal(state, cp.CursorBefore.String)
		}
	}
	check(q, 0, 0, "")
	initial := threadsRun(t, st, source, store.ThreadsSyncBaseline, q.BaselineFingerprint, store.SyncStatusCompleted, `{"epoch":"initial"}`)
	a := threadsRun(t, st, source, store.ThreadsSyncFull, q.FullFingerprint, store.SyncStatusPaused, `{"epoch":"full-A","next_lane":3}`)
	b := threadsRun(t, st, source, store.ThreadsSyncPost, post.PostFingerprint, store.SyncStatusCompleted, `{"root":"800"}`)
	threadsRun(t, st, source, store.ThreadsSyncFull, "incompatible-C", store.SyncStatusCompleted, `{"epoch":"C"}`)
	// Normal D and repeated --full use the same compatibility pair. Credential
	// generation is deliberately absent; same app/owner reauth keeps the walk.
	check(q, initial, a, `{"epoch":"full-A","next_lane":3}`)
	check(post, b, 0, "")
	fullComplete := threadsRun(t, st, source, store.ThreadsSyncFull, q.FullFingerprint, store.SyncStatusCompleted, `{"epoch":"normalized-source-baseline"}`)
	threadsRun(t, st, source, store.ThreadsSyncPost, post.PostFingerprint, store.SyncStatusCompleted, `{"root":"800","new":true}`)
	check(q, fullComplete, 0, "")
	base, _, err := st.GetThreadsSyncResumeContext(ctx, source, q)
	require.NoError(err)
	require.NotNil(base)
	assert.Equal(q.FullFingerprint, base.RequestFingerprint.String)
	assert.Equal(`{"epoch":"normalized-source-baseline"}`, base.CursorAfter.String)
	for _, status := range []string{store.SyncStatusFailed, store.SyncStatusCancelled, store.SyncStatusPaused} {
		id := threadsRun(t, st, source, store.ThreadsSyncBaseline, q.BaselineFingerprint, status, `{"committed_page":2}`)
		check(q, fullComplete, id, `{"committed_page":2}`)
	}
	// Wrong mode with an otherwise exact fingerprint is never compatible.
	threadsRun(t, st, source, store.ThreadsSyncPost, q.BaselineFingerprint, store.SyncStatusCompleted, `{"wrong_mode":true}`)
	_, cp, err := st.GetThreadsSyncResumeContext(ctx, source, q)
	require.NoError(err)
	require.NotNil(cp)
	assert.Equal(store.SyncStatusPaused, cp.Status)
	// A newer interrupted run without a checkpoint must not hide committed work.
	threadsRun(t, st, source, store.ThreadsSyncFull, q.FullFingerprint, store.SyncStatusFailed, "")
	check(q, fullComplete, cp.ID, `{"committed_page":2}`)
	// A -> B -> A changes coverage generation, so neither old A nor B returns.
	qB := store.ThreadsResumeQuery{BaselineFingerprint: "floor-B-g2-baseline", FullFingerprint: "floor-B-g2-full"}
	check(qB, 0, 0, "")
	threadsRun(t, st, source, store.ThreadsSyncFull, qB.FullFingerprint, store.SyncStatusPaused, `{"epoch":"B"}`)
	check(store.ThreadsResumeQuery{BaselineFingerprint: "floor-A-g3-baseline", FullFingerprint: "floor-A-g3-full"}, 0, 0, "")
	other, err := st.GetOrCreateSource("threads", "threads:100:201")
	require.NoError(err)
	threadsRun(t, st, other.ID, store.ThreadsSyncFull, q.FullFingerprint, store.SyncStatusCompleted, `{"other_source":true}`)
	check(q, fullComplete, cp.ID, `{"committed_page":2}`)
	// Running rows are considered only after execution-lease recovery.
	execution, err := st.AcquireSyncExecutionContext(ctx, source)
	require.NoError(err)
	defer func() { require.NoError(execution.Release()) }()
	running, err := execution.StartSyncWithRequestContext(ctx, store.ThreadsSyncFull, "", q.FullFingerprint)
	require.NoError(err)
	require.NoError(st.UpdateSyncCheckpointContext(ctx, running, &store.Checkpoint{PageToken: `{"not_recovered":true}`}))
	check(q, fullComplete, cp.ID, `{"committed_page":2}`)
	require.NoError(st.FailSyncContext(ctx, running, "test cleanup"))
}

func TestThreadsCompatibleResumeRejectsAmbiguousQuery(t *testing.T) {
	st, source := threadsStore(t)
	for _, q := range []store.ThreadsResumeQuery{{}, {BaselineFingerprint: "a"}, {FullFingerprint: "f"}, {BaselineFingerprint: "a", FullFingerprint: "a"}, {BaselineFingerprint: "a", FullFingerprint: "f", PostFingerprint: "p"}} {
		_, _, err := st.GetThreadsSyncResumeContext(t.Context(), source, q)
		assert.Error(t, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, err := st.GetThreadsSyncResumeContext(ctx, source, store.ThreadsResumeQuery{PostFingerprint: "p"})
	assert.ErrorIs(t, err, context.Canceled)
}

func TestThreadsRootKeysetAndPause(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := threadsStore(t)
	ctx := t.Context()
	upper, err := st.ThreadsOwnRootUpperKeyContext(ctx, source)
	require.NoError(err)
	assert.Zero(upper)
	roots, err := st.ListThreadsOwnRootsContext(ctx, source, 0, upper, time.Time{}, 2)
	require.NoError(err)
	assert.Empty(roots)
	conv, err := st.EnsureConversation(source, "800", "Synthetic root")
	require.NoError(err)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	hotSince := now.Add(-7 * 24 * time.Hour)
	add := func(media, metadata string, when time.Time, own bool) int64 {
		t.Helper()
		id, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: conv, SourceMessageID: media, MessageType: "threads", SentAt: sql.NullTime{Time: when, Valid: true}, IsFromMe: own})
		require.NoError(err)
		if metadata != "" {
			require.NoError(st.SetMessageMetadataContext(ctx, id, sql.NullString{String: metadata, Valid: true}))
		}
		return id
	}
	old := add("800", `{"root_id":"800","root_owned":true,"has_replies":false}`, now.Add(-365*24*time.Hour), true)
	edge := add("801", `{"root_id":"801","root_owned":true}`, hotSince, true)
	equal := add("802", `{"root_id":"802","root_owned":true}`, hotSince, true)
	add("803", `{"root_id":"803","root_owned":false}`, now, false)
	add("804", `{"root_id":"803","parent_id":"803","root_owned":false}`, now, true)
	add("805", `{"root_id":"803","parent_id":"803","root_owned":true}`, now, true)
	add("806", `{"root_id":"806","root_owned":"true"}`, now, true)
	add("807", `{"root_id":"807","root_owned":1}`, now, true)
	add("808", `{"root_id":808,"root_owned":true}`, now, true)
	add("809", "", now, true)
	add("810", `{"root_id":"810","root_owned":true,"parent_id":"800"}`, now, true)
	upper, err = st.ThreadsOwnRootUpperKeyContext(ctx, source)
	require.NoError(err)
	assert.Equal(equal, upper)
	late := add("811", `{"root_id":"811","root_owned":true}`, now, true)
	roots, err = st.ListThreadsOwnRootsContext(ctx, source, 0, upper, time.Time{}, 2)
	require.NoError(err)
	require.Len(roots, 2)
	assert.Equal([]int64{old, edge}, []int64{roots[0].MessageID, roots[1].MessageID})
	assert.Equal("800", roots[0].MediaID)
	assert.True(roots[0].PublishedAt.Equal(now.Add(-365 * 24 * time.Hour)))
	roots, err = st.ListThreadsOwnRootsContext(ctx, source, edge, upper, time.Time{}, 2)
	require.NoError(err)
	require.Len(roots, 1)
	assert.Equal(equal, roots[0].MessageID)
	roots, err = st.ListThreadsOwnRootsContext(ctx, source, equal, upper, time.Time{}, 2)
	require.NoError(err)
	assert.Empty(roots)
	roots, err = st.ListThreadsOwnRootsContext(ctx, source, 0, upper, hotSince, 10)
	require.NoError(err)
	require.Len(roots, 2)
	assert.Equal(edge, roots[0].MessageID)
	// A resumed pass uses its saved hotSince even after these roots age out.
	roots, err = st.ListThreadsOwnRootsContext(ctx, source, edge, upper, hotSince, 10)
	require.NoError(err)
	require.Len(roots, 1)
	assert.Equal(equal, roots[0].MessageID)
	nextUpper, err := st.ThreadsOwnRootUpperKeyContext(ctx, source)
	require.NoError(err)
	assert.Equal(late, nextUpper)
	roots, err = st.ListThreadsOwnRootsContext(ctx, source, 0, nextUpper, time.Time{}, 20)
	require.NoError(err)
	require.Len(roots, 4)
	assert.Equal(old, roots[0].MessageID)
	assert.Equal(late, roots[3].MessageID)
	// No bodies were written: selection depends only on messages and metadata.
	var bodies int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM message_bodies`).Scan(&bodies))
	assert.Zero(bodies)
	id, err := st.StartSyncContext(ctx, source, store.ThreadsSyncBaseline)
	require.NoError(err)
	checkpoint := `{"version":1,"hot_roots":{"through_id":3,"hot_since":"2026-10-23T12:00:00Z","active_root":"802","cursor":"next"}}`
	require.NoError(st.PauseSyncWithCheckpointContext(ctx, id, &store.Checkpoint{PageToken: checkpoint, MessagesAdded: 3}))
	var status, before string
	var after sql.NullString
	var added int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT status,cursor_before,cursor_after,messages_added FROM sync_runs WHERE id=?`), id).Scan(&status, &before, &after, &added))
	assert.Equal(store.SyncStatusPaused, status)
	assert.Equal(checkpoint, before)
	assert.False(after.Valid)
	assert.Equal(int64(3), added)
}

func TestThreadsRootKeysetAndPauseCancellation(t *testing.T) {
	assert := assert.New(t)
	st, source := threadsStore(t)
	id, err := st.StartSyncContext(t.Context(), source, store.ThreadsSyncBaseline)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.FailSync(id, "cleanup")) }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(st.PauseSyncWithCheckpointContext(ctx, id, &store.Checkpoint{PageToken: "state"}), context.Canceled)
	assert.ErrorIs(st.UpdateSourceSyncConfigContext(ctx, source, `{"version":1}`), context.Canceled)
	_, err = st.ThreadsOwnRootUpperKeyContext(ctx, source)
	assert.ErrorIs(err, context.Canceled)
	_, err = st.ListThreadsOwnRootsContext(ctx, source, 0, 1, time.Time{}, 10)
	assert.ErrorIs(err, context.Canceled)
	for _, limit := range []int{0, -1} {
		_, err = st.ListThreadsOwnRootsContext(t.Context(), source, 0, 1, time.Time{}, limit)
		assert.Error(err)
	}
}

func TestThreadsRootKeysetExactHotBoundary(t *testing.T) {
	require := require.New(t)
	st, source := threadsStore(t)
	conv, err := st.EnsureConversation(source, "800", "Synthetic root")
	require.NoError(err)
	floor := time.Date(2026, 10, 23, 12, 0, 0, 123456000, time.UTC)
	want := []int64{}
	for i, when := range []time.Time{floor.Add(-time.Microsecond), floor.In(time.FixedZone("synthetic-west", -12*3600)), floor, floor.Add(time.Microsecond)} {
		media := []string{"800", "801", "802", "803"}[i]
		id, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: conv, SourceMessageID: media, MessageType: "threads", SentAt: sql.NullTime{Time: when, Valid: true}})
		require.NoError(err)
		require.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"root_id":"` + media + `","root_owned":true}`, Valid: true}))
		if i > 0 {
			want = append(want, id)
		}
	}
	upper, err := st.ThreadsOwnRootUpperKeyContext(t.Context(), source)
	require.NoError(err)
	after := int64(0)
	got := []int64{}
	for {
		page, err := st.ListThreadsOwnRootsContext(t.Context(), source, after, upper, floor, 1)
		require.NoError(err)
		if len(page) == 0 {
			break
		}
		require.Len(page, 1)
		got = append(got, page[0].MessageID)
		after = page[0].MessageID
	}
	assert.Equal(t, want, got, "hot cutoff compares instants, including equal instants with another offset")
}

func TestThreadsRootKeysetRejectsOtherTypes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source := threadsStore(t)
	conv, err := st.EnsureConversation(source, "800", "Synthetic root")
	require.NoError(err)
	other, err := st.GetOrCreateSource("gmail", "synthetic@example.com")
	require.NoError(err)
	otherConv, err := st.EnsureConversation(other.ID, "802", "Synthetic other source")
	require.NoError(err)
	add := func(sourceID, conversationID int64, media, kind string) int64 {
		t.Helper()
		id, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conversationID, SourceMessageID: media, MessageType: kind, SentAt: sql.NullTime{Time: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), Valid: true}})
		require.NoError(err)
		require.NoError(st.SetMessageMetadata(id, sql.NullString{String: `{"root_id":"` + media + `","root_owned":true}`, Valid: true}))
		return id
	}
	valid := add(source, conv, "800", "threads")
	add(source, conv, "801", "email")
	add(other.ID, otherConv, "802", "threads")
	upper, err := st.ThreadsOwnRootUpperKeyContext(t.Context(), source)
	require.NoError(err)
	assert.Equal(valid, upper)
	roots, err := st.ListThreadsOwnRootsContext(t.Context(), source, 0, 1<<62, time.Time{}, 10)
	require.NoError(err)
	if assert.Len(roots, 1) {
		assert.Equal(valid, roots[0].MessageID)
	}
	upper, err = st.ThreadsOwnRootUpperKeyContext(t.Context(), other.ID)
	require.NoError(err)
	assert.Zero(upper)
	roots, err = st.ListThreadsOwnRootsContext(t.Context(), other.ID, 0, 1<<62, time.Time{}, 10)
	require.NoError(err)
	assert.Empty(roots)
}
