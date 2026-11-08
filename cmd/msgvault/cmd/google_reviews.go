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
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/googlereviews"
	"go.kenn.io/msgvault/internal/oauth"
	"golang.org/x/oauth2"
)

const businessManageScope = "https://www.googleapis.com/auth/business.manage"

var googleReviewScopes = []string{businessManageScope, oauth.ScopeUserinfoEmail}

type googleReviewsRunner func(context.Context, googlereviews.Request) (*googlereviews.Result, error)

func init() { rootCmd.AddCommand(newGoogleReviewsCmd(nil)) }
func newGoogleReviewsCmd(run googleReviewsRunner) *cobra.Command {
	root := &cobra.Command{Use: "google-reviews", Short: "Archive and search Google reviews, and confirm owner replies", Long: "Read live Google Business Profile reviews, archive reviews returned by the API, and search the main archive.\nRequires your own approved Google project, a dedicated named OAuth app, and explicitly configured targets.\nBackfill follows API pagination and reports exhaustion without guaranteeing lifetime coverage.\nEach reply needs an exact preview and a separate confirmation. Facebook remains unavailable."}
	root.PersistentFlags().Bool("json", false, "Accepted for compatibility; output is always JSON")
	for _, entry := range []struct {
		name, action, use, short string
		args                     int
	}{
		{"capabilities", "capabilities", "capabilities", "Show supported providers and configured targets", 0},
		{cmdUseList, cmdUseList, cmdUseList + " TARGET", "Read a fresh page of reviews", 1},
		{"backfill", "backfill", "backfill TARGET", "Archive available reviews; resume interrupted imports.", 1},
		{"archive-status", "archive_status", "archive-status TARGET", "Show archive progress and totals.", 1},
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
			request := googlereviews.Request{Action: entry.action}
			switch entry.action {
			case cmdUseList:
				request.Target = args[0]
				request.PageSize = pageSize
				request.PageToken = pageToken
			case "backfill", "archive_status":
				request.Target = args[0]
			case "get", "preview_reply":
				request.Target = args[0]
				request.ReviewID = args[1]
				if entry.action == "preview_reply" {
					data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4097))
					if err != nil {
						return googlereviews.ErrInvalid
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
				runner = func(ctx context.Context, request googlereviews.Request) (*googlereviews.Result, error) {
					client, _, err := OpenHTTPStore(ctx)
					if err != nil {
						return nil, err
					}
					defer func() { _ = client.Close() }()
					return client.ControlGoogleReviews(ctx, request)
				}
			}
			result, err := runner(cmd.Context(), request)
			if err != nil {
				return err
			}
			if result == nil {
				return errors.New("google review response unavailable")
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
	var searchTarget string
	var searchPage, searchPageSize int
	searchCommand := &cobra.Command{
		Use:   "search QUERY",
		Short: "Search archived Google reviews (query limit 256 UTF-8 bytes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			result, err := client.SearchGoogleReviewsArchive(cmd.Context(), googlereviews.ArchiveSearchRequest{
				Query: args[0], Target: searchTarget, Page: searchPage, PageSize: searchPageSize,
			})
			if err != nil {
				return err
			}
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true))
		},
	}
	searchCommand.Flags().StringVar(&searchTarget, "target", "", "Restrict search to one archive target")
	searchCommand.Flags().IntVar(&searchPage, "page", 1, "One-based result page")
	searchCommand.Flags().IntVar(&searchPageSize, "page-size", 20, "Results per page (1-100)")
	root.AddCommand(searchCommand)
	for _, action := range []string{"authorize", "disconnect"} {
		var headless bool
		command := &cobra.Command{Use: action + " TARGET", Short: map[string]string{"authorize": "Authorize the configured own-project Google app", "disconnect": "Remove only the dedicated google-review token"}[action], Args: cobra.ExactArgs(1)}
		command.RunE = func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration unavailable")
			}
			if isRemoteModeFor(state) || isAgentMode(state) {
				return errors.New("google review authorization and disconnect run on the daemon host only")
			}
			target, err := googleReviewTarget(state.cfg, args[0])
			if err != nil {
				return err
			}
			if action == "authorize" && !target.OwnProject {
				return errors.New("set own_project = true only for your own approved project before authorization")
			}
			// Stop imports and clear outstanding consent before changing credentials.
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return errors.New("the owner daemon must be available to stop imports and invalidate previews before changing credentials")
			}
			invalidation, invalidateErr := client.ControlGoogleReviews(cmd.Context(), googlereviews.Request{Action: "invalidate_target", Target: target.Name})
			_ = client.Close()
			if invalidateErr != nil || invalidation == nil || invalidation.CredentialChangeToken == "" {
				return errors.New("could not stop imports and invalidate google-review previews; credentials were not changed")
			}
			if action == "disconnect" {
				manager := oauth.NewStoredTokenManager(state.cfg.TokensDir(), state.cfg.OAuth.Tokens).
					WithTokenNamespace(googleReviewNamespace(target.OAuthApp))
				if err := manager.DeleteToken(cmd.Context(), target.Email); err != nil {
					return fmt.Errorf("remove stored google-review token: %w", googlereviews.ErrUnavailable)
				}
				// The daemon may have restarted since the first invalidation. Settle
				// any imports admitted by the replacement before reporting success.
				client, _, err = OpenHTTPStore(cmd.Context())
				if err != nil {
					return errors.New("token removed, but the daemon could not confirm imports stopped")
				}
				_, invalidateErr = client.ControlGoogleReviews(cmd.Context(), googlereviews.Request{Action: "invalidate_target", Target: target.Name})
				_ = client.Close()
				if invalidateErr != nil {
					return errors.New("token removed, but the daemon could not confirm imports stopped")
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "Dedicated google-review token removed; other integrations are unchanged.")
				if err != nil {
					return fmt.Errorf("write google-review disconnect result: %w", err)
				}
				return nil
			}
			manager, err := newGoogleReviewOAuthManager(cmd.Context(), state.cfg, target)
			if err != nil {
				return fmt.Errorf("load configured google-review OAuth app credentials: %w", googlereviews.ErrUnavailable)
			}
			if headless {
				err = manager.AuthorizeManual(cmd.Context(), target.Email)
			} else {
				err = manager.Authorize(cmd.Context(), target.Email)
			}
			if err != nil {
				return fmt.Errorf("authorize google-review app: %w", googlereviews.ErrUnavailable)
			}
			client, _, err = OpenHTTPStore(cmd.Context())
			if err != nil {
				return errors.New("authorization was saved, but the daemon could not resume review imports; authorize again")
			}
			_, resumeErr := client.ControlGoogleReviews(cmd.Context(), googlereviews.Request{Action: "resume_target", Target: target.Name, ConfirmationToken: invalidation.CredentialChangeToken})
			_ = client.Close()
			if resumeErr != nil {
				return errors.New("authorization was saved, but the daemon could not resume review imports; authorize again")
			}
			return writeGoogleReviewAuthorizationResult(cmd.OutOrStdout())
		}
		if action == "authorize" {
			command.Flags().BoolVar(&headless, "headless", false, "Use manual authorization without opening a browser")
		}
		root.AddCommand(command)
	}
	return root
}

