package meta

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const checkpointV2Fixture = `{"version":2,"source":"app:9:page:100","next_surface":"ads_posts","phases":{"posts":{},"visitor_posts":{},"ads_posts":{}}}`

func TestImportCollectionTurnsSurviveLongPhaseAndDenial(t *testing.T) {
	t.Run("unfinished organic collection turns", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st, f, imp, src := setupImport(t)
		for _, id := range []string{"100_401", "100_402", "100_403"} {
			f.comments["100_200"] = append(f.comments["100_200"], *comment(id, "100_200", "synthetic organic history"))
		}
		f.denied["100/visitor_posts"] = true
		f.ads = []Object{{ID: "100_800", Message: "Synthetic ad"}}
		for _, turn := range []struct {
			budget int
			path   string
			next   PostEdge
		}{{4, "100/posts", VisitorPosts}, {2, "100/visitor_posts", AdsPosts}, {2, "100/ads_posts", Posts}} {
			before := len(f.paths)
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: turn.budget})
			if turn.next == AdsPosts {
				require.ErrorContains(err, "facebook visitor_posts")
			} else {
				require.NoError(err)
			}
			require.NotNil(sum)
			assert.LessOrEqual(len(f.paths)-before, turn.budget)
			assert.Equal("/v26.0/"+turn.path, f.paths[before+1])
			run, err := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			var saved syncState
			require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &saved))
			assert.Equal(turn.next, saved.NextSurface)
			assert.False(saved.Phases[Posts].Done)
			require.NotEmpty(saved.Phases[Posts].Stack)
		}
		message(t, st, src.ID, "100_800")
	})
	t.Run("budget two empty organic visitor denial eventually reaches ad comment", func(t *testing.T) {
		assert, require := assert.New(t), require.New(t)
		st, f, imp, src := setupImport(t)
		f.posts = nil
		f.denied["100/visitor_posts"] = true
		f.ads = []Object{{ID: "100_800", Message: "Synthetic ad post"}}
		f.comments["100_800"] = []Object{*comment("100_801", "100_800", "Synthetic ad comment")}
		for range 16 {
			before := f.calls
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 2})
			if err != nil {
				require.ErrorContains(err, "facebook visitor_posts")
			}
			require.NotNil(sum)
			assert.LessOrEqual(f.calls-before, 2)
			ids, readErr := st.MessageExistsBatch(src.ID, []string{"100_800", "100_801"})
			require.NoError(readErr)
			if ids["100_801"] != 0 {
				assert.NotZero(ids["100_800"])
				assert.Equal(message(t, st, src.ID, "100_800").ID, message(t, st, src.ID, "100_801").ReplyToMessageID.Int64)
				return
			}
		}
		assert.Fail("accessible ad post/comment starved under repeated bounded imports")
	})
}

func TestImportContinuationRequiresCollectionAttempt(t *testing.T) {
	for _, stop := range []string{"budget one", "identity denial", "yield before collection", "skipped completed surface", "completed scan"} {
		t.Run(stop, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			id, err := st.StartSync(src.ID, SourceType)
			require.NoError(err)
			checkpoint := checkpointV2Fixture
			switch stop {
			case "skipped completed surface":
				checkpoint = strings.Replace(checkpoint, `"ads_posts":{}`, `"ads_posts":{"done":true,"exhausted":true}`, 1)
			case "completed scan":
				checkpoint = strings.ReplaceAll(checkpoint, `:{}`, `:{"done":true,"exhausted":true}`)
			}
			require.NoError(st.FailSyncWithCheckpoint(id, "synthetic resume", &store.Checkpoint{PageToken: checkpoint}))
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			f.denied["me"] = stop == "identity denial"
			if stop == "yield before collection" {
				imp.api = yieldingAPI{API: imp.api, cancel: cancel, yieldBeforeCollection: true}
			}
			before := f.calls
			_, err = imp.Import(ctx, "9", "100", ImportOptions{MaxRequests: 1})
			if stop == "identity denial" || stop == "yield before collection" {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			assert.Equal(before+1, f.calls)
			run, err := st.GetLatestCheckpointedSync(src.ID)
			require.NoError(err)
			var saved syncState
			require.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &saved))
			assert.Equal(AdsPosts, saved.NextSurface)
		})
	}
}

