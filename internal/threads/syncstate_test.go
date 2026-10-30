package threads

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestThreadsSyncStateCoverageGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(st.IsPostgreSQL())
	}
	source, err := st.GetOrCreateSource("threads", "threads:100:200")
	require.NoError(err)
	now := time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC)
	initial, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{}, now)
	require.NoError(err)
	assert.Equal(1, initial.Version)
	assert.Equal(uint64(1), initial.CoverageGeneration)
	assert.Equal(time.Unix(1688540400, 0).UTC(), initial.HistoryFloor)
	assert.Equal(source.CreatedAt.UTC(), initial.HistoryCeiling)
	floorA := now.Add(-365 * 24 * time.Hour)
	a, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: floorA}, now.Add(time.Hour))
	require.NoError(err)
	assert.Equal(uint64(2), a.CoverageGeneration)
	assert.Equal(initial.HistoryCeiling, a.HistoryCeiling)
	same, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{}, now.Add(2*time.Hour))
	require.NoError(err)
	assert.Equal(a, same)
	// Equivalent timestamps do not reset coverage; credential generations are
	// independent of this persisted app/owner coverage configuration.
	same, err = PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: floorA.In(time.FixedZone("synthetic", 3600))}, now.Add(3*time.Hour))
	require.NoError(err)
	assert.Equal(a, same)
	b, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: floorA.Add(time.Hour)}, now.Add(4*time.Hour))
	require.NoError(err)
	again, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: floorA}, now.Add(5*time.Hour))
	require.NoError(err)
	assert.Equal(uint64(3), b.CoverageGeneration)
	assert.Equal(uint64(4), again.CoverageGeneration)
	fpA, err := (SyncRequest{}).Fingerprint(source.Identifier, a)
	require.NoError(err)
	fpAgain, err := (SyncRequest{}).Fingerprint(source.Identifier, again)
	require.NoError(err)
	assert.NotEqual(fpA, fpAgain)
	saved, err := st.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	var decoded SourceSyncConfig
	require.NoError(json.Unmarshal([]byte(saved.SyncConfig.String), &decoded))
	assert.Equal(again, decoded)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = PrepareSourceSyncConfigContext(ctx, st, source.ID, SyncRequest{After: floorA.Add(2 * time.Hour)}, now)
	assert.ErrorIs(err, context.Canceled)
	_, err = PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: time.Unix(1688540399, 0)}, now)
	assert.Error(err)
	// Targeted runs cannot silently replace source-wide coverage.
	_, err = PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{PostID: "800", After: floorA.Add(time.Hour)}, now)
	assert.Error(err)
}

func TestThreadsSyncStateFingerprint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	cfg := SourceSyncConfig{Version: 1, HistoryFloor: now.Add(-24 * time.Hour), HistoryCeiling: now, CoverageGeneration: 7}
	q, err := (SyncRequest{}).ResumeQuery("threads:100:200", cfg)
	require.NoError(err)
	full, err := (SyncRequest{Full: true}).Fingerprint("threads:100:200", cfg)
	require.NoError(err)
	assert.Equal(q.FullFingerprint, full)
	var decoded SyncFingerprint
	require.NoError(json.Unmarshal([]byte(full), &decoded))
	assert.Equal("100", decoded.AppID)
	assert.Equal("200", decoded.OwnerID)
	assert.Equal("full", decoded.Mode)
	assert.Equal(ownPostFields, decoded.PostFields)
	assert.Equal(replyFields, decoded.ReplyFields)
	assert.Equal(conversationFields, decoded.ConversationFields)
	assert.Equal("provider", decoded.ListOrder)
	assert.Equal("reverse=true", decoded.ConversationOrder)
	assert.Equal(uint64(7), decoded.CoverageGeneration)
	assert.True(cfg.HistoryFloor.Equal(decoded.Floor))
	post, err := (SyncRequest{PostID: "800"}).ResumeQuery("threads:100:200", cfg)
	require.NoError(err)
	assert.Empty(post.BaselineFingerprint)
	assert.Empty(post.FullFingerprint)
	assert.NotEmpty(post.PostFingerprint)
	for _, source := range []string{"threads:101:200", "threads:100:201"} {
		other, err := (SyncRequest{}).ResumeQuery(source, cfg)
		require.NoError(err)
		assert.NotEqual(q, other)
	}
	equivalent := cfg
	equivalent.HistoryFloor = cfg.HistoryFloor.In(time.FixedZone("synthetic", -3600))
	same, err := (SyncRequest{}).ResumeQuery("threads:100:200", equivalent)
	require.NoError(err)
	assert.Equal(q, same)
	for _, request := range []SyncRequest{{Full: true, PostID: "800"}, {PostID: "bad"}, {After: cfg.HistoryFloor.Add(time.Second)}} {
		_, err := request.Fingerprint("threads:100:200", cfg)
		assert.Error(err)
	}
	_, err = (SyncRequest{}).Fingerprint("threads:bad:200", cfg)
	assert.Error(err)
	cfg.Version = 2
	_, err = (SyncRequest{}).Fingerprint("threads:100:200", cfg)
	assert.Error(err)
}

