package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

const calScopeEscalationConfirmedFlag = "scope-escalation-confirmed"

var (
	calAddOAuthApp   string
	calAddHeadless   bool
	calAddAll        bool
	calAddMinRole    string
	calAddCalendars  []string
	calSyncOAuthApp  string
	calSyncFull      bool
	calSyncLimit     int
	calSyncAfter     string
	calSyncBefore    string
	calSyncNoResume  bool
	calSyncAll       bool
	calSyncMinRole   string
	calSyncCalendars []string
)

func init() {
	rootCmd.AddCommand(newAddCalendarCmd())
	rootCmd.AddCommand(newSyncCalendarCmd())
}

func interactiveStdin() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

func newAddCalendarCmd() *cobra.Command {
	cmd := newAddCalendarLocalCmd()
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runAddCalendarHTTP(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newAddCalendarLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add-calendar <email>",
		Short: "Authorize Google Calendar access and register calendars for an account",
		Long: "Grants read-only Calendar access (calendar.readonly) to an account and " +
			"registers its calendars for sync. If the account already has a Gmail token, " +
			"re-consent bundles Gmail + Calendar together; keep BOTH checked on the consent " +
			"screen so Gmail access is not dropped.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := normalizeCalendarAccountEmail(args[0])
			ctx := cmd.Context()
			oauthAppExplicit := cmd.Flags().Changed("oauth-app")
			if email == "" {
				return usageErr(cmd, errors.New("account email is required"))
			}
			if err := calsync.ValidateMinAccessRole(calAddMinRole); err != nil {
				return usageErr(cmd, err)
			}

			st, cleanup, err := openWritableStoreAndInit()
			if err != nil {
				return err
			}
			defer cleanup()

			appDecision, err := calendarAddOAuthAppDecision(st, email, calAddOAuthApp, oauthAppExplicit)
			if err != nil {
				return err
			}
			oauthApp := appDecision.OAuthApp

			secretsPath, err := cfg.OAuth.ClientSecretsFor(oauthApp)
			if err != nil {
				return err
			}
			mgr, err := newCalendarOAuthManager(secretsPath, email)
			if err != nil {
				return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
			}
			hasToken := mgr.HasToken(email)
			hasCalendarScope := mgr.HasScope(email, oauth.ScopeCalendarReadonly)
			tokenReusable := calendarAddTokenReusable(mgr, email, appDecision)

			// A token that exists, carries the calendar scope, and matches the
			// client still looks reusable even when its refresh token is expired
			// or revoked. Probe it so a dead token is reauthorized here instead
			// of falling through to buildCalendarClient and failing with a
			// non-interactive invalid_grant that the recovery hint would only
			// tell the user to repeat.
			tokenExpiredOrRevoked := hasToken && hasCalendarScope && tokenReusable &&
				calendarTokenExpiredOrRevoked(ctx, mgr, email)

			// A headless host cannot complete Google's browser consent, and the
			// OAuth device flow does not support Calendar scopes. If this account
			// still needs Calendar authorization, mirror add-account --headless:
			// print copy-the-token instructions and stop — without launching a
			// browser or touching the existing Gmail token. Once the dual-scope
			// token is copied in, re-running add-calendar --headless skips this
			// and registers the calendars (an API call that needs no browser).
			if calAddHeadless && (!hasToken || !hasCalendarScope || !tokenReusable || tokenExpiredOrRevoked) {
				oauth.PrintCalendarHeadlessInstructions(email, cfg.TokensDir(), oauthApp)
				return nil
			}

			switch {
			case !hasToken:
				fmt.Printf("Authorizing %s for Calendar...\n", email)
				if err := mgr.Authorize(ctx, email); err != nil {
					return wrapOAuthError(err)
				}
			case tokenExpiredOrRevoked:
				fmt.Printf("Calendar token for %s is expired or revoked. Re-authorizing...\n", email)
				if err := mgr.AuthorizePreservingGrantedScopes(ctx, email); err != nil {
					return wrapOAuthError(err)
				}
			case !hasCalendarScope:
				headline, body, cancelHint := calendarScopeEscalationPrompt()
				existingScopes := mgr.GrantedScopes(email)
				requiredScopes := calendarEscalationScopes(existingScopes,
					calendarShouldPreserveGmail(hasToken, mgr.HasScopeMetadata(email), existingScopes))
				confirmed, err := cmd.Flags().GetBool(calScopeEscalationConfirmedFlag)
				if err != nil {
					return fmt.Errorf("read --%s flag: %w", calScopeEscalationConfirmedFlag, err)
				}
				if confirmed {
					if err := authorizeScopeEscalation(ctx, email, requiredScopes, secretsPath); err != nil {
						return err
					}
				} else {
					if err := promptScopeEscalation(ctx, email, requiredScopes,
						headline, body, cancelHint, secretsPath); err != nil {
						if errors.Is(err, errUserCanceled) {
							return nil
						}
						return err
					}
				}
			case !tokenReusable:
				fmt.Printf("OAuth app for %s requires reauthorization. Authorizing...\n", email)
				if err := mgr.Authorize(ctx, email); err != nil {
					return wrapOAuthError(err)
				}
			}

			client, err := buildCalendarClient(ctx, email, oauthApp, interactiveStdin())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			syncer := calsync.New(client, st, calsync.Options{
				AccountEmail:  email,
				OAuthApp:      oauthApp,
				OAuthAppSet:   oauthAppExplicit || appDecision.BindingChanged,
				Calendars:     calAddCalendars,
				AllCalendars:  calAddAll,
				MinAccessRole: calAddMinRole,
			}).WithLogger(logger)

			// RegisterCalendars enumerates calendars (a live smoke test that the
			// calendar scope was actually granted) and creates the source rows.
			cals, err := syncer.RegisterCalendars(ctx)
			if err != nil {
				return fmt.Errorf("register calendars (was Calendar access granted?): %w", err)
			}
			if len(cals) == 0 {
				fmt.Println("No calendars matched the filter (try --all-calendars or --calendars).")
				return nil
			}
			fmt.Printf("Registered %d calendar(s) for %s:\n", len(cals), email)
			for _, c := range cals {
				fmt.Printf("  - %s (%s)\n", calendarLabel(c), c.AccessRole)
			}
			fmt.Printf("\nNext: %s\n", calendarSyncNextCommand(email, oauthApp, calendarSyncNextOptions{
				AllCalendars:  calAddAll,
				MinAccessRole: calAddMinRole,
				Calendars:     calAddCalendars,
			}))
			return nil
		},
	}
	cmd.Flags().StringVar(&calAddOAuthApp, "oauth-app", "", "named OAuth app to use")
	cmd.Flags().BoolVar(&calAddHeadless, "headless", false, "headless host: print token-copy instructions instead of opening a browser")
	cmd.Flags().BoolVar(&calAddAll, "all-calendars", false, "include reader/freeBusyReader calendars (default: owner+writer)")
	cmd.Flags().StringVar(&calAddMinRole, "min-access-role", "", "minimum access role: owner|writer|reader")
	cmd.Flags().StringSliceVar(&calAddCalendars, "calendars", nil, "comma-separated calendar IDs to register (default: by access role)")
	cmd.Flags().Bool(calScopeEscalationConfirmedFlag, false, "Internal: Calendar scope escalation was already accepted by the frontend CLI")
	if err := cmd.Flags().MarkHidden(calScopeEscalationConfirmedFlag); err != nil {
		panic(err)
	}
	return cmd
}

