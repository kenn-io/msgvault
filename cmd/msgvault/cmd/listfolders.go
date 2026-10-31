package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newListFoldersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-folders [name]",
		Short: "List IMAP folders (mailboxes) available for an account",
		Example: `  msgvault source imap folders you@example.com
  msgvault sync you@example.com --folder INBOX`,
		Long: `List all IMAP folders (mailboxes) available for one or all
IMAP accounts, with message counts. Use these folder names with --folder or
--skip-folder on msgvault sync or msgvault sync full.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			return runListFoldersLocal(cmd, args)
		},
	}
	return cmd
}

func runListFoldersLocal(cmd *cobra.Command, args []string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	cfg := state.cfg
	logger := state.logger
	s, cleanup, err := openWritableStoreAndInitForInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := cmd.Context()

	var sources []*store.Source
	if len(args) == 1 {
		matches, err := s.GetSourcesByIdentifierOrDisplayName(args[0])
		if err != nil {
			return fmt.Errorf("lookup source: %w", err)
		}
		for _, src := range matches {
			if src.SourceType == sourceTypeIMAP {
				sources = append(sources, src)
			}
		}
		if len(sources) == 0 && len(matches) > 0 {
			return withHint(fmt.Errorf("account %q is not an IMAP source (type: %s)", args[0], matches[0].SourceType), "run 'msgvault account list --json' and choose an IMAP account")
		}
	} else {
		all, err := s.ListSources(sourceTypeIMAP)
		if err != nil {
			return fmt.Errorf("list IMAP sources: %w", err)
		}
		sources = all
	}
	if len(sources) == 0 {
		return sourceNotConfigured("imap", errors.New("no IMAP accounts configured"))
	}

	for i, src := range sources {
		if i > 0 {
			fmt.Println()
		}
		if err := listFolders(ctx, src, cfg, logger); err != nil {
			return err
		}
	}
	return nil
}

func listFolders(ctx context.Context, src *store.Source, cfg *config.Config, logger *slog.Logger) error {
	if cfg == nil {
		return errors.New("configuration not loaded")
	}
	displayID := src.Identifier
	if src.DisplayName.Valid && src.DisplayName.String != "" {
		displayID = src.DisplayName.String
	}
	fmt.Printf("Account: %s\n", displayID)

	skip, err := imapSkipReason(src, cfg, logger)
	if err != nil {
		return err
	}
	if skip != "" {
		fmt.Println(skip)
		return nil
	}

	imapCfg, err := imapclient.ConfigFromJSON(src.SyncConfig.String)
	if err != nil {
		return fmt.Errorf("malformed IMAP config: %w", err)
	}

	var password string
	var tokenFn func(ctx context.Context) (string, error)
	msClientID := cfg.Microsoft.ClientID
	msTenant := cfg.Microsoft.EffectiveTenantID()
	msTokensDir := cfg.TokensDir()

	switch imapCfg.EffectiveAuthMethod() {
	case imapclient.AuthXOAuth2:
		if msClientID == "" {
			fmt.Println("  Microsoft OAuth not configured — add [microsoft] section to config.toml")
			return errors.New("no Microsoft client ID configured")
		}
		msRedirectURI := cfg.Microsoft.EffectiveRedirectURI()
		msMgr := microsoft.NewManager(msClientID, msTenant, msRedirectURI, msTokensDir, logger)
		if !msMgr.HasToken(imapCfg.Username) {
			fmt.Printf("  Microsoft token not found — run 'msgvault source o365 add %s' first\n", imapCfg.Username)
			return fmt.Errorf("no Microsoft token for %s", imapCfg.Username)
		}
		tokenFn, err = msMgr.TokenSource(ctx, imapCfg.Username)
		if err != nil {
			fmt.Printf("  Failed to load token: %v (run 'msgvault source o365 add %s' first)\n", err, imapCfg.Username)
			return fmt.Errorf("load Microsoft token: %w", err)
		}
	default:
		password, err = imapclient.LoadCredentials(cfg.TokensDir(), src.Identifier)
		if err != nil {
			fmt.Printf("  Credentials not found — run 'msgvault source imap add --host %s --username %s' first\n", imapCfg.Host, imapCfg.Username)
			return fmt.Errorf("load credentials: %w", err)
		}
	}

	folders, err := imapclient.NewClient(imapCfg, password, imapclient.WithTokenSource(tokenFn)).ListMailboxes(ctx)
	if err != nil {
		return fmt.Errorf("list mailboxes: %w", err)
	}

	fmt.Printf("\n  %-35s %10s\n", "Folder", "Messages")
	fmt.Println("  " + strings.Repeat("-", 46))
	for _, f := range folders {
		if f.NumMessages == -1 {
			fmt.Printf("  %-35s %10s\n", textutil.SanitizeTerminal(f.Mailbox), "??")
		} else {
			fmt.Printf("  %-35s %10d\n", textutil.SanitizeTerminal(f.Mailbox), f.NumMessages)
		}
	}
	fmt.Println()
	return nil
}

func init() {
	rootCmd.AddCommand(newListFoldersCmd())
}
