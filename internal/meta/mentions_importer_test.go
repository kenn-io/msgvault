package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type mentionFixture struct {
	media             []IGMentionMedia
	lookup            IGMentionMedia
	linked            string
	status            int
	calls, freshReads atomic.Int32
}

func setupMentions(t *testing.T) (*store.Store, *mentionFixture, *MentionImporter, *store.Source) {
	t.Helper()
	f := &mentionFixture{lookup: mentionRoot(), linked: mentionIG}
	for n := 1; n <= 5; n++ {
		m := mentionRoot()
		m.ID = fmt.Sprintf("1790000000000000%d", n)
		f.media = append(f.media, m)
	}
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var wire any
		switch r.URL.Path {
		case "/v26.0/me":
			wire = Page{ID: "100", Name: "Example Page", Category: "Community"}
		case "/v26.0/100":
			wire = map[string]any{"id": "100", "instagram_business_account": IGUser{ID: f.linked, Username: "example_account"}}
		case "/v26.0/" + mentionIG + "/tags":
			if f.status != 0 {
				w.WriteHeader(f.status)
				return
			}
			after := r.URL.Query().Get("after")
			if after == "" {
				f.freshReads.Add(1)
			}
			start, _ := strconv.Atoi(after)
			end := min(start+2, len(f.media))
			wire = map[string]any{"data": f.media[start:end], "paging": map[string]any{"cursors": map[string]string{"after": strconv.Itoa(end)}}}
		case "/v26.0/" + mentionIG:
			if strings.HasPrefix(r.URL.Query().Get("fields"), "mentioned_comment") {
				wire = map[string]any{"mentioned_comment": IGMentionComment{ID: mentionComment, Text: "hello acct", Media: &f.lookup}}
			} else {
				wire = map[string]any{"mentioned_media": f.lookup}
			}
		default:
			assert.Fail(t, "unexpected fixture route", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, err := json.Marshal(wire)
		if !assert.NoError(t, err) {
			return
		}
		_, _ = w.Write(data)
	})
	st := testutil.NewSQLiteTestStore(t)
	imp := NewMentionImporter(st, c)
	src, err := imp.Register(t.Context(), "9", "100", mentionIG)
	require.NoError(t, err)
	f.calls.Store(0)
	return st, f, imp, src
}

func TestMentionRegisterBindsSource(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, f, imp, src := setupMentions(t)
	assertions.Equal("instagram_mentions", src.SourceType)
	assertions.Equal("app:9:page:100:instagram:17841400000000001", src.Identifier)
	assertions.Equal("example_account", src.DisplayName.String)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM account_identities WHERE source_id=?`, src.ID).Scan(&count))
	assertions.Equal(1, count)
	f.linked = "999"
	_, err := imp.Register(t.Context(), "10", "100", mentionIG)
	requirements.Error(err)
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM sources`).Scan(&count))
	assertions.Equal(1, count)
}

func TestMentionImportTagsBudgetResume(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, f, imp, src := setupMentions(t)
	for _, max := range []int{1, 2, 3} {
		before := f.calls.Load()
		sum, err := imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{MaxRequests: max})
		requirements.NoError(err)
		assertions.True(sum.Partial)
		assertions.Equal(max, sum.Requests)
		assertions.Equal(int32(max), f.calls.Load()-before)
		assertions.Equal(max == 3, sum.MessagesAdded > 0)
	}
	_, err := imp.Resolve(t.Context(), "9", "100", mentionIG, MentionEvent{MediaID: mentionMedia, CommentID: mentionComment})
	requirements.NoError(err)
	sum, err := imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	requirements.NoError(err)
	assertions.False(sum.Partial)
	assertions.Equal(int32(1), f.freshReads.Load(), "webhook completion cannot hide typed polling checkpoint")
	run, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), src.ID, "instagram_mentions")
	requirements.NoError(err)
	var state mentionSyncState
	requirements.NoError(json.Unmarshal([]byte(run.CursorAfter.String), &state))
	assertions.True(state.Done)
	sum, err = imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	requirements.NoError(err)
	assertions.Zero(sum.MessagesAdded)
	assertions.Equal(int32(2), f.freshReads.Load(), "completed cycle restarts")
}

