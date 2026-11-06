package youtube

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"hegel.dev/go/hegel"
)

// captionFixtureAPI models the official list/download contract for cursor and
// budget tests; separate client tests exercise the real authenticated HTTP path.
type captionFixtureAPI struct {
	tracks               map[string][]CaptionTrack
	listCalls, downloads []string
	data                 map[string][]byte
}

func (a *captionFixtureAPI) ListCaptionTracks(_ context.Context, video string) ([]CaptionTrack, error) {
	a.listCalls = append(a.listCalls, video)
	return a.tracks[video], nil
}

func (a *captionFixtureAPI) DownloadCaption(_ context.Context, track string) ([]byte, error) {
	a.downloads = append(a.downloads, track)
	if data, ok := a.data[track]; ok {
		return data, nil
	}
	return []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nSynthetic transcript " + track + "\n"), nil
}

func captionStore(t *testing.T, n int) (*store.Store, CaptionOptions, []int64) {
	t.Helper()
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCexample")
	requirements.NoError(err)
	var roots []int64
	for i := range n {
		video := fmt.Sprintf("video-%d", i)
		conv, err := st.EnsureConversation(source.ID, video, "Example video")
		requirements.NoError(err)
		id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "video:" + video, MessageType: "post"})
		requirements.NoError(err)
		roots = append(roots, id)
	}
	return st, CaptionOptions{Enabled: true, SourceID: source.ID, AttachmentsDir: t.TempDir(), MaxUnits: 2000}, roots
}

func standardTrack(id, version string) CaptionTrack {
	return CaptionTrack{ID: id, ETag: version, Snippet: CaptionSnippet{TrackKind: "standard", Language: "en", Status: "serving"}}
}

func TestCaptionsOffDoesNothing(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, _ := captionStore(t, 1)
	opts.Enabled = false
	api := &captionFixtureAPI{}
	summary, err := SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Zero(summary.Units)
	assertions.Empty(api.listCalls)
	state, err := st.YouTubeCaptionStateContext(t.Context(), opts.SourceID)
	requirements.NoError(err)
	assertions.Empty(state)
}

func TestCaptionsStoreTextAndSkipUnchangedVersion(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, roots := captionStore(t, 1)
	api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{"video-0": {standardTrack("track-a", "v1")}}}
	summary, err := SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal(250, summary.Units)
	assertions.Equal(1, summary.Stored)
	var mime, path, role, provenance string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT mime_type,storage_path,attachment_role,role_source FROM attachments WHERE message_id=? AND attachment_state='stored'`), roots[0]).Scan(&mime, &path, &role, &provenance))
	assertions.Equal("text/plain", mime)
	assertions.Equal("standalone", role)
	assertions.Equal("importer_semantics", provenance)
	text, err := os.ReadFile(filepath.Join(opts.AttachmentsDir, path))
	requirements.NoError(err)
	assertions.Equal("Synthetic transcript track-a", string(text))
	_, err = SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal([]string{"track-a"}, api.downloads)
	api.tracks["video-0"] = []CaptionTrack{standardTrack("track-a", "v2")}
	_, err = SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Len(api.downloads, 2)
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND attachment_state='stored'`), roots[0]).Scan(&count))
	assertions.Equal(1, count)
}

func TestCaptionsASRAndMissingRevisited(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, roots := captionStore(t, 2)
	asr := standardTrack("auto-track", "v1")
	asr.Snippet.TrackKind = "ASR"
	api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{"video-0": {asr}}}
	summary, err := SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal(2, summary.Unavailable)
	assertions.Empty(api.downloads)
	refs, err := st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[0])
	requirements.NoError(err)
	assertions.Contains(refs, "youtube:caption:status", "ASR observation regression: available through the public reader")
	for _, root := range roots {
		var metadata string
		requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT attachment_metadata FROM attachments WHERE message_id=? AND attachment_state='unavailable'`), root).Scan(&metadata))
		var observation map[string]any
		requirements.NoError(json.Unmarshal([]byte(metadata), &observation))
		assertions.NotEmpty(observation["observed_at"])
	}
	api.tracks["video-1"] = []CaptionTrack{standardTrack("later-track", "v1")}
	summary, err = SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal(1, summary.Stored)
	assertions.Equal([]string{"later-track"}, api.downloads)
	var stale int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND source_part_key='youtube:captions:status'`), roots[1]).Scan(&stale))
	assertions.Zero(stale, "a resolved transcript must clear its unavailable marker")
}