func runAddCalendarHTTP(cmd *cobra.Command, args []string) error {
	email := normalizeCalendarAccountEmail(args[0])
	if email == "" {
		return usageErr(cmd, errors.New("account email is required"))
	}
	if err := calsync.ValidateMinAccessRole(calAddMinRole); err != nil {
		return usageErr(cmd, err)
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	plan, err := st.PlanCLIAddCalendar(cmd.Context(), daemonclient.CLIAddCalendarPlanRequest{
		Email:            email,
		OAuthApp:         calAddOAuthApp,
		OAuthAppExplicit: cmd.Flags().Changed("oauth-app"),
		Headless:         calAddHeadless,
	})
	if err != nil {
		return err
	}
	escalationConfirmed := false
	if plan != nil && plan.NeedsScopeEscalation {
		ok, err := promptAddCalendarScopeEscalation(cmd.InOrStdin(), cmd.OutOrStdout(), *plan)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		escalationConfirmed = true
		if err := cmd.Flags().Set(calScopeEscalationConfirmedFlag, "true"); err != nil {
			return fmt.Errorf("set --%s after confirmation: %w", calScopeEscalationConfirmedFlag, err)
		}
	}
	if err := preflightAddCalendarAuthorize(cmd.Context(), email, plan, escalationConfirmed,
		calAddOAuthApp, cmd.Flags().Changed("oauth-app")); err != nil {
		return err
	}
	return runDaemonCLICommandHTTPFromCobra(cmd, args)
}

// preflightCalendarOAuthApp picks the OAuth app for the client-side
// authorization preflight. The daemon-resolved binding wins; against an
// older daemon that does not report one, only an explicitly requested app
// is safe to authorize here (ok=false keeps daemon-side authorization, the
// pre-preflight behavior, instead of guessing the default app and minting
// a token for the wrong client).
func preflightCalendarOAuthApp(
	plan *daemonclient.CLIAddCalendarPlan,
	requestedApp string,
	requestedExplicit bool,
) (app string, needsClientCheck bool, ok bool) {
	if plan.OAuthAppResolved {
		return plan.OAuthApp, plan.NeedsClientCheck, true
	}
	if requestedExplicit {
		return requestedApp, true, true
	}
	return "", false, false
}

// preflightAddCalendarAuthorize completes any Calendar browser authorization
// in this process before proxying, so the daemon subprocess never opens a
// browser or waits on human consent while holding the operation gate. After a
// successful preflight the subprocess re-evaluates the token, finds it valid,
// and skips its own authorization branches. Remote daemons keep daemon-side
// authorization because tokens live on that host; headless mode never opens a
// browser; service-account apps need no browser flow.
func preflightAddCalendarAuthorize(
	ctx context.Context,
	email string,
	plan *daemonclient.CLIAddCalendarPlan,
	escalationConfirmed bool,
	requestedApp string,
	requestedExplicit bool,
) error {
	if IsRemoteMode() || calAddHeadless || plan == nil {
		return nil
	}
	oauthApp, needsClientCheck, ok := preflightCalendarOAuthApp(plan, requestedApp, requestedExplicit)
	if !ok {
		return nil
	}
	if cfg.OAuth.ServiceAccountKeyFor(oauthApp) != "" {
		return nil
	}
	secretsPath, err := cfg.OAuth.ClientSecretsFor(oauthApp)
	if err != nil {
		return err
	}
	mgr, err := newCalendarOAuthManager(secretsPath, email)
	if err != nil {
		return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	hasToken := mgr.HasToken(email)
	hasCalendarScope := mgr.HasScope(email, oauth.ScopeCalendarReadonly)
	tokenReusable := hasToken && (!needsClientCheck || mgr.TokenMatchesClient(email))
	tokenExpiredOrRevoked := hasToken && hasCalendarScope && tokenReusable &&
		calendarTokenExpiredOrRevoked(ctx, mgr, email)

	switch {
	case !hasToken:
		fmt.Printf("Authorizing %s for Calendar...\n", email)
		if err := mgr.Authorize(ctx, email); err != nil {
			return wrapOAuthError(err)
		}
	case tokenExpiredOrRevoked:
		fmt.Printf("Calendar token for %s is expired or revoked. Re-authorizing...\n", email)
		if err := mgr.AuthorizePreservingGrantedScopes(ctx, email); err != nil {
			return wrapOAuthError(err)
		}
	case !hasCalendarScope:
		// Scope escalation replaces the granted scope set, so it only runs
		// after the user accepted the plan's warning prompt.
		if !escalationConfirmed {
			return nil
		}
		existingScopes := mgr.GrantedScopes(email)
		requiredScopes := calendarEscalationScopes(existingScopes,
			calendarShouldPreserveGmail(hasToken, mgr.HasScopeMetadata(email), existingScopes))
		if err := authorizeScopeEscalation(ctx, email, requiredScopes, secretsPath); err != nil {
			return err
		}
	case !tokenReusable:
		fmt.Printf("OAuth app for %s requires reauthorization. Authorizing...\n", email)
		if err := mgr.Authorize(ctx, email); err != nil {
			return wrapOAuthError(err)
		}
	}
	return nil
}

func promptAddCalendarScopeEscalation(
	in io.Reader,
	out io.Writer,
	plan daemonclient.CLIAddCalendarPlan,
) (bool, error) {
	return promptScopeEscalationConfirmation(in, out, plan.Headline, plan.BodyLines, plan.CancelHint)
}

func newSyncCalendarCmd() *cobra.Command {
	cmd := newSyncCalendarLocalCmd()
	runLocal := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		return runLocal(cmd, args)
	}
	return cmd
}

func newSyncCalendarLocalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "sync-calendar <name|email>",
		Aliases: []string{"sync-calendar-incremental"},
		Short:   "Sync Google Calendar events for a configured or registered account",
		Long: "Syncs calendar events for an account. The first run (or --full) does a full " +
			"sync that enumerates and registers calendars; subsequent runs are incremental " +
			"via syncToken. The account is resolved from a [[gcal]] config entry (by name or " +
			"email) or used directly as an email. --after/--before bound a full sync only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			email := normalizeCalendarAccountEmail(args[0])
			oauthAppExplicit := cmd.Flags().Changed("oauth-app")
			oauthAppProvided := oauthAppExplicit
			oauthApp := ""
			if oauthAppProvided {
				oauthApp = calSyncOAuthApp
			}
			calendars := calSyncCalendars
			if src := cfg.GetGCalSource(args[0]); src != nil {
				email = normalizeCalendarAccountEmail(src.Email)
				if !oauthAppProvided && src.OAuthApp != "" {
					oauthApp = src.OAuthApp
					oauthAppProvided = true
				}
				if len(calendars) == 0 {
					calendars = src.Calendars
				}
			}
			if email == "" {
				return usageErr(cmd, errors.New("could not resolve an account email (pass an email or a configured [[gcal]] name)"))
			}

			timeMin, timeMax, err := calendarDateBounds(cmd, calSyncAfter, calSyncBefore)
			if err != nil {
				return err
			}
			if calSyncLimit < 0 {
				return usageErr(cmd, errors.New("--limit must be a non-negative number"))
			}
			hasFullOnlyOptions := calendarSyncHasFullOnlyOptions(timeMin, timeMax, calSyncLimit)
			if err := calsync.ValidateMinAccessRole(calSyncMinRole); err != nil {
				return usageErr(cmd, err)
			}

			st, cleanup, err := openWritableStoreAndInit()
			if err != nil {
				return err
			}
			defer cleanup()

			existing, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, email)
			if err != nil {
				return fmt.Errorf("load registered calendar sources for %s: %w", email, err)
			}
			appDecision, err := calendarSyncOAuthAppDecision(st, email, existing, oauthApp, oauthAppProvided)
			if err != nil {
				return err
			}
			oauthApp = appDecision.OAuthApp

			client, err := buildCalendarClient(ctx, email, oauthApp, interactiveStdin())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			syncer := calsync.New(client, st, calsync.Options{
				AccountEmail:  email,
				OAuthApp:      oauthApp,
				OAuthAppSet:   appDecision.OAuthAppSet,
				Calendars:     calendars,
				AllCalendars:  calSyncAll,
				MinAccessRole: calSyncMinRole,
				TimeMin:       timeMin,
				TimeMax:       timeMax,
				Limit:         calSyncLimit,
				NoResume:      calSyncNoResume,
			}).WithLogger(logger)

			var res calsync.Result
			if calendarSyncShouldRunFullForSources(existing, calSyncFull, calSyncAll, calSyncMinRole, calendars, hasFullOnlyOptions) {
				res, err = syncer.Full(ctx)
			} else {
				res, err = syncer.Incremental(ctx)
			}
			if err != nil {
				return err
			}
			fmt.Printf("Calendar sync complete: %d calendar(s), %d event(s) added, %d cancelled\n",
				res.CalendarsSynced, res.EventsAdded, res.EventsCancelled)
			rebuildCacheAfterWrite(cfg.DatabaseDSN())
			return nil
		},
	}
	cmd.Flags().StringVar(&calSyncOAuthApp, "oauth-app", "", "named OAuth app to use")
	cmd.Flags().BoolVar(&calSyncFull, "full", false, "force a full sync (ignore stored sync tokens)")
	cmd.Flags().IntVar(&calSyncLimit, "limit", 0, "max events per calendar (0 = unlimited)")
	cmd.Flags().StringVar(&calSyncAfter, "after", "", "full-sync only: earliest event date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&calSyncBefore, "before", "", "full-sync only: latest event date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&calSyncNoResume, "noresume", false, "do not resume an interrupted full sync")
	cmd.Flags().BoolVar(&calSyncAll, "all-calendars", false, "include reader/freeBusyReader calendars")
	cmd.Flags().StringVar(&calSyncMinRole, "min-access-role", "", "minimum access role: owner|writer|reader")
	cmd.Flags().StringSliceVar(&calSyncCalendars, "calendar", nil, "restrict to specific calendar IDs")
	return cmd
}