func TestMentionUpdatesPersistSyncCounters(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st, fixture, importer, source := setupMentions(t)
	_, err := importer.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	require.NoError(err)
	fixture.media[0].Caption += "\nEdited caption"
	summary, err := importer.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	require.NoError(err)
	assert.Equal(1, summary.MessagesUpdated)
	checkRun := func(kind string) {
		t.Helper()
		run, err := st.GetLastSuccessfulSyncByTypeContext(t.Context(), source.ID, kind)
		require.NoError(err)
		assert.Equal(int64(1), run.MessagesUpdated)
	}
	checkRun(MentionsSourceType)
	fixture.lookup = fixture.media[0]
	fixture.lookup.Caption += "\nWebhook edit"
	summary, err = importer.Resolve(t.Context(), "9", "100", mentionIG, MentionEvent{MediaID: fixture.lookup.ID})
	require.NoError(err)
	assert.Equal(1, summary.MessagesUpdated)
	checkRun("instagram_mentions_webhook")
	summary, err = importer.Resolve(t.Context(), "9", "100", mentionIG, MentionEvent{MediaID: fixture.lookup.ID})
	require.NoError(err)
	assert.Zero(summary.MessagesUpdated, "an identical replay must not invalidate cached message facts")
}

func TestMentionImportTagsFullAndFailure(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	for _, status := range []int{400, 403} {
		st, f, imp, src := setupMentions(t)
		_, err := imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{MaxRequests: 3})
		requirements.NoError(err)
		before, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, "instagram_mentions")
		requirements.NoError(err)
		f.status = status
		_, err = imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
		requirements.ErrorIs(err, ErrMentionsRefused)
		var provider *APIError
		requirements.ErrorAs(err, &provider)
		assertions.Equal(status, provider.Status)
		after, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, "instagram_mentions")
		requirements.NoError(err)
		assertions.Equal(before.CursorBefore.String, after.CursorBefore.String)
		assertions.EqualValues(1, after.ErrorsCount)
		f.status = 0
		_, err = imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{Full: true})
		requirements.NoError(err)
		assertions.Equal(int32(2), f.freshReads.Load())
	}
}

func TestMentionResolveOwnershipAndMediaMismatch(t *testing.T) {
	for _, mode := range []string{"caption", "comment", "own", "foreign owner", "wrong media"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st, f, imp, src := setupMentions(t)
			ev := MentionEvent{MediaID: mentionMedia}
			if mode != "caption" {
				ev.CommentID = mentionComment
			}
			if mode == "own" {
				f.lookup.Owner = &IGUser{ID: mentionIG}
			}
			if mode == "foreign owner" {
				f.lookup.Owner = &IGUser{ID: "999"}
			}
			if mode == "wrong media" {
				f.lookup.ID = "999"
			}
			sum, err := imp.Resolve(t.Context(), "9", "100", mentionIG, ev)
			if mode == "wrong media" {
				requirements.Error(err)
			} else {
				requirements.NoError(err)
			}
			assertions.Equal(3, sum.Requests)
			var count int
			requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
			want := map[string]int{"caption": 1, "comment": 2, "foreign owner": 2}[mode]
			assertions.Equal(want, count)
		})
	}
}

func TestMentionResolveLockBusyAndRetired(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, f, imp, src := setupMentions(t)
	lock, err := st.AcquireSyncExecutionContext(t.Context(), src.ID)
	requirements.NoError(err)
	_, err = imp.Resolve(t.Context(), "9", "100", mentionIG, MentionEvent{MediaID: mentionMedia})
	requirements.ErrorIs(err, store.ErrSyncAlreadyActive)
	assertions.Zero(f.calls.Load())
	requirements.NoError(lock.Release())
	other, err := st.GetOrCreateSource("instagram_mentions", InstagramSourceIdentifier("99", "100", mentionIG))
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: src.ID, IntoSourceID: other.ID})
	requirements.NoError(err)
	_, err = imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	requirements.ErrorIs(err, store.ErrSourceRetired)
	assertions.Zero(f.calls.Load())
	_, err = imp.Resolve(t.Context(), "10", "100", mentionIG, MentionEvent{MediaID: mentionMedia})
	requirements.Error(err)
	assertions.Zero(f.calls.Load())
}

