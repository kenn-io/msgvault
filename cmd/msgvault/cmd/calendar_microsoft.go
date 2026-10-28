package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/mscalendar"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
)

// msCalendarQPS stays under Graph's 10,000 requests per 10 minutes per mailbox.
const msCalendarQPS = 15

func newGraphCalendarManager(state *invocation) *microsoft.GraphManager {
	cfg := state.cfg
	return microsoft.NewGraphCalendarManager(
		cfg.Microsoft.ClientID,
		cfg.Microsoft.EffectiveTenantID(),
		cfg.Microsoft.EffectiveRedirectURI(),
		cfg.TokensDir(),
		state.logger,
	)
}

func authorizeMSCalendar(cmd *cobra.Command, email string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if err := requireMicrosoftOAuthConfig(state.cfg); err != nil {
		return err
	}
	mgr := newGraphCalendarManager(state)
	if calAddHeadless {
		mgr.UseDeviceCode()
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Authorizing %s with Microsoft...\n", email)
	if err := mgr.Authorize(cmd.Context(), email); err != nil {
		return fmt.Errorf("authorization failed: %w", err)
	}
	return nil
}

// rejectMSCalendarAddFlags refuses Google-only add-calendar options before
// sign-in. A Microsoft account registers every calendar the mailbox owns.
func rejectMSCalendarAddFlags(cmd *cobra.Command) error {
	for _, name := range []string{"calendars", "all-calendars", "min-access-role", "write", "oauth-app"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s is not supported with --microsoft", name)
		}
	}
	return nil
}

// preflightAddCalendarMicrosoft signs in from the frontend CLI, so the daemon
// subprocess never waits on a browser. See preflightAddO365Authorize.
func preflightAddCalendarMicrosoft(cmd *cobra.Command, email string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if err := rejectMSCalendarAddFlags(cmd); err != nil {
		return usageErr(cmd, err)
	}
	if IsRemoteMode(state) {
		return nil
	}
	if err := authorizeMSCalendar(cmd, normalizeCalendarAccountEmail(email)); err != nil {
		return err
	}
	if err := cmd.Flags().Set(oauthPreflightedFlag, "true"); err != nil {
		return fmt.Errorf("set --%s after authorization: %w", oauthPreflightedFlag, err)
	}
	return nil
}

func runAddCalendarMicrosoftLocal(cmd *cobra.Command, email string) error {
	state := invocationFromCommand(cmd)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	if err := rejectMSCalendarAddFlags(cmd); err != nil {
		return usageErr(cmd, err)
	}
	preflighted, err := oauthPreflighted(cmd)
	if err != nil {
		return err
	}
	if !preflighted {
		if err := authorizeMSCalendar(cmd, email); err != nil {
			return err
		}
	}
	st, cleanup, err := openWritableStoreAndInitForInvocation(state)
	if err != nil {
		return err
	}
	defer cleanup()
	client, err := newMSCalendarClient(cmd.Context(), email, state)
	if err != nil {
		return err
	}
	_, cals, err := mscalendar.Register(cmd.Context(), st, client, email)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Registered %d Microsoft calendar(s) for %s:\n", len(cals), email)
	for _, cal := range cals {
		_, _ = fmt.Fprintf(out, "  %s\n", cal.Name)
	}
	_, _ = fmt.Fprintf(out, "\nNext: msgvault sync-calendar %s\n", email)
	return nil
}

func newMSCalendarClient(ctx context.Context, email string, state *invocation) (*mscalendar.Client, error) {
	tokenFn, err := newGraphCalendarManager(state).TokenSource(ctx, email)
	if err != nil {
		return nil, err
	}
	return mscalendar.NewClient(msgraph.GraphBaseURL, tokenFn, msCalendarQPS), nil
}

// rejectMSCalendarSyncFlags refuses Google-only selections. A Microsoft sync
// always reads every owned calendar in full, so these flags would be ignored.
func rejectMSCalendarSyncFlags(cmd *cobra.Command) error {
	for _, name := range []string{"calendar", "limit", "after", "before", "all-calendars", "min-access-role", "noresume", "oauth-app"} {
		if cmd.Flags().Changed(name) {
			return fmt.Errorf("--%s is not supported for Microsoft calendars", name)
		}
	}
	return nil
}

func hasMSCalendarSources(st *store.Store, email string) (bool, error) {
	srcs, err := st.GetSourcesByTypeAndAccount(mscalendar.SourceType, email)
	if err != nil {
		return false, fmt.Errorf("load Microsoft calendar sources for %s: %w", email, err)
	}
	// Retired sources still mark a Microsoft account, so its sync never falls
	// through to Google sign-in.
	return len(srcs) > 0, nil
}

// runMSCalendarSync syncs the registered Microsoft calendars of an account.
// With discover, it first registers calendars that the mailbox owns.
func runMSCalendarSync(ctx context.Context, st *store.Store, email string, discover bool, state *invocation) (*mscalendar.Summary, error) {
	client, err := newMSCalendarClient(ctx, email, state)
	if err != nil {
		return nil, err
	}
	return mscalendar.Sync(ctx, st, client, email, discover, state.logger)
}

func runMSCalendarSyncAndReport(ctx context.Context, out io.Writer, st *store.Store, email string, discover bool, state *invocation) error {
	sum, err := runMSCalendarSync(ctx, st, email, discover, state)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Microsoft calendar sync complete for %s\n", email)
	_, _ = fmt.Fprintf(out, "  Calendars: %d\n  Events:    %d\n", sum.Calendars, sum.Events)
	if sum.Errors > 0 {
		_, _ = fmt.Fprintf(out, "  Errors:    %d\n", sum.Errors)
	}
	_, _ = fmt.Fprintf(out, "  Duration:  %s\n", sum.Duration.Round(time.Second))
	return nil
}