func calendarAddOAuthScopes(preserveGmail bool) []string {
	if preserveGmail {
		return append([]string(nil), oauth.ScopesGmailCalendar...)
	}
	return append([]string(nil), oauth.ScopesCalendar...)
}

func newCalendarOAuthManager(clientSecretsPath, account string) (*oauth.Manager, error) {
	account = normalizeCalendarAccountEmail(account)
	probe, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, oauth.ScopesCalendar)
	if err != nil {
		return nil, err
	}
	existingScopes := probe.GrantedScopes(account)
	scopes := calendarOAuthScopesForAccount(probe.HasToken(account), probe.HasScopeMetadata(account), existingScopes)
	if slices.Equal(scopes, oauth.ScopesCalendar) {
		return probe, nil
	}
	return oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, scopes)
}

func calendarSyncHasFullOnlyOptions(timeMin, timeMax string, limit int) bool {
	return timeMin != "" || timeMax != "" || limit > 0
}

func calendarSyncShouldRunFullForSources(existing []*store.Source, forceFull bool, allCalendars bool, minRole string, calendars []string, hasFullOnlyOptions bool) bool {
	if forceFull || hasFullOnlyOptions || len(existing) == 0 || allCalendars || minRole != "" {
		return true
	}
	return calendarSelectionMissingRegisteredSource(existing, calendars)
}