func TestThreadsSyncStatePinnedWalks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	now := time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC)
	cfg := SourceSyncConfig{Version: 1, HistoryFloor: now.Add(-365 * 24 * time.Hour), HistoryCeiling: now.Add(-24 * time.Hour), CoverageGeneration: 3}
	fp, err := (SyncRequest{Full: true}).Fingerprint("threads:100:200", cfg)
	require.NoError(err)
	state, err := NewSyncState("epoch-A", fp, cfg, now, 42)
	require.NoError(err)
	assert.Equal("epoch-A", state.Epoch)
	assert.Equal("full", state.Mode)
	assert.Equal(cfg.HistoryFloor, state.OwnPostsHistory.Since)
	assert.Equal(cfg.HistoryCeiling, state.OwnPostsHistory.Until)
	assert.Equal(state.OwnPostsHistory, state.OwnRepliesHistory)
	assert.Equal(cfg.HistoryCeiling.Add(-time.Hour), state.OwnPostsHead.Since)
	assert.Equal(now, state.OwnPostsHead.Until)
	assert.Equal(int64(42), state.HotRoots.ThroughID)
	assert.Equal(now.Add(-7*24*time.Hour), state.HotRoots.HotSince)
	assert.Equal(int64(42), state.AllRoots.ThroughID)
	assert.True(state.AllRoots.HotSince.IsZero())
	state.HotRoots.ActiveRoot = "800"
	state.HotRoots.Cursor = "conversation-next"
	state.NextLane = 4
	state.OwnPostsHistory.Cursor = "history-next"
	state.Coverage.Incomplete = 2
	encoded, err := state.Encode()
	require.NoError(err)
	recovered, err := DecodeSyncState(encoded)
	require.NoError(err)
	assert.Equal(state, recovered)
	// Later time/root growth cannot alter a saved active interval or pass.
	require.NoError(recovered.OwnPostsHead.PinHead(cfg.HistoryFloor, now.Add(time.Hour)))
	assert.Equal(state, recovered)
	cp := &store.SyncRun{CursorBefore: sql.NullString{String: encoded, Valid: true}}
	resumed, err := ResumeSyncState(SyncRequest{}, nil, cp)
	require.NoError(err)
	assert.Equal(state, resumed)
	resumed, err = ResumeSyncState(SyncRequest{Full: true}, nil, cp)
	require.NoError(err)
	assert.Equal(state, resumed)
	baseFP, err := (SyncRequest{}).Fingerprint("threads:100:200", cfg)
	require.NoError(err)
	ordinary := *state
	ordinary.Mode = "baseline"
	ordinary.Fingerprint = baseFP
	ordinary.Epoch = "ordinary"
	raw, err := ordinary.Encode()
	require.NoError(err)
	baseline := &store.SyncRun{CursorAfter: sql.NullString{String: raw, Valid: true}}
	resumed, err = ResumeSyncState(SyncRequest{}, baseline, nil)
	require.NoError(err)
	assert.Equal(&ordinary, resumed)
	cp.CursorBefore.String = raw
	resumed, err = ResumeSyncState(SyncRequest{Full: true}, baseline, cp)
	require.NoError(err)
	assert.Nil(resumed, "explicit full starts a fresh epoch when only an ordinary checkpoint exists")
	postFP, err := (SyncRequest{PostID: "800"}).Fingerprint("threads:100:200", cfg)
	require.NoError(err)
	post, err := NewSyncState("post-A", postFP, cfg, now, 42)
	require.NoError(err)
	assert.Equal("800", post.TargetRoot.ActiveRoot)
	raw, err = post.Encode()
	require.NoError(err)
	cp.CursorBefore.String = raw
	resumed, err = ResumeSyncState(SyncRequest{PostID: "800"}, nil, cp)
	require.NoError(err)
	assert.Equal(post, resumed)
	_, err = ResumeSyncState(SyncRequest{}, nil, cp)
	assert.Error(err)
	_, err = ResumeSyncState(SyncRequest{PostID: "801"}, nil, cp)
	assert.Error(err)
	_, err = DecodeSyncState(`{"version":99}`)
	assert.Error(err)
	_, err = DecodeSyncState(`{"version":1}`)
	assert.Error(err)
}

