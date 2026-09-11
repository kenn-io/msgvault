package cmd

import (
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/textutil"
)

func newAuthorizeGoogleCardDAVCmd() *cobra.Command {
	var app string
	var manual bool
	cmd := &cobra.Command{
		Use:   "authorize-google <email>",
		Short: "Authorize Google Contacts for CardDAV",
		Long:  "Authorize Google Contacts on this machine. Reuse a matching Google authorization when available, or store separate CardDAV credentials. Then select Google Contacts in CardDAV settings. For a remote daemon, copy the token to that host using the same OAuth client configuration.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := strings.ToLower(strings.TrimSpace(args[0]))
			address, err := mail.ParseAddress(email)
			if err != nil || address.Address != email {
				return usageErr(cmd, errors.New("invalid email address"))
			}
			secrets, err := cfg.OAuth.ClientSecretsFor(app)
			if err != nil {
				return err
			}
			mgr, err := carddav.NewGoogleOAuthManager(secrets, cfg.TokensDir(), app, email, logger)
			if err != nil {
				return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
			}
			if mgr.HasToken(email) && !mgr.HasScopeMetadata(email) {
				if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Warning: existing Google permissions are not recorded. This sign-in requests Contacts access; Gmail or Calendar may need separate reauthorization afterward."); err != nil {
					return fmt.Errorf("write authorization warning: %w", err)
				}
			}
			if manual {
				err = mgr.AuthorizeManualPreservingGrantedScopes(cmd.Context(), email)
			} else {
				err = mgr.AuthorizePreservingGrantedScopes(cmd.Context(), email)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Google Contacts authorized. Token saved to %s.\nSelect Google Contacts in Settings → CardDAV account, or run add-carddav --google with this email.\n", textutil.SanitizeTerminal(mgr.TokenPath(email)))
			if err != nil {
				return fmt.Errorf("write Google authorization result: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "oauth-app", "", "Named Google OAuth application")
	cmd.Flags().BoolVar(&manual, "no-browser", false, "Print the authorization URL without opening a browser")
	return cmd
}