func calendarSelectionMissingRegisteredSource(existing []*store.Source, calendars []string) bool {
	registered := calendarRegisteredIDs(existing)
	if len(registered) == 0 {
		return true
	}
	if len(calendars) == 0 {
		return false
	}
	for _, calendarID := range calendars {
		if _, ok := registered[calendarID]; !ok {
			return true
		}
	}
	return false
}

func calendarRegisteredIDs(sources []*store.Source) map[string]struct{} {
	ids := make(map[string]struct{}, len(sources))
	for _, src := range sources {
		if src == nil || !src.SyncConfig.Valid || src.SyncConfig.String == "" {
			continue
		}
		var cfg struct {
			CalendarID string `json:"calendar_id"`
		}
		if err := json.Unmarshal([]byte(src.SyncConfig.String), &cfg); err != nil {
			continue
		}
		if cfg.CalendarID != "" {
			ids[cfg.CalendarID] = struct{}{}
		}
	}
	return ids
}

func calendarEscalationScopes(existingScopes []string, preserveGmail bool) []string {
	scopes := append([]string(nil), existingScopes...)
	required := calendarAddOAuthScopes(preserveGmail)
	for _, scope := range required {
		scopes = appendScopeIfMissing(scopes, scope)
	}
	return scopes
}

func calendarScopeEscalationPrompt() (string, []string, string) {
	return "CALENDAR ACCESS REQUIRED", []string{
		"Calendar sync needs read-only Calendar access.",
		"",
		"Re-authorizing REPLACES the granted scopes, so msgvault will",
		"re-request Gmail, Calendar, and any already granted Google",
		"scopes together. On the consent screen, keep every existing",
		"permission checked or Google will remove that access for this",
		"account.",
	}, "Cancelled. Calendar was not added."
}

