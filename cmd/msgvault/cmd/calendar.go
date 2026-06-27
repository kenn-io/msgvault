package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"

	"go.kenn.io/msgvault/internal/calsync"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

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
	cmd := &cobra.Command{
		Use:   "add-calendar <email>",
		Short: "Authorize Google Calendar access and register calendars for an account",
		Long: "Grants read-only Calendar access (calendar.readonly) to an account and " +
			"registers its calendars for sync. If the account already has a Gmail token, " +
			"re-consent bundles Gmail + Calendar together; keep BOTH checked on the consent " +
			"screen so Gmail access is not dropped.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email := args[0]
			ctx := cmd.Context()
			oauthAppExplicit := cmd.Flags().Changed("oauth-app")
			if err := calsync.ValidateMinAccessRole(calAddMinRole); err != nil {
				return usageErr(cmd, err)
			}

			st, err := openCalendarStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

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

			// A headless host cannot complete Google's browser consent, and the
			// OAuth device flow does not support Calendar scopes. If this account
			// still needs Calendar authorization, mirror add-account --headless:
			// print copy-the-token instructions and stop — without launching a
			// browser or touching the existing Gmail token. Once the dual-scope
			// token is copied in, re-running add-calendar --headless skips this
			// and registers the calendars (an API call that needs no browser).
			if calAddHeadless && (!hasToken || !hasCalendarScope || !tokenReusable) {
				oauth.PrintCalendarHeadlessInstructions(email, cfg.TokensDir(), oauthApp)
				return nil
			}

			switch {
			case !hasToken:
				fmt.Printf("Authorizing %s for Calendar...\n", email)
				if err := mgr.Authorize(ctx, email); err != nil {
					return wrapOAuthError(err)
				}
			case !hasCalendarScope:
				body := []string{
					"Calendar sync needs read-only Calendar access.",
					"",
					"Re-authorizing REPLACES the granted scopes, so msgvault will",
					"re-request Gmail, Calendar, and any already granted Google",
					"scopes together. On the consent screen, keep every existing",
					"permission checked or Google will remove that access for this",
					"account.",
				}
				existingScopes := mgr.GrantedScopes(email)
				requiredScopes := calendarEscalationScopes(existingScopes,
					calendarShouldPreserveGmail(hasToken, mgr.HasScopeMetadata(email), existingScopes))
				if err := promptScopeEscalation(ctx, email, requiredScopes,
					"CALENDAR ACCESS REQUIRED", body,
					"Cancelled. Calendar was not added.", secretsPath); err != nil {
					if errors.Is(err, errUserCanceled) {
						return nil
					}
					return err
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
			fmt.Printf("\nNext: %s\n", calendarSyncNextCommand(email, oauthApp))
			return nil
		},
	}
	cmd.Flags().StringVar(&calAddOAuthApp, "oauth-app", "", "named OAuth app to use")
	cmd.Flags().BoolVar(&calAddHeadless, "headless", false, "headless host: print token-copy instructions instead of opening a browser")
	cmd.Flags().BoolVar(&calAddAll, "all-calendars", false, "include reader/freeBusyReader calendars (default: owner+writer)")
	cmd.Flags().StringVar(&calAddMinRole, "min-access-role", "", "minimum access role: owner|writer|reader")
	cmd.Flags().StringSliceVar(&calAddCalendars, "calendars", nil, "comma-separated calendar IDs to register (default: by access role)")
	return cmd
}

func newSyncCalendarCmd() *cobra.Command {
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

			email := args[0]
			oauthApp := calSyncOAuthApp
			calendars := calSyncCalendars
			if src := cfg.GetGCalSource(args[0]); src != nil {
				email = src.Email
				if oauthApp == "" {
					oauthApp = src.OAuthApp
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

			st, err := openCalendarStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			existing, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, email)
			if err != nil {
				return fmt.Errorf("load registered calendar sources for %s: %w", email, err)
			}
			if oauthApp == "" {
				oauthApp = calendarStoredOAuthApp(existing)
			}

			client, err := buildCalendarClient(ctx, email, oauthApp, interactiveStdin())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			syncer := calsync.New(client, st, calsync.Options{
				AccountEmail:  email,
				OAuthApp:      oauthApp,
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
	sources, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, email)
	if err != nil {
		return calendarAddOAuthApp{}, fmt.Errorf("load registered calendar sources for %s: %w", email, err)
	}

	resolvedApp := requestedApp
	if !explicit && resolvedApp == "" {
		resolvedApp = calendarStoredOAuthApp(sources)
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

func calendarSyncNextCommand(email, oauthApp string) string {
	if oauthApp != "" {
		return "msgvault sync-calendar --oauth-app " + oauthApp + " " + email
	}
	return "msgvault sync-calendar " + email
}

// openCalendarStore opens the main store and runs schema init + startup
// migrations, matching the other ingest commands.
func openCalendarStore() (*store.Store, error) {
	st, err := store.Open(cfg.DatabaseDSN())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := st.InitSchema(); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(st); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("startup migrations: %w", err)
	}
	return st, nil
}

// buildCalendarClient constructs a gcal.API client. The OAuth token is keyed on
// the account email (never a calendar source identifier). If reauth is needed,
// it preserves Gmail only for existing Gmail/legacy tokens; Calendar-only tokens
// stay Calendar-only. The limiter is sized for the Calendar per-user budget.
func buildCalendarClient(ctx context.Context, accountEmail, oauthApp string, interactive bool) (gcal.API, error) {
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
		tokenSource, err = getTokenSourceWithReauth(ctx, mgr, accountEmail, interactive)
		if err != nil {
			return nil, err
		}
	}

	return gcal.NewClient(tokenSource,
		gcal.WithLogger(logger),
		gcal.WithRateLimiter(gmail.NewRateLimiterWithCapacity(10, 8)),
	), nil
}

// runConfiguredGCalSync runs one configured [[gcal]] source. It is shared by the
// sync-calendar CLI and the daemon scheduler (serve), which passes its single
// Store. The first run full-syncs (and registers calendars); later runs are
// incremental. Embedding is picked up later by scan-and-fill via embed_gen=NULL.
func runConfiguredGCalSync(ctx context.Context, st *store.Store, src config.GCalSource) error {
	if src.Email == "" {
		return fmt.Errorf("gcal source %q email is required", src.Name)
	}
	client, err := buildCalendarClient(ctx, src.Email, src.OAuthApp, false)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	syncer := calsync.New(client, st, calsync.Options{
		AccountEmail: src.Email,
		OAuthApp:     src.OAuthApp,
		Calendars:    src.Calendars,
	}).WithLogger(logger)

	existing, err := st.GetSourcesByTypeAndAccount(sourceTypeCalendar, src.Email)
	if err != nil {
		return fmt.Errorf("load registered calendar sources for %s: %w", src.Email, err)
	}
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