func TestImportSnapshotsCyclicOrderForWholeRun(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, f, imp, src := setupImport(t)
	f.posts = nil
	id, err := st.StartSync(src.ID, SourceType)
	require.NoError(err)
	require.NoError(st.FailSyncWithCheckpoint(id, "synthetic resume", &store.Checkpoint{PageToken: checkpointV2Fixture}))
	before := len(f.paths)
	sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
	require.NoError(err)
	assert.False(sum.Partial)
	assert.Equal([]string{"/v26.0/me", "/v26.0/100/ads_posts", "/v26.0/100/posts", "/v26.0/100/visitor_posts"}, f.paths[before:])
}

func TestImportRefusesNewestIncompatibleCheckpointAndFullRecovers(t *testing.T) {
	for _, edit := range []struct{ name, from, to string }{
		{"missing version", `"version":2,`, ""},
		{"old version", `"version":2`, `"version":1`},
		{"future version", `"version":2`, `"version":3`},
		{"missing continuation", `"next_surface":"ads_posts",`, ""},
		{"invalid continuation", `"next_surface":"ads_posts"`, `"next_surface":"me"`},
		{"unknown surface", `"visitor_posts":{}`, `"arbitrary":{}`},
		{"extra surface", `"posts":{}`, `"posts":{},"arbitrary":{}`},
	} {
		t.Run(edit.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, f, imp, src := setupImport(t)
			_, err := imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.NoError(err)
			original := message(t, st, src.ID, "100_200").ID
			id, err := st.StartSync(src.ID, SourceType)
			require.NoError(err)
			bad := strings.Replace(checkpointV2Fixture, edit.from, edit.to, 1)
			require.NoError(st.FailSyncWithCheckpoint(id, "newest synthetic checkpoint", &store.Checkpoint{PageToken: bad}))
			before := f.calls
			_, err = imp.Import(t.Context(), "9", "100", ImportOptions{})
			require.ErrorContains(err, "--full")
			assert.Equal(before, f.calls, "never fall back to the older valid successful cursor")
			sum, err := imp.Import(t.Context(), "9", "100", ImportOptions{Full: true})
			require.NoError(err)
			assert.False(sum.Partial)
			assert.Equal(original, message(t, st, src.ID, "100_200").ID)
		})
	}
}

func TestLoadStateSurvivesWebhookCompletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, imp, src := setupImport(t)
	summary, err := imp.Import(t.Context(), "9", "100", ImportOptions{MaxRequests: 2})
	require.NoError(err)
	require.True(summary.Partial)
	before, err := loadState(t.Context(), st, src, false)
	require.NoError(err)
	require.NotEqual(freshSourceState(src), before)
	execution, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	require.NoError(err)
	defer func() { require.NoError(execution.Release()) }()
	run, err := execution.StartSyncContext(t.Context(), WebhookSyncType, "")
	require.NoError(err)
	require.NoError(st.CompleteSyncContext(t.Context(), run, ""))
	after, err := loadState(t.Context(), st, src, false)
	require.NoError(err)
	assert.Equal(before.Phases, after.Phases)
	assert.Equal(before.NextSurface, after.NextSurface)
}

func TestLoadStateInstagramSurvivesWebhookCompletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	src, err := st.GetOrCreateSource(InstagramSourceType, "app:9:page:100:instagram:200")
	require.NoError(err)
	state := freshSourceState(src)
	state.Phases[instagramMediaEdge].After = "opaque-ig-cursor"
	blob, err := json.Marshal(state)
	require.NoError(err)
	run, err := st.StartSync(src.ID, InstagramSourceType)
	require.NoError(err)
	require.NoError(st.PauseSyncWithCheckpoint(run, &store.Checkpoint{PageToken: string(blob)}))
	other, err := st.StartSync(src.ID, "instagram_comments_webhook")
	require.NoError(err)
	require.NoError(st.CompleteSync(other, ""))
	got, err := loadState(t.Context(), st, src, false)
	require.NoError(err)
	assert.Equal(state, got)
}
