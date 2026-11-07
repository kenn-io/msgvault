package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/docbanktest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/youtube"
)

type mirrorCaptionFixture struct{}

func (mirrorCaptionFixture) ListCaptionTracks(context.Context, string) ([]youtube.CaptionTrack, error) {
	return []youtube.CaptionTrack{{ID: "track-a", ETag: "version-1", Snippet: youtube.CaptionSnippet{TrackKind: "standard", Status: "serving"}}}, nil
}

func (mirrorCaptionFixture) DownloadCaption(context.Context, string) ([]byte, error) {
	return []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nExample transcript\n"), nil
}

func TestYouTubeLateCaptionUsesConsentedMirror(t *testing.T) {
	for _, mode := range []string{"enabled", "no-consent", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			src, err := f.Store.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCexample")
			requirements.NoError(err)
			conv, err := f.Store.EnsureConversation(t.Context(), src.ID, "video-example", "Example video")
			requirements.NoError(err)
			_, err = f.Store.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "video:video-example", MessageType: "post"})
			requirements.NoError(err)
			dir := t.TempDir()
			blobs, err := attachmentstore.New(store.NewPackCatalog(f.Store), dir)
			requirements.NoError(err)
			t.Cleanup(func() { assertions.NoError(blobs.Close()) })
			uploads := docbanktest.NewUploadHandler(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/api/v1/path" {
					_, _ = fmt.Fprint(w, `{"id":7,"kind":"dir"}`)
					return
				}
				uploads.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			t.Setenv(beeperMediaTestKeyEnv, "synthetic-key")
			cfg := config.DocbankIntegrationConfig{Enabled: mode != "disabled", AttachmentUploadConsent: mode != "no-consent", URL: server.URL, APIKeyEnv: beeperMediaTestKeyEnv}
			sched := scheduler.New(nil)
			t.Cleanup(func() { <-sched.Stop().Done() })
			requirements.NoError(configureDocbankAttachmentJob(t.Context(), sched, nil, f.Store, blobs, dir, cfg, nil))
			if mode == "enabled" {
				requirements.NoError(sched.TriggerJob(api.DocbankAttachmentMirrorJob)) // Finish initial empty scan.
			}
			summary, err := youtube.SyncCaptions(t.Context(), mirrorCaptionFixture{}, f.Store, youtube.CaptionOptions{Enabled: true, SourceID: src.ID, AttachmentsDir: dir, MaxUnits: 250})
			requirements.NoError(err)
			assertions.Equal(1, summary.Stored)
			if mode == "enabled" {
				requirements.NoError(sched.TriggerJob(api.DocbankAttachmentMirrorJob))
				assertions.Equal(int32(1), uploads.Uploads.Load())
			} else {
				assertions.False(sched.IsJobScheduled(api.DocbankAttachmentMirrorJob))
				assertions.Zero(requests.Load(), "caption sync must not contact Docbank without consent")
			}
		})
	}
}