func planCLIAddCalendar(
	_ context.Context,
	st *store.Store,
	req api.CLIAddCalendarPlanRequest,
) (api.CLIAddCalendarPlanResponse, error) {
	email := normalizeCalendarAccountEmail(req.Email)
	if email == "" {
		return api.CLIAddCalendarPlanResponse{}, errors.New("account email is required")
	}
	appDecision, err := calendarAddOAuthAppDecision(st, email, req.OAuthApp, req.OAuthAppExplicit)
	if err != nil {
		return api.CLIAddCalendarPlanResponse{}, err
	}
	oauthApp := appDecision.OAuthApp

	secretsPath, err := cfg.OAuth.ClientSecretsFor(oauthApp)
	if err != nil {
		return api.CLIAddCalendarPlanResponse{}, err
	}
	mgr, err := newCalendarOAuthManager(secretsPath, email)
	if err != nil {
		return api.CLIAddCalendarPlanResponse{}, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
	}
	// The resolved app binding is returned even when no escalation is
	// needed: the frontend uses it to run any required browser
	// authorization client-side before proxying.
	plan := api.CLIAddCalendarPlanResponse{
		OAuthApp:         oauthApp,
		OAuthAppResolved: true,
		NeedsClientCheck: appDecision.NeedsClientCheck,
	}
	hasToken := mgr.HasToken(email)
	hasCalendarScope := mgr.HasScope(email, oauth.ScopeCalendarReadonly)
	if req.Headless || !hasToken || hasCalendarScope {
		return plan, nil
	}

	headline, body, cancelHint := calendarScopeEscalationPrompt()
	plan.NeedsScopeEscalation = true
	plan.Headline = headline
	plan.BodyLines = body
	plan.CancelHint = cancelHint
	return plan, nil
}