func TestThreadsSyncStateHeadOverlap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	floor := time.Unix(1688540400, 123456789).UTC()
	completed := floor.Add(48 * time.Hour)
	walk := FeedWalk{CompletedThrough: completed, Done: true}
	until := completed.Add(time.Hour)
	require.NoError(walk.PinHead(floor, until))
	assert.Equal(completed.Add(-time.Hour), walk.Since)
	assert.Equal(until, walk.Until)
	assert.False(walk.Done)
	assert.Equal(123456789, walk.Since.Nanosecond())
	// Equal and subsecond bounds remain exact locally; the importer decides
	// when enough whole-second API coverage can be requested.
	small := FeedWalk{CompletedThrough: floor, Done: true}
	require.NoError(small.PinHead(floor, floor.Add(time.Nanosecond)))
	assert.Equal(floor, small.Since)
	assert.Equal(floor.Add(time.Nanosecond), small.Until)
	_, err := NewSyncState("", "", SourceSyncConfig{}, until, 0)
	assert.Error(err)
}

func TestThreadsSyncStateConfigIntegrity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(st.IsPostgreSQL())
	}
	source, err := st.GetOrCreateSource("threads", "threads:100:200")
	require.NoError(err)
	initial, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{}, source.CreatedAt.Add(time.Hour))
	require.NoError(err)
	assert.Equal(source.CreatedAt.UTC(), initial.HistoryCeiling, "missing config must use original source creation, not a later reconnect/run")
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{`{}`, `null`, `{"version":1}`, `{"version":2,"history_floor":"2026-01-01T00:00:00Z","history_ceiling":"2026-10-30T12:00:00Z","coverage_generation":1}`} {
		require.NoError(st.UpdateSourceSyncConfigContext(t.Context(), source.ID, raw))
		_, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{}, now)
		assert.Error(err, "invalid config must not fabricate coverage: %s", raw)
	}
	require.NoError(st.UpdateSourceSyncConfigContext(t.Context(), source.ID, `{"version":1,"history_floor":"2026-01-01T00:00:00Z","history_ceiling":"2026-10-01T12:00:00Z","coverage_generation":18446744073709551615}`))
	_, err = PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: now.Add(-24 * time.Hour)}, now)
	assert.Error(err)
	require.NoError(st.UpdateSourceSyncConfigContext(t.Context(), source.ID, `{"version":1,"history_floor":"2026-01-01T00:00:00Z","history_ceiling":"2026-10-01T12:00:00Z","coverage_generation":1,"extra":{"keep":true}}`))
	conv, err := st.EnsureConversation(source.ID, "800", "Synthetic root")
	require.NoError(err)
	message, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "800", MessageType: "threads"})
	require.NoError(err)
	// A synthetic existing uncertain send proves coverage changes never reset
	// durable receipts. Task 3 owns the schema; no publishing path is invoked.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO threads_reply_sends
  (draft_id,source_id,source_identifier,conversation_id,draft_revision,root_media_id,parent_media_id,body,body_hash,state)
  VALUES ('synthetic-receipt',?,'threads:100:200',?,1,'800','800','Synthetic reply','synthetic-hash','remote_unknown')`), source.ID, conv)
	require.NoError(err)
	cfg, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: now.Add(-24 * time.Hour)}, now)
	require.NoError(err)
	assert.Equal(uint64(2), cfg.CoverageGeneration)
	saved, err := st.GetSourceByIDContext(t.Context(), source.ID)
	require.NoError(err)
	var fields map[string]any
	require.NoError(json.Unmarshal([]byte(saved.SyncConfig.String), &fields))
	assert.Equal(map[string]any{"keep": true}, fields["extra"])
	var storedID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id='800'`), source.ID).Scan(&storedID))
	assert.Equal(message, storedID)
	var state string
	require.NoError(st.DB().QueryRow(`SELECT state FROM threads_reply_sends WHERE draft_id='synthetic-receipt'`).Scan(&state))
	assert.Equal("remote_unknown", state)
	fp, err := (SyncRequest{}).Fingerprint(source.Identifier, cfg)
	require.NoError(err)
	fresh, err := NewSyncState("empty-history", fp, cfg, now, 0)
	require.NoError(err)
	assert.True(fresh.OwnPostsHistory.Done, "a floor after the original ceiling has no history interval")
	_, err = PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: now.Add(time.Second)}, now)
	assert.NoError(err)
}

