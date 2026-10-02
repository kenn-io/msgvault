package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/clirun"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/inline"
	"go.kenn.io/msgvault/internal/store"
)

const inlineMCPEndpoint = config.DefaultInlineMCPEndpoint

func newAddInlineCmd() *cobra.Command {
	var chatIDs []string
	var transport, cliPath string
	var noDefaultIdentity bool
	cmd := &cobra.Command{
		Use:   "add-inline",
		Short: "Authorize Inline to archive all accessible chats",
		Long: `Authorize Inline with read-only OAuth, or use an authenticated Inline CLI
on the daemon host. All accessible chats are included by default, including
child threads. Optional --chat-id flags restrict capture to those chats.
Re-running with IDs merges an existing filter; omitting IDs restores all chats.

Examples:
  msgvault add-inline
  msgvault add-inline --transport cli
  msgvault add-inline --chat-id 123 --chat-id 456`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			ids, err := parseInlineChatIDs(chatIDs)
			if err != nil {
				return usageErr(cmd, err)
			}
			if transport != "mcp" && transport != "cli" {
				return usageErr(cmd, errors.New("--transport must be mcp or cli"))
			}
			if transport != "cli" && cliPath != "" {
				return usageErr(cmd, errors.New("--cli-path requires --transport cli"))
			}
			mgr := inline.NewOAuthManager(inlineMCPEndpoint, state.cfg.TokensDir(), state.logger)
			if !isDaemonCLISubprocess() {
				if transport == "cli" {
					return runDaemonCLICommandHTTPFromCobra(cmd, args)
				}
				payload, err := mgr.AuthorizePayload(cmd.Context())
				if err != nil {
					return fmt.Errorf("authorize Inline: %w", err)
				}
				// Browser consent belongs to this process; only the daemon persists
				// credentials and archive/config state. Never place tokens in argv.
				return runDaemonCLICommandHTTPFromCobraWithEnv(cmd, args, map[string]string{clirun.EnvInlineOAuth: string(payload)})
			}
			var client inline.Client
			payload := []byte(os.Getenv(clirun.EnvInlineOAuth))
			if transport == "cli" {
				client = inline.NewCLIClient(cliPath)
			} else {
				if len(payload) == 0 {
					return errors.New("missing Inline OAuth handoff; run add-inline from the CLI")
				}
				httpClient, err := mgr.HTTPClientFromPayload(cmd.Context(), payload)
				if err != nil {
					return err
				}
				client, err = inline.NewMCPClient(cmd.Context(), inlineMCPEndpoint, httpClient)
				if err != nil {
					return err
				}
			}
			defer func() { _ = client.Close() }()
			account, err := verifyInlineSelection(cmd.Context(), client, ids)
			if err != nil {
				return err
			}
			canonicalIDs := make([]string, len(ids))
			for index, id := range ids {
				canonicalIDs[index] = strconv.FormatInt(id, 10)
			}
			entry := config.InlineAccount{Identifier: account.Identifier(), Transport: transport, ChatIDs: canonicalIDs, CLIPath: cliPath}
			snapshot, err := config.ReadConfigFile(state.cfg.ConfigFilePath())
			if err != nil {
				return err
			}
			if transport == "mcp" {
				if err := mgr.ImportCredentials(account.Identifier(), payload); err != nil {
					return fmt.Errorf("save Inline credentials: %w", err)
				}
			}
			if _, err := config.EditInlineAccount(state.cfg.ConfigFilePath(), snapshot.ETag, entry); err != nil {
				return fmt.Errorf("save Inline selection: %w", err)
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			src, err := s.GetOrCreateSource(sourceTypeInline, account.Identifier())
			if err != nil {
				return err
			}
			if err := s.UpdateSourceDisplayName(src.ID, "Inline"); err != nil {
				return err
			}
			if !noDefaultIdentity {
				confirmDefaultIdentity(cmd.OutOrStdout(), s, src.ID, account.Identifier(), account.Identifier(), "account-identifier", state.logger)
			}
			if err := runPostSourceCreateMigrationsForInvocation(s, state); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Added Inline account %s. Run msgvault sync-inline %s. Restart the daemon after changing scheduled accounts.\n", account.Identifier(), account.Identifier()); err != nil {
				return fmt.Errorf("write Inline account confirmation: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&chatIDs, "chat-id", nil, "canonical chat ID to archive (repeat for each selected chat)")
	cmd.Flags().StringVar(&transport, "transport", "mcp", "read transport: mcp (OAuth) or cli (daemon-host Inline CLI)")
	cmd.Flags().StringVar(&cliPath, "cli-path", "", "Inline executable on the daemon host (default: inline)")
	cmd.Flags().BoolVar(&noDefaultIdentity, "no-default-identity", false, noDefaultIdentityHelp)
	return cmd
}

func parseInlineChatIDs(values []string) ([]int64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(values))
	seen := make(map[int64]bool)
	for _, value := range values {
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 || id > inline.MaxID || strconv.FormatInt(id, 10) != value {
			return nil, errors.New("chat IDs must be positive canonical decimal integers within Inline's supported range")
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}

func verifyInlineSelection(ctx context.Context, client inline.Client, ids []int64) (inline.Account, error) {
	account, err := client.Me(ctx)
	if err != nil {
		return inline.Account{}, fmt.Errorf("verify Inline account: %w", err)
	}
	if account.UserID <= 0 || account.Origin != inline.ProductionOrigin {
		return inline.Account{}, errors.New("inline authentication does not identify a production account")
	}
	if len(ids) == 0 {
		if _, err := client.Discover(ctx); err != nil {
			return inline.Account{}, fmt.Errorf("verify complete Inline chat discovery: %w", err)
		}
		return account, nil
	}
	for _, id := range ids {
		chat, err := client.Conversation(ctx, id)
		if err != nil {
			return inline.Account{}, fmt.Errorf("verify selected Inline chat: %w", err)
		}
		if chat.ID != id {
			return inline.Account{}, errors.New("inline returned a different chat than requested")
		}
	}
	return account, nil
}

func connectInlineAccount(ctx context.Context, state *invocation, account config.InlineAccount) (inline.Client, error) {
	switch account.EffectiveTransport() {
	case "cli":
		return inline.NewCLIClient(account.CLIPath), nil
	case "mcp":
		mgr := inline.NewOAuthManager(account.EffectiveEndpoint(), state.cfg.TokensDir(), state.logger)
		httpClient, err := mgr.HTTPClient(ctx, account.Identifier)
		if err != nil {
			return nil, err
		}
		return inline.NewMCPClient(ctx, account.EffectiveEndpoint(), httpClient)
	default:
		return nil, errors.New("unsupported Inline transport")
	}
}

func resolveInlineAccounts(cfg *config.Config, args []string) ([]config.InlineAccount, error) {
	if len(args) > 0 {
		account := cfg.GetInlineAccount(args[0])
		if account == nil {
			return nil, errors.New("inline account is not configured; run add-inline")
		}
		return []config.InlineAccount{*account}, nil
	}
	if len(cfg.Inline.Accounts) == 0 {
		return nil, errors.New("no Inline accounts configured; run add-inline")
	}
	return cfg.Inline.Accounts, nil
}

func newSyncInlineCmd() *cobra.Command          { return newInlineReadCommand(false) }
func newBackfillInlineMediaCmd() *cobra.Command { return newInlineReadCommand(true) }

func newInlineReadCommand(backfill bool) *cobra.Command {
	var full, probe, noMedia bool
	var limit int
	use, short := "sync-inline [identifier]", "Archive Inline chats (all accessible chats by default)"
	if backfill {
		use, short = "backfill-inline-media [identifier]", "Retry eligible Inline attachment downloads"
	}
	cmd := &cobra.Command{
		Use: use, Short: short, Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			if limit < 0 {
				return usageErr(cmd, errors.New("--limit must not be negative"))
			}
			if probe && (full || noMedia || limit != 0) {
				return usageErr(cmd, errors.New("--probe cannot be combined with sync options"))
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			accounts, err := resolveInlineAccounts(state.cfg, args)
			if err != nil {
				return err
			}
			ctx, stop := withInterruptCancel(cmd, "Interrupted. Re-run to resume Inline sync.")
			defer stop()
			if probe {
				var errs []error
				for _, account := range accounts {
					if err := probeInlineAccount(ctx, cmd.OutOrStdout(), state, account); err != nil {
						errs = append(errs, err)
					}
				}
				return errors.Join(errs...)
			}
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			var errs []error
			for _, account := range accounts {
				if ctx.Err() != nil {
					break
				}
				sum, err := runInlineAccount(ctx, state, s, account, full, noMedia, backfill, limit)
				if sum != nil {
					writeInlineSummary(cmd.OutOrStdout(), account.Identifier, sum)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", account.Identifier, err))
				}
			}
			// Partial committed work must be visible even after cancellation or a
			// different account's failure. Cache work uses the existing daemon path.
			return errors.Join(ctx.Err(), errors.Join(errs...), rebuildCacheAfterManualSync(state.cfg.DatabaseDSN(), state))
		},
	}
	if !backfill {
		cmd.Long = "Discover all accessible chats and archive their history by default; chat_ids can restrict an account. Archive new messages and resume interrupted history capture. Use --full to refresh surviving messages, including older edits. Previously captured deletions remain in the archive."
		cmd.Flags().BoolVar(&full, "full", false, "start or resume a full refresh of selected chats")
		cmd.Flags().BoolVar(&probe, "probe", false, "verify account and selected-chat schemas without archive writes or printing message text")
		cmd.Flags().BoolVar(&noMedia, "no-media", false, "archive attachment metadata without downloading bytes")
		cmd.Flags().IntVar(&limit, "limit", 0, "messages of work per account this run across selected chats (0 = unlimited; interrupted work resumes)")
	}
	return cmd
}

func inlineImportOptions(cfg *config.Config, account config.InlineAccount) (inline.ImportOptions, error) {
	ids, err := parseInlineChatIDs(account.ChatIDs)
	if err != nil {
		return inline.ImportOptions{}, err
	}
	prefix := inline.ProductionOrigin + ":user:"
	if !strings.HasPrefix(account.Identifier, prefix) {
		return inline.ImportOptions{}, errors.New("invalid Inline account identifier")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(account.Identifier, prefix), 10, 64)
	if err != nil || id <= 0 || id > inline.MaxID || prefix+strconv.FormatInt(id, 10) != account.Identifier {
		return inline.ImportOptions{}, errors.New("invalid Inline account identifier")
	}
	return inline.ImportOptions{Account: inline.Account{UserID: id, Origin: inline.ProductionOrigin}, ChatIDs: ids, AttachmentsDir: cfg.AttachmentsDir(), MediaPolicy: cfg.Inline.MediaPolicy(account.Identifier)}, nil
}

func runInlineAccount(ctx context.Context, state *invocation, s *store.Store, account config.InlineAccount, full, noMedia, backfill bool, limit int) (*inline.ImportSummary, error) {
	registered, err := s.GetSourceByTypeAndIdentifier(sourceTypeInline, account.Identifier)
	if errors.Is(err, store.ErrSourceNotFound) || registered == nil && err == nil {
		return nil, errors.New("inline account is not registered; run add-inline")
	}
	if err != nil {
		return nil, err
	}
	opts, err := inlineImportOptions(state.cfg, account)
	if err != nil {
		return nil, err
	}
	opts.Full, opts.NoMedia, opts.Limit = full, noMedia, limit
	client, err := connectInlineAccount(ctx, state, account)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	imp := inline.NewImporter(s, client)
	if backfill {
		return imp.BackfillMedia(ctx, opts)
	}
	return imp.Import(ctx, opts)
}

func probeInlineAccount(ctx context.Context, out io.Writer, state *invocation, account config.InlineAccount) error {
	opts, err := inlineImportOptions(state.cfg, account)
	if err != nil {
		return err
	}
	client, err := connectInlineAccount(ctx, state, account)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return runInlineProbe(ctx, out, client, opts)
}

func runInlineProbe(ctx context.Context, out io.Writer, client inline.Client, opts inline.ImportOptions) error {
	account, err := client.Me(ctx)
	if err != nil {
		return err
	}
	if account.UserID <= 0 || account.Origin != inline.ProductionOrigin || account.Identifier() != opts.Account.Identifier() {
		return errors.New("authenticated Inline account differs from configuration")
	}
	if len(opts.ChatIDs) == 0 {
		chats, err := client.Discover(ctx)
		if err != nil {
			return fmt.Errorf("verify complete Inline chat discovery: %w", err)
		}
		for _, chat := range chats {
			opts.ChatIDs = append(opts.ChatIDs, chat.ID)
		}
		if _, err := fmt.Fprintf(out, "Complete Inline catalog: %d accessible chats\n", len(chats)); err != nil {
			return fmt.Errorf("write Inline catalog probe: %w", err)
		}
	}
	for _, id := range opts.ChatIDs {
		chat, err := client.Conversation(ctx, id)
		if err != nil {
			return err
		}
		if chat.ID != id {
			return errors.New("inline returned a different chat than requested")
		}
		page, err := client.Messages(ctx, id, 0)
		if err != nil {
			return fmt.Errorf("validate Inline message schema: %w", err)
		}
		if _, err := fmt.Fprintf(out, "Inline chat %d: schema accepted, %d first-page messages, more=%t\n", id, len(page.Messages), page.HasMore); err != nil {
			return fmt.Errorf("write Inline chat probe: %w", err)
		}
	}
	return nil
}

func writeInlineSummary(out io.Writer, identifier string, sum *inline.ImportSummary) {
	_, _ = fmt.Fprintf(out, "%s: %d messages processed, %d added, %d updated; media %d stored, %d pending, %d skipped\n", identifier, sum.MessagesProcessed, sum.MessagesAdded, sum.MessagesUpdated, sum.AttachmentsDownloaded, sum.AttachmentsPending, sum.AttachmentsSkipped)
}

func runConfiguredInlineSync(ctx context.Context, s *store.Store) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	var errs []error
	for _, account := range state.cfg.Inline.Accounts {
		if ctx.Err() != nil {
			break
		}
		if _, err := runInlineAccount(ctx, state, s, account, false, false, false, 0); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", account.Identifier, err))
		}
	}
	return errors.Join(ctx.Err(), errors.Join(errs...), rebuildCacheAfterScheduledSync(context.WithoutCancel(ctx), "inline"))
}

func init() {
	rootCmd.AddCommand(newAddInlineCmd(), newSyncInlineCmd(), newBackfillInlineMediaCmd())
}
