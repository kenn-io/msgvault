package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newListFoldersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list-folders [name]",
		Short: "List IMAP folders (mailboxes) available for an account",
		Long: `List all IMAP folders (mailboxes) available for one or all
IMAP accounts, along with message count. This helps you
choose which folders to include or exclude via --folders or
--skip-folders on the sync command.`,
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
	s, cleanup, err := openWritableStoreAndInit()
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
			return fmt.Errorf("account %q is not an IMAP source (type: %s)", args[0], matches[0].SourceType)
		}
	} else {
		all, err := s.ListSources(sourceTypeIMAP)
		if err != nil {
			return fmt.Errorf("list IMAP sources: %w", err)
		}
		sources = all
	}
	if len(sources) == 0 {
		return errors.New("no IMAP accounts configured")
	}

	for i, src := range sources {
		if i > 0 {
			fmt.Println()
		}
		if err := listFolders(ctx, src); err != nil {
			return err
		}
	}
	return nil
}

func listFolders(ctx context.Context, src *store.Source) error {
	if cfg == nil {
		return errors.New("configuration not loaded")
	}
	displayID := src.Identifier
	if src.DisplayName.Valid && src.DisplayName.String != "" {
		displayID = src.DisplayName.String
	}
	fmt.Printf("Account: %s\n", displayID)

	skip, err := imapSkipReason(src)
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
			fmt.Println("  Microsoft token not found — run 'add-o365' first")
			return fmt.Errorf("no Microsoft token for %s", imapCfg.Username)
		}
		tokenFn, err = msMgr.TokenSource(ctx, imapCfg.Username)
		if err != nil {
			fmt.Printf("  Failed to load token: %v (run 'add-o365' first)\n", err)
			return fmt.Errorf("load Microsoft token: %w", err)
		}
	default:
		password, err = imapclient.LoadCredentials(cfg.TokensDir(), src.Identifier)
		if err != nil {
			fmt.Println("  Credentials not found — run 'add-imap' first")
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
