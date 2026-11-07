package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/youtube"
)

func youtubeReplyProfile(cfg *config.Config, channelID string) (config.YouTubeSource, error) {
	var found *config.YouTubeSource
	for _, profile := range cfg.YouTube {
		if profile.ChannelID != channelID {
			continue
		}
		if found != nil {
			return config.YouTubeSource{}, errors.New("multiple YouTube profiles configure this channel; select one credential profile in configuration")
		}
		selected := profile
		found = &selected
	}
	if found == nil {
		return config.YouTubeSource{}, errors.New("no YouTube profile configured for the archived channel")
	}
	return *found, nil
}

// Construct OAuth only when the service actually needs a provider request.
// Accepted/archived receipts can recover locally even after credentials expire.
type lazyYouTubeReplyProvider struct {
	profile config.YouTubeSource
	client  *youtube.Client
}

func (p *lazyYouTubeReplyProvider) get() (*youtube.Client, error) {
	if p.client != nil {
		return p.client, nil
	}
	client, err := configuredYouTubeClient(p.profile)
	if err == nil {
		p.client = client
	}
	return client, err
}
func (p *lazyYouTubeReplyProvider) CheckReply(ctx context.Context, target youtube.ReplyTarget) error {
	client, err := p.get()
	if err != nil {
		return err
	}
	return client.CheckReply(ctx, target)
}
func (p *lazyYouTubeReplyProvider) InsertReply(ctx context.Context, parent, body string) (youtube.Comment, error) {
	client, err := p.get()
	if err != nil {
		return youtube.Comment{}, err
	}
	return client.InsertReply(ctx, parent, body)
}
func (p *lazyYouTubeReplyProvider) GetReply(ctx context.Context, id string) (youtube.Comment, error) {
	client, err := p.get()
	if err != nil {
		return youtube.Comment{}, err
	}
	return client.GetReply(ctx, id)
}

func newYouTubeReplyCmd() *cobra.Command {
	var body, token, resolveComment string
	var dryRun bool
	cmd := &cobra.Command{Use: "reply <message-id>", Short: "Preview and confirm a public reply to an archived YouTube comment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageErr(cmd, errors.New("one archived message ID is required"))
		}
		messageID, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || messageID <= 0 {
			return usageErr(cmd, errors.New("message ID must be a positive integer"))
		}
		if strings.TrimSpace(body) == "" {
			return usageErr(cmd, errors.New("--body must be nonempty"))
		}
		if dryRun == (token != "") || (resolveComment != "" && (dryRun || token == "")) {
			return usageErr(cmd, errors.New("use --dry-run or --confirm <token>; --resolve-comment also requires --confirm"))
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		st, cleanup, err := openWritableStoreAndInitForIngestInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		target, err := youtube.ResolveReplyTarget(cmd.Context(), st, messageID)
		if err != nil {
			return err
		}
		profile, err := youtubeReplyProfile(state.cfg, target.ChannelID)
		if err != nil {
			return err
		}
		service := &youtube.ReplyService{Store: st}
		var receipt *youtube.ReplyReceipt
		if dryRun {
			receipt, err = service.Preview(cmd.Context(), target, body)
		} else {
			service.Provider = &lazyYouTubeReplyProvider{profile: profile}
			archivedTarget, readErr := st.GetMessageContext(cmd.Context(), messageID)
			if readErr != nil {
				return readErr
			}
			video := youtube.Video{ID: target.VideoID, Snippet: youtube.VideoSnippet{ChannelID: target.ChannelID, Title: archivedTarget.ConversationTitle}}
			service.Persist = func(ctx context.Context, comment youtube.Comment) (int64, error) {
				comment.ThreadID = target.ThreadID
				return youtube.PersistComment(ctx, st, target.SourceID, target.ChannelID, video, comment, comment.Snippet.ModerationStatus)
			}
			if resolveComment != "" {
				receipt, err = service.Resolve(cmd.Context(), target, body, token, resolveComment)
			} else {
				receipt, err = service.Confirm(cmd.Context(), target, body, token)
			}
			if receipt != nil && receipt.MessageID != 0 {
				err = errors.Join(err, rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
			}
		}
		if receipt != nil {
			encoded, writeErr := json.Marshal(receipt)
			if writeErr == nil {
				_, writeErr = fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			}
			if writeErr != nil {
				return errors.Join(err, fmt.Errorf("print reply receipt: %w", writeErr))
			}
		}
		return err
	}}
	cmd.Flags().StringVar(&body, "body", "", "exact public reply text")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "prepare a confirmation and show its parent and body without contacting YouTube")
	cmd.Flags().StringVar(&token, "confirm", "", "single-use --dry-run token; accepted replies only retry saving")
	cmd.Flags().StringVar(&resolveComment, "resolve-comment", "", "recover an uncertain reply using a matching YouTube comment ID; never sends")
	return cmd
}

func init() {
	parent := &cobra.Command{Use: "youtube", Short: "Work with archived YouTube comments"}
	parent.AddCommand(newYouTubeReplyCmd())
	rootCmd.AddCommand(parent)
}
