package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"golang.org/x/oauth2"
)

func runSyncIncrementalHTTP(cmd *cobra.Command, args []string) error {
	req := daemonclient.CLISyncRequest{}
	if len(args) == 1 {
		req.Email = args[0]
	}
	return runSyncHTTP(cmd, req)
}

func runSyncFullHTTP(cmd *cobra.Command, args []string) error {
	req := daemonclient.CLISyncRequest{
		Full:     true,
		Query:    syncQuery,
		NoResume: syncNoResume,
		Before:   syncBefore,
		After:    syncAfter,
		Limit:    syncLimit,
	}
	if len(args) == 1 {
		req.Email = args[0]
	}
	return runSyncHTTP(cmd, req)
}

func runSyncHTTP(cmd *cobra.Command, req daemonclient.CLISyncRequest) error {
	st, info, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := preflightReauth(cmd.Context(), buildSyncPreflight(st, info), req.Email); err != nil {
		return err
	}

	return st.RunCLISync(cmd.Context(), req, func(stream, data string) error {
		switch stream {
		case cliStreamStdout:
			if _, err := fmt.Fprint(cmd.OutOrStdout(), data); err != nil {
				return fmt.Errorf("write sync stdout: %w", err)
			}
		case cliStreamStderr:
			if _, err := fmt.Fprint(cmd.ErrOrStderr(), data); err != nil {
				return fmt.Errorf("write sync stderr: %w", err)
			}
		}
		return nil
	})
}

// preflightReauthManager is the subset of oauth.Manager the sync preflight
// needs. Keeping it an interface lets tests inject a fake without real OAuth.
type preflightReauthManager interface {
	HasToken(email string) bool
	TokenSource(ctx context.Context, email string) (oauth2.TokenSource, error)
	Authorize(ctx context.Context, email string) error
}

// preflightAccount is a single Gmail account the preflight may re-authorize.
type preflightAccount struct {
	Email    string
	OAuthApp string
}

// preflightConfig carries the seams the sync preflight depends on. The
// production wiring is built by buildSyncPreflight; tests supply fakes.
type preflightConfig struct {
	// Local is true only when the sync targets this machine's local daemon,
	// whose OAuth tokens share the local filesystem. Reauth is meaningless
	// against a remote daemon.
	Local bool
	// Interactive is true when stdin is a terminal (a browser flow can run).
	Interactive bool
	// OAuthConfigured mirrors cfg.OAuth.HasAnyConfig().
	OAuthConfigured bool
	Out             io.Writer
	// ListGmailAccounts enumerates the daemon's Gmail accounts.
	ListGmailAccounts func(ctx context.Context) ([]preflightAccount, error)
	// ServiceAccountKey returns a non-empty key path when the app uses a
	// service account (tokens minted on demand; no browser reauth needed).
	ServiceAccountKey func(appName string) string
	// ManagerFor resolves the OAuth manager for a given app name.
	ManagerFor func(appName string) (preflightReauthManager, error)
}

// buildSyncPreflight wires the production preflight config from global config,
// the daemon client, and the resolved store endpoint.
func buildSyncPreflight(st *daemonclient.Client, info HTTPStoreInfo) preflightConfig {
	getMgr := oauthManagerCache()
	return preflightConfig{
		Local: info.Kind == HTTPStoreLocalDaemon,
		Interactive: isatty.IsTerminal(os.Stdin.Fd()) ||
			isatty.IsCygwinTerminal(os.Stdin.Fd()),
		OAuthConfigured: cfg.OAuth.HasAnyConfig(),
		Out:             os.Stdout,
		ListGmailAccounts: func(ctx context.Context) ([]preflightAccount, error) {
			accounts, err := st.GetCLIAccounts(ctx)
			if err != nil {
				return nil, err
			}
			gmail := make([]preflightAccount, 0, len(accounts))
			for _, a := range accounts {
				if a.Type != sourceTypeGmail {
					continue
				}
				gmail = append(gmail, preflightAccount{Email: a.Email, OAuthApp: a.OAuthApp})
			}
			return gmail, nil
		},
		ServiceAccountKey: cfg.OAuth.ServiceAccountKeyFor,
		ManagerFor: func(appName string) (preflightReauthManager, error) {
			mgr, err := getMgr(appName)
			if err != nil {
				return nil, err
			}
			return mgr, nil
		},
	}
}

// preflightReauth re-authorizes expired Gmail tokens on the local filesystem
// before the sync request is proxied to the daemon's non-TTY CLI subprocess,
// which cannot open a browser itself. It is strictly best-effort: it only
// re-authorizes tokens that already exist and have expired/been revoked, and
// it never enrolls a new account (that is add-account's job).
func preflightReauth(ctx context.Context, p preflightConfig, reqEmail string) error {
	if !p.Local || !p.Interactive || !p.OAuthConfigured {
		return nil
	}

	targets, err := p.ListGmailAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list accounts for reauth preflight: %w", err)
	}
	if reqEmail != "" {
		targets = filterPreflightAccounts(targets, reqEmail)
	}

	for _, target := range targets {
		if err := preflightReauthAccount(ctx, p, target); err != nil {
			return err
		}
	}
	return nil
}

// filterPreflightAccounts keeps only the account whose email matches the
// requested identifier (case-insensitive). A request for an IMAP or unknown
// account yields no Gmail targets, so the preflight skips it.
func filterPreflightAccounts(accounts []preflightAccount, reqEmail string) []preflightAccount {
	for _, a := range accounts {
		if strings.EqualFold(a.Email, reqEmail) {
			return []preflightAccount{a}
		}
	}
	return nil
}

func preflightReauthAccount(ctx context.Context, p preflightConfig, target preflightAccount) error {
	// Service accounts mint tokens on demand; there is nothing to re-authorize.
	if p.ServiceAccountKey(target.OAuthApp) != "" {
		return nil
	}

	mgr, err := p.ManagerFor(target.OAuthApp)
	if err != nil {
		// A manager build failure is a config problem the daemon reports with
		// its own skip line; don't abort the whole sync over it.
		return nil //nolint:nilerr // best-effort: config errors surface via the daemon
	}

	// A bare sync must not newly enroll an account.
	if !mgr.HasToken(target.Email) {
		return nil
	}

	_, err = mgr.TokenSource(ctx, target.Email)
	if err == nil {
		return nil
	}
	// Only an expired/revoked token warrants reauth; transient errors
	// (network, cancellation) must not trigger a browser flow.
	if !isAuthInvalidError(err) {
		return nil
	}

	if p.Out != nil {
		_, _ = fmt.Fprintf(p.Out,
			"Token for %s is expired; opening browser to re-authorize...\n",
			target.Email)
	}
	if err := mgr.Authorize(ctx, target.Email); err != nil {
		return fmt.Errorf("re-authorize %s: %w", target.Email, err)
	}
	return nil
}