func calendarOAuthScopesForAccount(hasToken bool, hasScopeMetadata bool, existingScopes []string) []string {
	return calendarEscalationScopes(existingScopes,
		calendarShouldPreserveGmail(hasToken, hasScopeMetadata, existingScopes))
}

func calendarShouldPreserveGmail(hasToken bool, hasScopeMetadata bool, existingScopes []string) bool {
	if hasAnyScope(existingScopes, oauth.Scopes) {
		return true
	}
	return hasToken && !hasScopeMetadata
}

func hasAnyScope(scopes []string, candidates []string) bool {
	for _, scope := range scopes {
		if slices.Contains(candidates, scope) {
			return true
		}
	}
	return false
}

func calendarEscalationScopesForAccount(account string, clientSecretsPath string) ([]string, error) {
	account = normalizeCalendarAccountEmail(account)
	mgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, oauth.ScopesGmailCalendar)
	if err != nil {
		return nil, fmt.Errorf("create oauth manager: %w", err)
	}
	existingScopes := mgr.GrantedScopes(account)
	return calendarOAuthScopesForAccount(mgr.HasToken(account), mgr.HasScopeMetadata(account), existingScopes), nil
}

func calendarStoredOAuthApp(sources []*store.Source) string {
	for _, src := range sources {
		if app := sourceOAuthApp(src); app != "" {
			return app
		}
	}
	return ""
}

func calendarStoredAccountOAuthApp(st *store.Store, email string, calendarSources []*store.Source) (string, error) {
	if app := calendarStoredOAuthApp(calendarSources); app != "" {
		return app, nil
	}
	if len(calendarSources) > 0 {
		return "", nil
	}
	src, err := calendarGmailSourceForAccount(st, email)
	if errors.Is(err, errGmailSourceNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("look up gmail source for %s: %w", email, err)
	}
	return sourceOAuthApp(src), nil
}

func calendarGmailSourceForAccount(st *store.Store, email string) (*store.Source, error) {
	sources, err := st.ListSources(sourceTypeGmail)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		if store.EqualIdentifier(src.Identifier, email) {
			return src, nil
		}
	}
	return nil, errGmailSourceNotFound
}

func normalizeCalendarAccountEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

type calendarAddOAuthApp struct {
	OAuthApp         string
	BindingChanged   bool
	NeedsClientCheck bool
}

type calendarTokenClientMatcher interface {
	HasToken(email string) bool
	TokenMatchesClient(email string) bool
}