func writeGoogleReviewAuthorizationResult(output io.Writer) error {
	if _, err := fmt.Fprintln(output, "Google Reviews authorization saved separately."); err != nil {
		return fmt.Errorf("write google-review authorization result: %w", err)
	}
	return nil
}

func googleReviewTarget(cfg *config.Config, name string) (googlereviews.Target, error) {
	for _, target := range cfg.GoogleReviews {
		if target.Name == name {
			if target.Validate() != nil {
				return googlereviews.Target{}, googlereviews.ErrInvalid
			}
			return target, nil
		}
	}
	return googlereviews.Target{}, googlereviews.ErrDenied
}
func googleReviewNamespace(app string) string {
	return fmt.Sprintf("business-reviews-google/%x", sha256.Sum256([]byte(app)))
}
func newGoogleReviewOAuthManager(ctx context.Context, cfg *config.Config, target googlereviews.Target) (*oauth.Manager, error) {
	if target.OAuthApp == "" {
		return nil, googlereviews.ErrDenied
	}
	credentials, err := cfg.OAuth.CredentialsFor(target.OAuthApp)
	if err != nil {
		return nil, googlereviews.ErrUnavailable
	}
	manager, err := oauth.NewManagerWithCredentials(ctx, credentials, cfg.TokensDir(), cfg.OAuth.Tokens, slog.New(slog.DiscardHandler), googleReviewScopes)
	if err != nil {
		return nil, googlereviews.ErrUnavailable
	}
	return manager.WithTokenNamespace(googleReviewNamespace(target.OAuthApp)), nil
}
func newDaemonGoogleReviews(cfg *config.Config, gate api.OperationGate, idle *api.IdleTracker, archives ...googlereviews.Archive) *googlereviews.Service {
	fresh := func() (*config.Config, error) {
		snapshot, err := config.ReadConfigFile(cfg.ConfigFilePath())
		if err != nil {
			return nil, googlereviews.ErrUnavailable
		}
		loaded, err := cfg.ReloadConfigFile(snapshot)
		if err != nil {
			return nil, googlereviews.ErrUnavailable
		}
		return loaded, nil
	}
	var archive googlereviews.Archive
	if len(archives) > 0 {
		archive = archives[0]
	}
	return googlereviews.NewService(googlereviews.Options{
		Archive:           archive,
		BeginBackfillWork: idle.BeginWorkContext,
		BeginArchiveWrite: func(ctx context.Context) (func(), bool) {
			if gate == nil {
				return func() {}, true
			}
			return gate.BeginWorkContext(ctx)
		},
		Targets: func(context.Context) ([]googlereviews.Target, error) {
			loaded, err := fresh()
			if err != nil {
				return nil, err
			}
			return loaded.GoogleReviews, nil
		},
		Provider: func(ctx context.Context, target googlereviews.Target) (googlereviews.Provider, string, error) {
			loaded, err := fresh()
			if err != nil {
				return nil, "", err
			}
			current, err := googleReviewTarget(loaded, target.Name)
			if err != nil || current != target {
				return nil, "", googlereviews.ErrDenied
			}
			manager, err := newGoogleReviewOAuthManager(ctx, loaded, target)
			if err != nil {
				return nil, "", err
			}
			source, binding, err := manager.CheckedTokenSource(ctx, target.Email, googleReviewScopes)
			if err != nil {
				return nil, "", googlereviews.ErrCredentials
			}
			info, err := os.Stat(cfg.ConfigFilePath())
			if err != nil {
				return nil, "", googlereviews.ErrUnavailable
			}
			generation := fmt.Sprintf("%s:%d:%d", binding, info.ModTime().UnixNano(), info.Size())
			digest := sha256.Sum256([]byte(generation))
			return googlereviews.NewGoogle(oauth2.NewClient(ctx, source)), hex.EncodeToString(digest[:]), nil
		},
	})
}

// Redact all google-review positionals as well as sensitive flags. The reply
// itself is accepted only on stdin, never in argv or the daemon CLI journal.
func sanitizeGoogleReviewArgs(args []string) []string {
	out := append([]string(nil), args...)
	seen, actionSeen := false, false
	for i, arg := range out {
		if arg == "google-reviews" {
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