func TestThreadsSyncStateFutureFloorDefersIntervals(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(st.IsPostgreSQL())
	}
	source, err := st.GetOrCreateSource("threads", "threads:100:200")
	require.NoError(err)
	connected := time.Date(2026, 10, 30, 12, 0, 0, 123456789, time.UTC)
	original := SourceSyncConfig{Version: 1, HistoryFloor: time.Unix(1688540400, 0).UTC(), HistoryCeiling: connected, CoverageGeneration: 1}
	encoded, err := json.Marshal(original)
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncConfigContext(t.Context(), source.ID, string(encoded)))
	future := connected.Add(7 * 24 * time.Hour)
	cfg, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{After: future}, connected.Add(time.Hour))
	require.NoError(err)
	assert.Equal(future, cfg.HistoryFloor)
	assert.Equal(connected, cfg.HistoryCeiling)
	assert.Equal(uint64(2), cfg.CoverageGeneration)
	same, err := PrepareSourceSyncConfigContext(t.Context(), st, source.ID, SyncRequest{}, connected.Add(2*time.Hour))
	require.NoError(err)
	assert.Equal(cfg, same)
	fp, err := (SyncRequest{}).Fingerprint(source.Identifier, cfg)
	require.NoError(err)
	state, err := NewSyncState("future-floor", fp, cfg, connected.Add(time.Hour), 0)
	require.NoError(err)
	assert.True(state.OwnPostsHistory.Done)
	assert.True(state.OwnRepliesHistory.Done)
	assert.True(state.OwnPostsHead.Done)
	assert.True(state.OwnRepliesHead.Done)
	assert.Equal(future, state.OwnPostsHead.Since)
	assert.Equal(connected.Add(time.Hour), state.OwnPostsHead.Until)
	raw, err := state.Encode()
	require.NoError(err)
	saved, err := DecodeSyncState(raw)
	require.NoError(err)
	assert.Equal(state, saved)
	// A later invocation can pin an actual interval once the floor is reached.
	require.NoError(saved.OwnPostsHead.PinHead(cfg.HistoryFloor, future))
	assert.True(saved.OwnPostsHead.Done, "equal bounds must not issue HTTP")
	require.NoError(saved.OwnPostsHead.PinHead(cfg.HistoryFloor, future.Add(time.Hour)))
	assert.False(saved.OwnPostsHead.Done)
	assert.Equal(future, saved.OwnPostsHead.Since)
}

func TestThreadsSyncStateTargetedRejectsAfterBeforeIO(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	if os.Getenv("MSGVAULT_TEST_DB") != "" {
		require.True(st.IsPostgreSQL())
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	floor := time.Unix(1688540400, 0).UTC()
	for _, request := range []SyncRequest{{PostID: "800", After: floor}, {PostID: "800", After: floor.Add(24 * time.Hour)}, {PostID: "800", Full: true}} {
		assert.Error(request.Validate())
		// A cancelled context on a real Store distinguishes option validation
		// from a database read. Invalid options must win before any Store I/O.
		_, err := PrepareSourceSyncConfigContext(ctx, st, 1, request, floor.Add(time.Hour))
		require.Error(err)
		assert.NotErrorIs(err, context.Canceled)
	}
	cfg := SourceSyncConfig{Version: 1, HistoryFloor: floor, HistoryCeiling: floor.Add(time.Hour), CoverageGeneration: 1}
	_, err := (SyncRequest{PostID: "800", After: floor}).Fingerprint("threads:100:200", cfg)
	assert.Error(err)
}