type yieldingMentionAPI struct {
	MentionAPI

	cancel context.CancelCauseFunc
}

func (a yieldingMentionAPI) Tags(ctx context.Context, ig, after string) (MediaPage, error) {
	p, err := a.MentionAPI.Tags(ctx, ig, after)
	if after != "" {
		a.cancel(jobctx.ErrYieldedToWaiter)
		return MediaPage{}, ctx.Err()
	}
	return p, err
}

func TestMentionImportSchedulerYieldKeepsProgress(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, _, imp, src := setupMentions(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	imp.api = yieldingMentionAPI{MentionAPI: imp.api, cancel: cancel}
	sum, err := imp.ImportTags(ctx, "9", "100", mentionIG, ImportOptions{})
	requirements.ErrorIs(err, context.Canceled)
	assertions.Equal(2, sum.MessagesAdded)
	run, err := st.GetLatestCheckpointedSyncByTypeContext(t.Context(), src.ID, "instagram_mentions")
	requirements.NoError(err)
	assertions.Equal(store.SyncStatusCancelled, run.Status)
	assertions.Zero(run.ErrorsCount)
	var state mentionSyncState
	requirements.NoError(json.Unmarshal([]byte(run.CursorBefore.String), &state))
	assertions.Equal("2", state.After)
}

func TestMentionImportLeavesCommentsSourceUntouched(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, _, imp, _ := setupMentions(t)
	src, err := st.GetOrCreateSource("instagram_comments", InstagramSourceIdentifier("9", "100", mentionIG))
	requirements.NoError(err)
	id, err := st.StartSync(src.ID, "instagram_comments")
	requirements.NoError(err)
	requirements.NoError(st.CompleteSync(id, "comments-original-cursor"))
	_, err = imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	requirements.NoError(err)
	run, err := st.GetLastSuccessfulSyncContext(t.Context(), src.ID)
	requirements.NoError(err)
	assertions.Equal(id, run.ID)
	assertions.Equal("comments-original-cursor", run.CursorAfter.String)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT count(*) FROM messages WHERE source_id=?`, src.ID).Scan(&count))
	assertions.Zero(count)
}

func TestMentionImportCheckpointSourceMismatch(t *testing.T) {
	requirements := require.New(t)
	for _, blob := range []string{`{"Version":1,"Source":"foreign"}`, `{}`, `{"Version":2,"Source":"app:9:page:100:instagram:17841400000000001"}`} {
		st, f, imp, src := setupMentions(t)
		id, err := st.StartSync(src.ID, "instagram_mentions")
		requirements.NoError(err)
		requirements.NoError(st.PauseSyncWithCheckpoint(id, &store.Checkpoint{PageToken: blob}))
		_, err = imp.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
		requirements.ErrorContains(err, "invalid Instagram mentions checkpoint; restart this source with --full")
		assert.Zero(t, f.calls.Load())
	}
}

func TestMentionTagsForeignOwnerResumes(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, fixture, importer, source := setupMentions(t)
	fixture.media[0].Owner = &IGUser{ID: "999"}
	fixture.media[1].Owner = &IGUser{ID: mentionIG}
	first, err := importer.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{MaxRequests: 3})
	requirements.NoError(err)
	assertions.Equal(1, first.MessagesAdded)
	assertions.True(first.Partial)
	second, err := importer.ImportTags(t.Context(), "9", "100", mentionIG, ImportOptions{})
	requirements.NoError(err)
	assertions.Equal(3, second.MessagesAdded)
	assertions.False(second.Partial)
	foreign := message(t, st, source.ID, fixture.media[0].ID)
	assertions.False(foreign.SenderID.Valid)
	assertions.False(foreign.IsFromMe)
	var participants, own int
	requirements.NoError(st.DB().QueryRow("SELECT count(*) FROM participants").Scan(&participants))
	assertions.Zero(participants)
	requirements.NoError(st.DB().QueryRow("SELECT count(*) FROM messages WHERE source_id=? AND source_message_id=?", source.ID, fixture.media[1].ID).Scan(&own))
	assertions.Zero(own)
}
