package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/youtube"
	"hegel.dev/go/hegel"
)

func captionBudgetStore(t *testing.T, count int) (*store.Store, int64, []int64, config.YouTubeSource, *config.Config) {
	t.Helper()
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCexample")
	requirements.NoError(err)
	var roots []int64
	for i := range count {
		video := fmt.Sprintf("video-%d", i)
		conversation, err := f.Store.EnsureConversation(source.ID, video, "Example video")
		requirements.NoError(err)
		root, err := f.Store.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "video:" + video, MessageType: "post"})
		requirements.NoError(err)
		roots = append(roots, root)
	}
	profile := config.YouTubeSource{Identifier: "fixture", ChannelID: "UCexample", Captions: true, CaptionUnits: 50}
	return f.Store, source.ID, roots, profile, &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}}
}

// A normal budget pause must not become a failed manual or scheduled sync.
// The next invocation must still consume the saved pending download.
func TestYouTubeCaptionBudgetPauseResumesWithoutFailure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, sourceID, roots, profile, cfg := captionBudgetStore(t, 2)
	summary, err := runYouTubeCaptionSync(t.Context(), st, mirrorCaptionFixture{}, profile, cfg)
	requirements.NoError(err)
	assertions.True(summary.Partial)
	assertions.Equal(50, summary.Units)
	refs, err := st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[0])
	requirements.NoError(err)
	assertions.Empty(refs)
	cursor, err := st.YouTubeCaptionStateContext(t.Context(), sourceID)
	requirements.NoError(err)
	assertions.NotEmpty(cursor)

	profile.CaptionUnits = 200 // Resume the download without another list charge.
	summary, err = runYouTubeCaptionSync(t.Context(), st, mirrorCaptionFixture{}, profile, cfg)
	requirements.NoError(err)
	assertions.True(summary.Partial)
	assertions.Equal(1, summary.Stored)
	refs, err = st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[0])
	requirements.NoError(err)
	assertions.Len(refs, 1)
	refs, err = st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[1])
	requirements.NoError(err)
	assertions.Empty(refs)

	profile.CaptionUnits = 250
	summary, err = runYouTubeCaptionSync(t.Context(), st, mirrorCaptionFixture{}, profile, cfg)
	requirements.NoError(err)
	assertions.False(summary.Partial)
	for _, root := range roots {
		refs, err := st.MessageYouTubeCaptionAttachmentsContext(t.Context(), root)
		requirements.NoError(err)
		assertions.Len(refs, 1)
	}
}

func TestYouTubeCaptionInsufficientBudgetProperty(t *testing.T) {
	st, sourceID, roots, profile, cfg := captionBudgetStore(t, 1)
	hegel.Test(t, func(ht *hegel.T) {
		profile.CaptionUnits = hegel.Draw(ht, hegel.Integers(1, 249))
		previous, err := st.YouTubeCaptionStateContext(ht.Context(), sourceID)
		require.NoError(ht, err)
		require.NoError(ht, st.CompareAndSwapYouTubeCaptionStateContext(ht.Context(), sourceID, previous, ""))
		summary, err := runYouTubeCaptionSync(ht.Context(), st, mirrorCaptionFixture{}, profile, cfg)
		require.NoError(ht, err)
		assert.True(ht, summary.Partial)
		assert.Zero(ht, summary.Stored)
		refs, err := st.MessageYouTubeCaptionAttachmentsContext(ht.Context(), roots[0])
		require.NoError(ht, err)
		assert.Empty(ht, refs, "an insufficient budget must not create a transcript")
	})
}

type failingCaptionFixture struct{ err error }

func (f failingCaptionFixture) ListCaptionTracks(context.Context, string) ([]youtube.CaptionTrack, error) {
	return nil, f.err
}

func (f failingCaptionFixture) DownloadCaption(context.Context, string) ([]byte, error) {
	return nil, f.err
}

func TestYouTubeCaptionBudgetPausePreservesRealFailures(t *testing.T) {
	requirements := require.New(t)
	st, _, _, profile, cfg := captionBudgetStore(t, 1)
	profile.CaptionUnits = 250
	apiErr := errors.New("synthetic caption provider failure")
	_, err := runYouTubeCaptionSync(t.Context(), st, failingCaptionFixture{err: apiErr}, profile, cfg)
	requirements.ErrorIs(err, apiErr)
	requirements.NoError(st.Close())
	_, err = runYouTubeCaptionSync(t.Context(), st, mirrorCaptionFixture{}, profile, cfg)
	requirements.Error(err)
}

func TestYouTubeCaptionSingleUnitPauseWithoutFailure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, _, _, profile, cfg := captionBudgetStore(t, 1)
	profile.CaptionUnits = 1 // The minimal counterexample shrunk by Hegel.
	summary, err := runYouTubeCaptionSync(t.Context(), st, mirrorCaptionFixture{}, profile, cfg)
	requirements.NoError(err)
	assertions.True(summary.Partial)
	assertions.Zero(summary.Units)
}

func TestYouTubeProfileReportsCaptionPartialWithoutFailure(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st, _, roots, profile, cfg := captionBudgetStore(t, 1)
	profile.ClientIDEnv, profile.ClientSecretEnv, profile.TokenEnv = "FIXTURE_YT_CLIENT", "FIXTURE_YT_SECRET", "FIXTURE_YT_REFRESH"
	t.Setenv(profile.ClientIDEnv, "synthetic-client")
	t.Setenv(profile.ClientSecretEnv, "synthetic-secret")
	t.Setenv(profile.TokenEnv, "synthetic-refresh")
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	// Intercept the external HTTP boundary only; both importer and caption hook
	// run unchanged against the real synthetic Store. No request leaves the test.
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.Method + " " + req.URL.Host + req.URL.Path {
		case "POST oauth2.googleapis.com/token":
			body = `{"access_token":"synthetic-access","expires_in":3600}`
		case "GET www.googleapis.com/youtube/v3/channels":
			body = `{"items":[{"id":"UCexample","snippet":{"title":"Example channel"},"contentDetails":{"relatedPlaylists":{"uploads":"UUexample"}}}]}`
		case "GET www.googleapis.com/youtube/v3/playlistItems":
			body = `{"items":[]}`
		case "GET www.googleapis.com/youtube/v3/captions":
			body = `{"items":[{"id":"track-a","etag":"v1","snippet":{"trackKind":"standard","status":"serving"}}]}`
		case "GET www.googleapis.com/youtube/v3/captions/track-a":
			body = "WEBVTT\n\n00:00.000 --> 00:01.000\nExample transcript\n"
		default:
			return nil, fmt.Errorf("unexpected synthetic request: %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	summary, err := syncYouTubeProfile(t.Context(), st, profile, cfg, 1000)
	requirements.NoError(err)
	requirements.NotNil(summary)
	assertions.True(summary.Partial, "CLI output must report a saved caption backlog")
	profile.CaptionUnits = 200
	summary, err = syncYouTubeProfile(t.Context(), st, profile, cfg, 1000)
	requirements.NoError(err)
	requirements.NotNil(summary)
	assertions.False(summary.Partial, "a completed caption queue must clear partial status")
	refs, err := st.MessageYouTubeCaptionAttachmentsContext(t.Context(), roots[0])
	requirements.NoError(err)
	assertions.Len(refs, 1)
}