func calendarAddOAuthAppDecision(st *store.Store, email, requestedApp string, explicit bool) (calendarAddOAuthApp, error) {
	email = normalizeCalendarAccountEmail(email)
	sources, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, email)
	if err != nil {
		return calendarAddOAuthApp{}, fmt.Errorf("load registered calendar sources for %s: %w", email, err)
	}

	resolvedApp := requestedApp
	if !explicit && resolvedApp == "" {
		resolvedApp, err = calendarStoredAccountOAuthApp(st, email, sources)
		if err != nil {
			return calendarAddOAuthApp{}, err
		}
	}

	bindingChanged := false
	if explicit {
		for _, src := range sources {
			if sourceOAuthApp(src) != requestedApp {
				bindingChanged = true
				break
			}
		}
	}

	return calendarAddOAuthApp{
		OAuthApp:         resolvedApp,
		BindingChanged:   bindingChanged,
		NeedsClientCheck: bindingChanged || explicit || resolvedApp != "",
	}, nil
}

func calendarAddTokenReusable(mgr calendarTokenClientMatcher, email string, app calendarAddOAuthApp) bool {
	if !mgr.HasToken(email) {
		return false
	}
	if app.NeedsClientCheck {
		return mgr.TokenMatchesClient(email)
	}
	return true
}

// calendarTokenRefreshProber is the subset of oauth.Manager used to detect an
// expired or revoked Calendar refresh token.
type calendarTokenRefreshProber interface {
	HasToken(email string) bool
	ForceRefresh(ctx context.Context, email string) error
}

// calendarTokenExpiredOrRevoked reports whether a stored Calendar token can no
// longer be refreshed (Google returns invalid_grant). Such a token still passes
// HasToken/HasScope/TokenMatchesClient, so add-calendar would otherwise treat it
// as reusable and fail later in buildCalendarClient with a non-interactive
// invalid_grant. Detecting it up front lets add-calendar reauthorize (browser)
// or print headless copy-token instructions instead. The probe forces a refresh
// grant rather than reading the cached access token, so a revoked refresh token
// is caught even while the stored access token is still unexpired. Transient
// failures (network, context cancellation) are not treated as expiry, so a
// flaky probe does not force a needless reauthorization.
func calendarTokenExpiredOrRevoked(ctx context.Context, mgr calendarTokenRefreshProber, email string) bool {
	if !mgr.HasToken(email) {
		return false
	}
	if err := mgr.ForceRefresh(ctx, email); err != nil {
		return isAuthInvalidError(err)
	}
	return false
}

type calendarSyncOAuthApp struct {
	OAuthApp    string
	OAuthAppSet bool
}

func calendarSyncOAuthAppDecision(
	st *store.Store,
	email string,
	existing []*store.Source,
	requestedApp string,
	provided bool,
) (calendarSyncOAuthApp, error) {
	if provided {
		return calendarSyncOAuthApp{OAuthApp: requestedApp, OAuthAppSet: true}, nil
	}
	app, err := calendarStoredAccountOAuthApp(st, email, existing)
	if err != nil {
		return calendarSyncOAuthApp{}, err
	}
	return calendarSyncOAuthApp{OAuthApp: app}, nil
}

type calendarSyncNextOptions struct {
	AllCalendars  bool
	MinAccessRole string
	Calendars     []string
}

func calendarSyncNextCommand(email, oauthApp string, opts calendarSyncNextOptions) string {
	parts := []string{"msgvault", "sync-calendar"}
	if oauthApp != "" {
		parts = append(parts, "--oauth-app", oauthApp)
	}
	if opts.AllCalendars {
		parts = append(parts, "--all-calendars")
	}
	if opts.MinAccessRole != "" {
		parts = append(parts, "--min-access-role", opts.MinAccessRole)
	}
	for _, calendarID := range opts.Calendars {
		calendarID = strings.TrimSpace(calendarID)
		if calendarID == "" {
			continue
		}
		parts = append(parts, "--calendar", calendarID)
	}
	parts = append(parts, email)
	return strings.Join(parts, " ")
}

