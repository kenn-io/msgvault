package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/youtube"
)

func applyYouTubeCaptionFlags(cmd *cobra.Command, profiles []config.YouTubeSource) ([]config.YouTubeSource, error) {
	enabled, err := cmd.Flags().GetBool("captions")
	if err != nil {
		return nil, fmt.Errorf("read captions flag: %w", err)
	}
	units, err := cmd.Flags().GetInt("caption-units")
	if err != nil {
		return nil, fmt.Errorf("read caption-units flag: %w", err)
	}
	if units <= 0 {
		return nil, errors.New("--caption-units must be positive")
	}
	out := slices.Clone(profiles)
	for i := range out {
		if cmd.Flags().Changed("captions") {
			out[i].Captions = enabled
		}
		if cmd.Flags().Changed("caption-units") {
			out[i].CaptionUnits = units
		}
	}
	return out, nil
}

func runYouTubeCaptionSync(ctx context.Context, st *store.Store, client youtube.CaptionAPI, profile config.YouTubeSource, cfg *config.Config) (youtube.CaptionSummary, error) {
	if !profile.Captions {
		return youtube.CaptionSummary{}, nil
	}
	source, err := st.GetSourceByTypeAndIdentifier(ctx, youtube.SourceType, youtube.SourceIdentifier(profile.ChannelID))
	if err != nil {
		return youtube.CaptionSummary{}, err
	}
	units := profile.CaptionUnits
	if units == 0 {
		units = youtube.DefaultCaptionUnits
	}
	return youtube.SyncCaptions(ctx, client, st, youtube.CaptionOptions{
		Enabled: true, SourceID: source.ID, AttachmentsDir: cfg.AttachmentsDir(), MaxUnits: units,
	})
}

func init() {
	youtubeCaptionSync = func(ctx context.Context, st *store.Store, client *youtube.Client, profile config.YouTubeSource, cfg *config.Config) (youtube.CaptionSummary, error) {
		return runYouTubeCaptionSync(ctx, st, client, profile, cfg)
	}
}