func TestCaptionPendingDownloadResumesWithoutRelisting(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, _ := captionStore(t, 2)
	api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{
		"video-0": {standardTrack("track-a", "v1")}, "video-1": {standardTrack("track-b", "v1")},
	}}
	opts.MaxUnits = 50
	first, err := SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.True(first.Partial)
	assertions.Empty(api.downloads)
	opts.MaxUnits = 200
	second, err := SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal(200, second.Units)
	assertions.Equal([]string{"video-0"}, api.listCalls)
	assertions.Equal([]string{"track-a"}, api.downloads)
	opts.MaxUnits = 250
	_, err = SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal([]string{"track-a", "track-b"}, api.downloads)
}

func TestCaptionBudgetProperty(t *testing.T) {
	st, opts, _ := captionStore(t, 1)
	iteration := 0
	hegel.Test(t, func(ht *hegel.T) {
		iteration++
		budget := hegel.Draw(ht, hegel.OneOf[int](hegel.Integers(0, int(^uint(0)>>1)), hegel.Integers(0, 300)))
		opts.MaxUnits = budget
		api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{"video-0": {standardTrack("track-a", strconv.Itoa(iteration))}}}
		previous, err := st.YouTubeCaptionStateContext(ht.Context(), opts.SourceID)
		require.NoError(ht, err)
		require.NoError(ht, st.CompareAndSwapYouTubeCaptionStateContext(ht.Context(), opts.SourceID, previous, `{}`))
		summary, err := SyncCaptions(ht.Context(), api, st, opts)
		require.NoError(ht, err)
		assert.LessOrEqual(ht, summary.Units, budget)
		assert.Equal(ht, 50*len(api.listCalls)+200*len(api.downloads), summary.Units)
		want := 0
		if budget >= 250 {
			want = 250
		} else if budget >= 50 {
			want = 50
		}
		assert.Equal(ht, want, summary.Units)
	})
}

func TestCaptionQueueMakesFairProgressAcrossRuns(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, _ := captionStore(t, 3)
	opts.MaxUnits = 250
	api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{
		"video-0": {standardTrack("track-a", "v1")},
		"video-1": {standardTrack("track-b", "v1")},
		"video-2": {standardTrack("track-c", "v1")},
	}}
	for range 3 {
		summary, err := SyncCaptions(t.Context(), api, st, opts)
		requirements.NoError(err)
		assertions.Equal(1, summary.Stored)
	}
	assertions.Equal([]string{"video-0", "video-1", "video-2"}, api.listCalls)
	assertions.Equal([]string{"track-a", "track-b", "track-c"}, api.downloads)
}

func TestMalformedCaptionDefersWithoutStarvingOtherRoots(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st, opts, roots := captionStore(t, 2)
	api := &captionFixtureAPI{tracks: map[string][]CaptionTrack{
		"video-0": {standardTrack("bad-track", "v1")}, "video-1": {standardTrack("good-track", "v1")},
	}, data: map[string][]byte{"bad-track": []byte("invalid VTT")}}
	summary, err := SyncCaptions(t.Context(), api, st, opts)
	requirements.Error(err)
	assertions.Equal(1, summary.Stored)
	assertions.Equal([]string{"bad-track", "good-track"}, api.downloads)
	var count int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=? AND attachment_state='stored'`), roots[1]).Scan(&count))
	assertions.Equal(1, count)
	delete(api.data, "bad-track")
	_, err = SyncCaptions(t.Context(), api, st, opts)
	requirements.NoError(err)
	assertions.Equal([]string{"bad-track", "good-track", "bad-track"}, api.downloads)
}