// buildCalendarClient constructs a gcal.API client. The OAuth token is keyed on
// the account email (never a calendar source identifier). If reauth is needed,
// it preserves Gmail only for existing Gmail/legacy tokens; Calendar-only tokens
// stay Calendar-only. The limiter is sized for the Calendar per-user budget.
func buildCalendarClient(ctx context.Context, accountEmail, oauthApp string, interactive bool) (gcal.API, error) {
	accountEmail = normalizeCalendarAccountEmail(accountEmail)
	var tokenSource oauth2.TokenSource

	if saKeyPath := cfg.OAuth.ServiceAccountKeyFor(oauthApp); saKeyPath != "" {
		saMgr, err := oauth.NewServiceAccountManager(saKeyPath, oauth.ScopesCalendar)
		if err != nil {
			return nil, fmt.Errorf("service account: %w", err)
		}
		tokenSource, err = saMgr.TokenSource(ctx, accountEmail)
		if err != nil {
			return nil, err
		}
	} else {
		secretsPath, err := cfg.OAuth.ClientSecretsFor(oauthApp)
		if err != nil {
			return nil, err
		}
		mgr, err := newCalendarOAuthManager(secretsPath, accountEmail)
		if err != nil {
			return nil, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
		}
		if err := requireCalendarTokenForSync(mgr, accountEmail); err != nil {
			return nil, err
		}
		tokenSource, err = getTokenSourceWithReauth(ctx, mgr, accountEmail, interactive, calendarReauthHint)
		if err != nil {
			return nil, err
		}
	}

	return gcal.NewClient(tokenSource,
		gcal.WithLogger(logger),
		gcal.WithRateLimiter(gmail.NewRateLimiterWithCapacity(10, 8)),
	), nil
}

func requireCalendarTokenForSync(mgr *oauth.Manager, accountEmail string) error {
	if !mgr.HasToken(accountEmail) {
		return calendarTokenActionError(accountEmail)
	}
	if !mgr.HasScopeMetadata(accountEmail) || !mgr.HasScope(accountEmail, oauth.ScopeCalendarReadonly) {
		return calendarTokenActionError(accountEmail)
	}
	return nil
}

func calendarTokenActionError(accountEmail string) error {
	return fmt.Errorf(
		"calendar access for %s is not authorized; run 'msgvault add-calendar %s' to grant %s",
		accountEmail,
		accountEmail,
		oauth.ScopeCalendarReadonly,
	)
}

// runConfiguredGCalSync runs one configured [[gcal]] source. It is shared by the
// sync-calendar CLI and the daemon scheduler (serve), which passes its single
// Store. The first run full-syncs (and registers calendars); later runs are
// incremental. Embedding is picked up later by scan-and-fill via embed_gen=NULL.
func runConfiguredGCalSync(ctx context.Context, st *store.Store, src config.GCalSource) error {
	email := normalizeCalendarAccountEmail(src.Email)
	if email == "" {
		return fmt.Errorf("gcal source %q email is required", src.Name)
	}

	existing, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, email)
	if err != nil {
		return fmt.Errorf("load registered calendar sources for %s: %w", email, err)
	}
	appDecision, err := calendarSyncOAuthAppDecision(st, email, existing, src.OAuthApp, src.OAuthApp != "")
	if err != nil {
		return err
	}

	client, err := buildCalendarClient(ctx, email, appDecision.OAuthApp, false)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	syncer := calsync.New(client, st, calsync.Options{
		AccountEmail: email,
		OAuthApp:     appDecision.OAuthApp,
		OAuthAppSet:  appDecision.OAuthAppSet,
		Calendars:    src.Calendars,
	}).WithLogger(logger)

	if calendarSyncShouldRunFullForSources(existing, false, false, "", src.Calendars, false) {
		_, err = syncer.Full(ctx)
	} else {
		_, err = syncer.Incremental(ctx)
	}
	if err != nil {
		return err
	}
	rebuildCacheAfterScheduledSync(ctx, "gcal:"+src.Name)
	return nil
}

func calendarDateBounds(cmd *cobra.Command, after, before string) (string, string, error) {
	var tmin, tmax string
	if after != "" {
		t, err := time.Parse("2006-01-02", after)
		if err != nil {
			return "", "", usageErr(cmd, fmt.Errorf("invalid --after %q (expected YYYY-MM-DD): %w", after, err))
		}
		tmin = t.UTC().Format(time.RFC3339)
	}
	if before != "" {
		t, err := time.Parse("2006-01-02", before)
		if err != nil {
			return "", "", usageErr(cmd, fmt.Errorf("invalid --before %q (expected YYYY-MM-DD): %w", before, err))
		}
		tmax = t.UTC().Format(time.RFC3339)
	}
	return tmin, tmax, nil
}

func calendarLabel(c gcal.Calendar) string {
	if c.Summary != "" {
		return c.Summary + " [" + c.ID + "]"
	}
	return c.ID
}
