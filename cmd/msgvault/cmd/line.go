package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/envutil"
	"go.kenn.io/msgvault/internal/line"
	"go.kenn.io/msgvault/internal/store"
)

// Tests supply an actual HTTP client pointed at the synthetic LINE API.
var lineClientFactory = func(channelID, secret string) *line.Client { return line.NewClient(channelID, secret) }

func configuredLineClient(source config.LineSource) (*line.Client, error) {
	secret := os.Getenv(source.SecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("LINE channel %s needs %s in the daemon environment", source.ChannelID, source.SecretEnv)
	}
	return lineClientFactory(source.ChannelID, secret), nil
}

func lineWebhookBase(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") || parsed.RawPath != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("--webhook-base-url must be an HTTPS origin with a host and no credentials, path, query or fragment")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}

func loadLineConfig(state *invocation) (*config.Config, error) {
	if state == nil || state.cfg == nil {
		return nil, errors.New("configuration is unavailable")
	}
	fresh, err := config.Load(state.cfg.ConfigFilePath(), state.cfg.HomeDir)
	if err != nil {
		return nil, fmt.Errorf("reload LINE configuration: %w", err)
	}
	return fresh, nil
}

func lineReceiverReady(sourceID int64, channelID, botID, secretEnv string) bool {
	if !isDaemonCLISubprocess() {
		return false
	}
	var active []line.ActiveChannel
	if err := json.Unmarshal([]byte(os.Getenv(lineActiveChannelsEnv)), &active); err != nil {
		return false
	}
	for _, record := range active {
		if record.ChannelID == channelID && record.SourceID == sourceID && record.BotUserID == botID && record.SecretEnv == secretEnv {
			return true
		}
	}
	return false
}

func newAddLineCmd() *cobra.Command {
	var channelID, secretEnv, baseURL string
	command := &cobra.Command{Use: "add-line", Short: "Add a LINE Official Account", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !config.ValidLineChannelID(channelID) {
				return usageErr(cmd, errors.New("--channel-id must be a canonical positive decimal uint64"))
			}
			if !envutil.ValidName(secretEnv) {
				return usageErr(cmd, errors.New("--secret-env must name an explicit environment variable"))
			}
			var base string
			if baseURL != "" {
				var err error
				base, err = lineWebhookBase(baseURL)
				if err != nil {
					return usageErr(cmd, err)
				}
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			state := invocationFromCommand(cmd)
			fresh, err := loadLineConfig(state)
			if err != nil {
				return err
			}
			client, err := configuredLineClient(config.LineSource{ChannelID: channelID, SecretEnv: secretEnv})
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			source, err := line.Register(cmd.Context(), st, client, channelID)
			if err != nil {
				return fmt.Errorf("register LINE channel: %w", err)
			}
			botID, err := st.LineBotUserIDContext(cmd.Context(), source.ID)
			if err != nil {
				return err
			}
			updated := false
			for index := range fresh.Line {
				if fresh.Line[index].ChannelID == channelID {
					fresh.Line[index].SecretEnv = secretEnv
					updated = true
					break
				}
			}
			if !updated {
				fresh.Line = append(fresh.Line, config.LineSource{ChannelID: channelID, SecretEnv: secretEnv, Enabled: true})
			}
			if err := fresh.Save(); err != nil {
				return fmt.Errorf("save LINE configuration: %w", err)
			}
			if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
				return fmt.Errorf("post-source-create migrations: %w", err)
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Registered LINE channel %s (source %d).\nWebhook route: /webhooks/line/%s\n", channelID, source.ID, channelID)
			_, _ = fmt.Fprintln(out, "Enable Use webhook in LINE Developers Console; Webhook redelivery is recommended.")
			if !lineReceiverReady(source.ID, channelID, botID, secretEnv) {
				_, _ = fmt.Fprintf(out, "Receiver is not active for this registration; restart the daemon/service with %s in its environment. Then repeat add-line to set and test the webhook.\n", secretEnv)
				return nil
			}
			if base == "" {
				_, _ = fmt.Fprintln(out, "Configure the public HTTPS webhook URL in LINE Developers Console and use Verify.")
				return nil
			}
			endpoint := base + "/webhooks/line/" + channelID
			if err := client.SetWebhook(cmd.Context(), endpoint); err != nil {
				return fmt.Errorf("set LINE webhook (registration saved): %w", err)
			}
			result, err := client.TestWebhook(cmd.Context(), endpoint)
			if err != nil {
				return fmt.Errorf("test LINE webhook (registration saved): %w", err)
			}
			if !result.Success {
				return errors.New("LINE webhook verification failed; registration saved; check receiver reachability and credentials")
			}
			_, _ = fmt.Fprintf(out, "LINE webhook verified: %s\n", endpoint)
			return nil
		}}
	command.Flags().StringVar(&channelID, "channel-id", "", "LINE Messaging API channel ID")
	command.Flags().StringVar(&secretEnv, "secret-env", "", "daemon environment variable holding the channel secret")
	command.Flags().StringVar(&baseURL, "webhook-base-url", "", "public HTTPS origin used to set and test the active receiver")
	_ = command.MarkFlagRequired("channel-id")
	_ = command.MarkFlagRequired("secret-env")
	return command
}

func lineSyncProfiles(cfg *config.Config, channelID string) ([]config.LineSource, error) {
	if channelID != "" {
		source := cfg.GetLineSource(channelID)
		if source == nil {
			return nil, fmt.Errorf("LINE channel %s is not configured; run add-line", channelID)
		}
		return []config.LineSource{*source}, nil
	}
	if len(cfg.Line) == 0 {
		return nil, errors.New("no LINE channels configured; run add-line")
	}
	// Manual drains include configured disabled channels, like Chatwoot; Enabled
	// controls receiving and automatic work, not access to an existing inbox.
	return cfg.Line, nil
}

func newSyncLineCmd() *cobra.Command {
	var channelID string
	command := &cobra.Command{Use: "sync-line", Short: "Process LINE inbox work", Long: "Drain due LINE webhook work, including configured disabled channels. This push source has no history API; Sync now never fetches past messages.", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("channel-id") && !config.ValidLineChannelID(channelID) {
				return usageErr(cmd, errors.New("--channel-id must be a canonical positive decimal uint64"))
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			state := invocationFromCommand(cmd)
			cfg, err := loadLineConfig(state)
			if err != nil {
				return err
			}
			profiles, err := lineSyncProfiles(cfg, channelID)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. LINE inbox work remains durable...")
			defer stop()
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Push source: processing due LINE inbox work; no message history is fetched.")
			var errs []error
			for _, profile := range profiles {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break
				}
				source, err := st.GetSourceByTypeAndIdentifier(line.SourceType, profile.ChannelID)
				if err == nil && source.MergedIntoSourceID != 0 {
					err = store.ErrSourceRetired
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("LINE channel %s: %w", profile.ChannelID, err))
					continue
				}
				client, err := configuredLineClient(profile)
				if err == nil {
					err = line.NewProcessor(st, client, source.ID, profile.ChannelID, cfg.AttachmentsDir()).Process(ctx)
				}
				_ = recordSyncAttempt(ctx, line.SourceType, profile.ChannelID, err)
				if err != nil {
					errs = append(errs, fmt.Errorf("LINE channel %s: %w", profile.ChannelID, err))
				}
				if ctx.Err() == nil {
					status, statusErr := st.LineInboxStatusContext(ctx, source.ID)
					if statusErr != nil {
						errs = append(errs, statusErr)
					} else {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "LINE %s: pending: %d, failed: %d\n", profile.ChannelID, status.Pending, status.Failed)
					}
				}
			}
			errs = append(errs, rebuildCacheAfterManualSync(cfg.DatabaseDSN(), state))
			return errors.Join(errs...)
		}}
	command.Flags().StringVar(&channelID, "channel-id", "", "restrict processing to one configured LINE channel")
	return command
}

func init() { rootCmd.AddCommand(newAddLineCmd(), addManualSyncCacheFlags(newSyncLineCmd())) }
