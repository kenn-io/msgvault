package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/businessreviews"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"golang.org/x/oauth2"
)

const businessManageScope = "https://www.googleapis.com/auth/business.manage"

var businessReviewScopes = []string{businessManageScope, oauth.ScopeUserinfoEmail}

type businessReviewsRunner func(context.Context, businessreviews.Request) (*businessreviews.Result, error)

func init() { rootCmd.AddCommand(newBusinessReviewsCmd(nil)) }
func newBusinessReviewsCmd(run businessReviewsRunner) *cobra.Command {
	root := &cobra.Command{Use: "business-reviews", Short: "Inspect live business reviews and confirm owner replies", Long: "Read live Google Business Profile reviews without archiving provider content.\nRequires your own approved Google project, a dedicated named OAuth app, and explicitly configured targets.\nEach reply needs an exact preview and a separate confirmation. Facebook remains unavailable."}
	root.PersistentFlags().Bool("json", false, "Accepted for compatibility; output is always JSON")
	for _, entry := range []struct {
		name, action, use, short string
		args                     int
	}{
		{"capabilities", "capabilities", "capabilities", "Show supported providers and configured targets", 0},
		{cmdUseList, cmdUseList, cmdUseList + " TARGET", "Read a fresh page of reviews", 1},
		{"get", "get", "get TARGET REVIEW_ID", "Read one current review", 2},
		{"preview-reply", "preview_reply", "preview-reply TARGET REVIEW_ID", "Read the exact reply from stdin and issue an expiring preview", 2},
		{"confirm-reply", "confirm_reply", "confirm-reply --confirmation-token TOKEN --confirmed", "Submit the exact owner-approved preview once", 0},
		{"receipt", "receipt", "receipt RECEIPT_ID", "Read an ephemeral content-free receipt without resubmitting", 1},
	} {
		var pageToken, confirmationToken string
		var pageSize int
		var confirmed bool
		command := &cobra.Command{Use: entry.use, Short: entry.short, Args: cobra.ExactArgs(entry.args)}
		command.RunE = func(cmd *cobra.Command, args []string) error {
			request := businessreviews.Request{Action: entry.action}
			switch entry.action {
			case cmdUseList:
				request.Target = args[0]
				request.PageSize = pageSize
				request.PageToken = pageToken
			case "get", "preview_reply":
				request.Target = args[0]
				request.ReviewID = args[1]
				if entry.action == "preview_reply" {
					data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4097))
					if err != nil {
						return businessreviews.ErrInvalid
					}
					request.Comment = string(data)
				}
			case "confirm_reply":
				request.ConfirmationToken = confirmationToken
				request.Confirmed = confirmed
			case "receipt":
				request.ReceiptID = args[0]
			}
			if err := request.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			runner := run
			if runner == nil {
				runner = func(ctx context.Context, request businessreviews.Request) (*businessreviews.Result, error) {
					client, _, err := OpenHTTPStore(ctx)
					if err != nil {
						return nil, err
					}
					defer func() { _ = client.Close() }()
					return client.ControlBusinessReviews(ctx, request)
				}
			}
			result, err := runner(cmd.Context(), request)
			if err != nil {
				return err
			}
			if result == nil {
				return errors.New("business review response unavailable")
			}
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true))
		}
		if entry.action == cmdUseList {
			command.Flags().IntVar(&pageSize, "page-size", 20, "Maximum reviews in this page (1-50)")
			command.Flags().StringVar(&pageToken, "page-token", "", "Opaque next-page token from the previous live result")
		}
		if entry.action == "confirm_reply" {
			command.Flags().StringVar(&confirmationToken, "confirmation-token", "", "Single-use token from the exact preview")
			command.Flags().BoolVar(&confirmed, "confirmed", false, "Confirm prior specific owner consent to the exact previewed reply")
		}
		root.AddCommand(command)
	}
	for _, action := range []string{"authorize", "disconnect"} {
		var headless bool
		command := &cobra.Command{Use: action + " TARGET", Short: map[string]string{"authorize": "Authorize the configured own-project Google app", "disconnect": "Remove only the dedicated business-review token"}[action], Args: cobra.ExactArgs(1)}
		command.RunE = func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration unavailable")
			}
			if isRemoteModeFor(state) || isAgentMode(state) {
				return errors.New("business review authorization and disconnect run on the daemon host only")
			}
			target, err := businessReviewTarget(state.cfg, args[0])
			if err != nil {
				return err
			}
			if !target.OwnProject {
				return errors.New("set own_project = true only for your own approved project before authorization")
			}
			// Clear outstanding consent on the daemon before changing credentials.
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return errors.New("the owner daemon must be available to invalidate outstanding business-review previews before changing credentials")
			}
			_, invalidateErr := client.ControlBusinessReviews(cmd.Context(), businessreviews.Request{Action: "invalidate_target", Target: target.Name})
			_ = client.Close()
			if invalidateErr != nil {
				return errors.New("could not invalidate business-review previews; credentials were not changed")
			}
			if action == "disconnect" {
				manager := oauth.NewStoredTokenManager(state.cfg.TokensDir(), state.cfg.OAuth.Tokens).
					WithTokenNamespace(businessReviewNamespace(target.OAuthApp))
				if err := manager.DeleteToken(cmd.Context(), target.Email); err != nil {
					return fmt.Errorf("remove stored business-review token: %w", businessreviews.ErrUnavailable)
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "Dedicated business-review token removed; other integrations are unchanged.")
				if err != nil {
					return fmt.Errorf("write business-review disconnect result: %w", err)
				}
				return nil
			}
			manager, err := newBusinessReviewOAuthManager(cmd.Context(), state.cfg, target)
			if err != nil {
				return fmt.Errorf("load configured business-review OAuth app credentials: %w", businessreviews.ErrUnavailable)
			}
			if headless {
				err = manager.AuthorizeManual(cmd.Context(), target.Email)
			} else {
				err = manager.Authorize(cmd.Context(), target.Email)
			}
			if err != nil {
				return fmt.Errorf("authorize business-review app: %w", businessreviews.ErrUnavailable)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Business-review authorization saved in its dedicated namespace.")
			if err != nil {
				return fmt.Errorf("write business-review authorization result: %w", err)
			}
			return nil
		}
		if action == "authorize" {
			command.Flags().BoolVar(&headless, "headless", false, "Use manual authorization without opening a browser")
		}
		root.AddCommand(command)
	}
	return root
}
func businessReviewTarget(cfg *config.Config, name string) (businessreviews.Target, error) {
	for _, target := range cfg.BusinessReviews {
		if target.Name == name {
			if target.Validate() != nil {
				return businessreviews.Target{}, businessreviews.ErrInvalid
			}
			return target, nil
		}
	}
	return businessreviews.Target{}, businessreviews.ErrDenied
}
func businessReviewNamespace(app string) string {
	return fmt.Sprintf("business-reviews-google/%x", sha256.Sum256([]byte(app)))
}
func newBusinessReviewOAuthManager(ctx context.Context, cfg *config.Config, target businessreviews.Target) (*oauth.Manager, error) {
	if target.OAuthApp == "" {
		return nil, businessreviews.ErrDenied
	}
	credentials, err := cfg.OAuth.CredentialsFor(target.OAuthApp)
	if err != nil {
		return nil, businessreviews.ErrUnavailable
	}
	manager, err := oauth.NewManagerWithCredentials(ctx, credentials, cfg.TokensDir(), cfg.OAuth.Tokens, slog.New(slog.DiscardHandler), businessReviewScopes)
	if err != nil {
		return nil, businessreviews.ErrUnavailable
	}
	return manager.WithTokenNamespace(businessReviewNamespace(target.OAuthApp)), nil
}
func newDaemonBusinessReviews(cfg *config.Config) *businessreviews.Service {
	fresh := func() (*config.Config, error) {
		snapshot, err := config.ReadConfigFile(cfg.ConfigFilePath())
		if err != nil {
			return nil, businessreviews.ErrUnavailable
		}
		loaded, err := cfg.ReloadConfigFile(snapshot)
		if err != nil {
			return nil, businessreviews.ErrUnavailable
		}
		return loaded, nil
	}
	return businessreviews.NewService(businessreviews.Options{
		Targets: func(context.Context) ([]businessreviews.Target, error) {
			loaded, err := fresh()
			if err != nil {
				return nil, err
			}
			return loaded.BusinessReviews, nil
		},
		Provider: func(ctx context.Context, target businessreviews.Target) (businessreviews.Provider, string, error) {
			loaded, err := fresh()
			if err != nil {
				return nil, "", err
			}
			current, err := businessReviewTarget(loaded, target.Name)
			if err != nil || current != target {
				return nil, "", businessreviews.ErrDenied
			}
			manager, err := newBusinessReviewOAuthManager(ctx, loaded, target)
			if err != nil {
				return nil, "", err
			}
			source, binding, err := manager.CheckedTokenSource(ctx, target.Email, businessReviewScopes)
			if err != nil {
				return nil, "", businessreviews.ErrCredentials
			}
			info, err := os.Stat(cfg.ConfigFilePath())
			if err != nil {
				return nil, "", businessreviews.ErrUnavailable
			}
			generation := fmt.Sprintf("%s:%d:%d", binding, info.ModTime().UnixNano(), info.Size())
			digest := sha256.Sum256([]byte(generation))
			return businessreviews.NewGoogle(oauth2.NewClient(ctx, source)), hex.EncodeToString(digest[:]), nil
		},
	})
}

// Redact all business-review positionals as well as sensitive flags. The reply
// itself is accepted only on stdin, never in argv or the daemon CLI journal.
func sanitizeBusinessReviewArgs(args []string) []string {
	out := append([]string(nil), args...)
	seen, actionSeen := false, false
	for i, arg := range out {
		if arg == "business-reviews" {
			seen = true
			continue
		}
		if !seen {
			continue
		}
		if !actionSeen && !strings.HasPrefix(arg, "-") {
			actionSeen = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			out[i] = "<redacted>"
		}
	}
	return out
}
