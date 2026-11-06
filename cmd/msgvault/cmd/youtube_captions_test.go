package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/youtube"
)

func TestYouTubeCaptionFlagsOverrideOnlyInvocation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cmd := newSyncYouTubeCmd()
	requirements.NotNil(cmd.Flags().Lookup("captions"))
	requirements.NotNil(cmd.Flags().Lookup("caption-units"))
	assertions.Equal("2000", cmd.Flags().Lookup("caption-units").DefValue)
	requirements.NoError(cmd.Flags().Parse([]string{"--captions", "--caption-units=250"}))
	profiles := []config.YouTubeSource{{Identifier: "fixture"}}
	got, err := applyYouTubeCaptionFlags(cmd, profiles)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.True(got[0].Captions)
	assertions.Equal(250, got[0].CaptionUnits)
	assertions.False(profiles[0].Captions)
	assertions.Zero(profiles[0].CaptionUnits)
}

func TestYouTubeCaptionFlagsRejectNegativeAndPermitDisable(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cmd := newSyncYouTubeCmd()
	requirements.NoError(cmd.Flags().Parse([]string{"--captions=false", "--caption-units=-1"}))
	_, err := applyYouTubeCaptionFlags(cmd, []config.YouTubeSource{{Captions: true}})
	requirements.Error(err)
	requirements.NoError(cmd.Flags().Set("caption-units", "50"))
	got, err := applyYouTubeCaptionFlags(cmd, []config.YouTubeSource{{Captions: true}})
	requirements.NoError(err)
	assertions.False(got[0].Captions)
}

func TestYouTubeCaptionHookOffNeedsNoStoreOrClient(t *testing.T) {
	requirements := require.New(t)
	summary, err := runYouTubeCaptionSync(t.Context(), nil, nil, config.YouTubeSource{}, nil)
	requirements.NoError(err)
	assert.Equal(t, youtube.CaptionSummary{}, summary)
}

func TestConfiguredCaptionDrainUsesScheduledProfile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	src, err := f.Store.GetOrCreateSource("youtube", "https://www.youtube.com/channel/UCexample")
	requirements.NoError(err)
	conv, err := f.Store.EnsureConversation(src.ID, "video-example", "Example video")
	requirements.NoError(err)
	root, err := f.Store.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "video:video-example", MessageType: "post"})
	requirements.NoError(err)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, YouTube: []config.YouTubeSource{{
		Identifier: "fixture", ChannelID: "UCexample", Enabled: true, Schedule: "*/30 * * * *", Captions: true, CaptionUnits: 250,
	}}}
	profiles := cfg.ScheduledYouTubeSources()
	requirements.Len(profiles, 1)
	summary, err := runYouTubeCaptionSync(t.Context(), f.Store, mirrorCaptionFixture{}, profiles[0], cfg)
	requirements.NoError(err)
	assertions.False(summary.Partial)
	refs, err := f.Store.MessageYouTubeCaptionAttachmentsContext(t.Context(), root)
	requirements.NoError(err)
	requirements.Len(refs, 1)
	assertions.Equal("text/plain", refs["youtube:caption:track:track-a"].MimeType)
}
