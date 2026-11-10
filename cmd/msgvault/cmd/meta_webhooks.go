package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

// webhookPageProfile is the Page-token projection shared by comment and inbox
// sources. Inbox integration supplies its profiles without a dependency here.
type webhookPageProfile struct {
	Kind, Identifier, AppID, PageID, TokenEnv string
}

var additionalWebhookPageProfiles func(*config.Config) []webhookPageProfile

func resolveWebhookPageProfiles(ids []string, cfg *config.Config) ([]webhookPageProfile, error) {
	profiles := make([]webhookPageProfile, 0, len(cfg.FacebookComments))
	for _, p := range cfg.FacebookComments {
		profiles = append(profiles, webhookPageProfile{Kind: "facebook_comments", Identifier: p.Identifier, AppID: p.AppID, PageID: p.PageID, TokenEnv: p.TokenEnv})
	}
	if additionalWebhookPageProfiles != nil {
		profiles = append(profiles, additionalWebhookPageProfiles(cfg)...)
	}
	if len(ids) == 0 {
		if len(profiles) == 0 {
			return nil, errors.New("configure a Meta Page profile with identifier, app_id, page_id and token_env first")
		}
		return profiles, nil
	}
	var selected []webhookPageProfile
	var choices []string
	for _, p := range profiles {
		qualified := p.Kind + ":" + p.Identifier
		match := strings.EqualFold(ids[0], p.Identifier)
		if strings.Contains(ids[0], ":") {
			match = strings.EqualFold(ids[0], qualified)
		}
		if match {
			selected = append(selected, p)
			choices = append(choices, qualified)
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no Meta Page profile with identifier %q", ids[0])
	}
	if len(selected) > 1 {
		return nil, fmt.Errorf("ambiguous Meta Page profile %q; select %s", ids[0], strings.Join(choices, " or "))
	}
	return selected, nil
}

type subscribeResult struct {
	Kind       string   `json:"kind"`
	Identifier string   `json:"identifier"`
	AppID      string   `json:"app_id"`
	PageID     string   `json:"page_id"`
	Installed  bool     `json:"installed"`
	Fields     []string `json:"fields"`
}

func messengerFields(fields []string) bool {
	for _, field := range fields {
		if field != "feed" && field != "mention" {
			return true
		}
	}
	return false
}

func validateWebhookFields(fields []string) error {
	if len(fields) == 0 {
		return errors.New("--fields must select at least one Page webhook field")
	}
	for _, field := range fields {
		if !slices.Contains(meta.PageWebhookFields, field) {
			return fmt.Errorf("unsupported Page webhook field %q", field)
		}
	}
	return nil
}

func subscriptionError(err error, fields []string) error {
	var apiErr *meta.APIError
	if errors.As(err, &apiErr) && apiErr.Permission() {
		permissions := "pages_manage_metadata and pages_show_list"
		if messengerFields(fields) {
			permissions += " and pages_messaging"
		}
		return fmt.Errorf("meta refused subscribed_apps (code %d): the Page token needs %s from a person with a MODERATE task on the Page", apiErr.Code, permissions)
	}
	return err
}

func subscribeMetaWebhooks(ctx context.Context, cfg *config.Config, ids, fields []string, check bool, newClient func(string) (*meta.Client, error)) ([]subscribeResult, error) {
	if err := validateWebhookFields(fields); err != nil {
		return nil, err
	}
	profiles, err := resolveWebhookPageProfiles(ids, cfg)
	if err != nil {
		return nil, err
	}
	results, errs := []subscribeResult{}, []error{}
	for _, profile := range profiles {
		wanted := slices.Clone(fields)
		result := subscribeResult{Kind: profile.Kind, Identifier: profile.Identifier, AppID: profile.AppID, PageID: profile.PageID, Fields: []string{}}
		err := func() error {
			token := os.Getenv(profile.TokenEnv)
			if strings.TrimSpace(token) == "" {
				return fmt.Errorf("needs %s in the daemon environment", profile.TokenEnv)
			}
			client, err := newClient(token)
			if err != nil {
				return errors.New("cannot create the configured Page client")
			}
			app, err := client.TokenApp(ctx)
			if err != nil {
				return fmt.Errorf("cannot verify the Page token app; refusing to change its subscriptions: %w", subscriptionError(err, fields))
			}
			if app != profile.AppID {
				return errors.New("the Page token belongs to a different app; refusing to change its subscriptions")
			}
			result.Installed, result.Fields, err = client.SubscribedFields(ctx, profile.PageID, profile.AppID)
			if err != nil {
				return subscriptionError(err, fields)
			}
			if !check {
				union := append(slices.Clone(result.Fields), fields...)
				slices.Sort(union)
				union = slices.Compact(union)
				if err = client.Subscribe(ctx, profile.PageID, fields, result.Fields); err != nil {
					return subscriptionError(err, union)
				}
				result.Installed, result.Fields, err = client.SubscribedFields(ctx, profile.PageID, profile.AppID)
				if err != nil {
					return subscriptionError(err, fields)
				}
				// Re-read must confirm the entire union, including existing fields.
				wanted = union
			}
			if !result.Installed {
				return errors.New("app is not installed on the Page")
			}
			for _, field := range wanted {
				if !slices.Contains(result.Fields, field) {
					return fmt.Errorf("missing requested webhook field %q after subscription check", field)
				}
			}
			return nil
		}()
		if result.Fields == nil {
			result.Fields = []string{}
		}
		slices.Sort(result.Fields)
		results = append(results, result)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s profile %q: %w", profile.Kind, profile.Identifier, err))
		}
	}
	if len(results) == 0 {
		return results, errors.New("no Meta Page profiles selected")
	}
	return results, errors.Join(errs...)
}

func newSubscribeMetaWebhooksCmd() *cobra.Command {
	var fields []string
	var check, asJSON bool
	cmd := &cobra.Command{Use: "subscribe [identifier]", Short: "Manage Meta Page subscriptions", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateWebhookFields(fields); err != nil {
			return metaWebhookCommandError(cmd, err, 3)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return metaWebhookCommandError(cmd, errors.New("configuration is unavailable"), 5)
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		if _, err := resolveWebhookPageProfiles(args, state.cfg); err != nil {
			exit := 5
			if len(args) > 0 {
				exit = 4
			}
			return metaWebhookCommandError(cmd, err, exit)
		}
		if messengerFields(fields) {
			_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Messenger fields need pages_messaging. Confirm primary receiver routing before subscribing; another app may own the conversation.")
		}
		results, err := subscribeMetaWebhooks(cmd.Context(), state.cfg, args, fields, check, meta.NewClient)
		err = metaWebhookCommandError(cmd, err, 7)
		if asJSON {
			return errors.Join(err, writeWebhookJSON(cmd, results))
		}
		for _, result := range results {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s %s: installed=%t fields=%s\n", result.Kind, result.Identifier, result.Installed, strings.Join(result.Fields, ","))
		}
		return err
	}}
	cmd.Flags().StringSliceVar(&fields, "fields", []string{"feed"}, "Page fields to subscribe (comma-separated; feed and mention supported; Messenger fields require explicit selection)")
	cmd.Flags().BoolVar(&check, "check", false, "check installed fields without changing subscriptions")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

type webhookStatusCounters struct {
	Accepted          uint64    `json:"accepted"`
	Duplicates        uint64    `json:"duplicates"`
	SignatureFailures uint64    `json:"signature_failures"`
	TooLarge          uint64    `json:"too_large"`
	Unavailable       uint64    `json:"unavailable"`
	LastAccepted      time.Time `json:"last_accepted"`
}
type webhookAppStatus struct {
	AppID                   string         `json:"app_id"`
	Pending                 int            `json:"pending"`
	Held                    int            `json:"held"`
	Failed                  int            `json:"failed"`
	Done                    int            `json:"done"`
	OldestPendingAgeSeconds int64          `json:"oldest_pending_age_seconds"`
	Bytes                   int64          `json:"bytes"`
	HeldOutcomes            map[string]int `json:"held_outcomes"`
}
type webhookPageStatus struct {
	Identifier          string     `json:"identifier"`
	AppID               string     `json:"app_id"`
	PageID              string     `json:"page_id"`
	LastPollCompletedAt *time.Time `json:"last_poll_completed_at"`
}
type statusReport struct {
	Listener       string                `json:"listener"`
	Addr           string                `json:"addr"`
	DisabledReason string                `json:"disabled_reason"`
	WrittenAt      time.Time             `json:"written_at"`
	Counters       webhookStatusCounters `json:"counters"`
	Apps           []webhookAppStatus    `json:"apps"`
	Pages          []webhookPageStatus   `json:"pages"`
	SpoolBytes     int64                 `json:"spool_bytes"`
	MaxSpoolBytes  int64                 `json:"max_spool_bytes"`
	Errors         []string              `json:"errors"`
}

func metaWebhooksStatus(ctx context.Context, cfg *config.Config, st *store.Store) statusReport {
	report := statusReport{Listener: "daemon not running or no snapshot", Apps: []webhookAppStatus{}, Pages: []webhookPageStatus{}, Errors: []string{}}
	_, report.MaxSpoolBytes, _ = cfg.MetaWebhooks.Effective()
	root := filepath.Join(cfg.Data.DataDir, "meta-webhooks")
	snap, err := metawebhook.ReadReceiverSnapshot(root)
	if err == nil {
		report.WrittenAt = snap.WrittenAt
		if !snap.WrittenAt.IsZero() && time.Since(snap.WrittenAt) <= 2*time.Minute {
			report.Listener, report.Addr, report.DisabledReason = snap.Listener, snap.Addr, snap.DisabledReason
		}
		c := snap.Counters
		report.Counters = webhookStatusCounters{c.Accepted, c.Duplicates, c.SignatureFailures, c.TooLarge, c.Unavailable, c.LastAccepted}
	} else if !errors.Is(err, os.ErrNotExist) {
		report.Errors = append(report.Errors, "receiver snapshot unavailable")
	}
	stats, err := metawebhook.ReadStats(root)
	if err != nil {
		report.Errors = append(report.Errors, "spool statistics unavailable")
	}
	if stats == nil {
		stats = map[string]metawebhook.AppStats{}
	}
	for _, app := range cfg.MetaWebhooks.Apps {
		if _, ok := stats[app.AppID]; !ok {
			stats[app.AppID] = metawebhook.AppStats{}
		}
	}
	keys := make([]string, 0, len(stats))
	for key := range stats {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		a := stats[key]
		age := int64(0)
		if !a.OldestPending.IsZero() {
			age = max(0, int64(time.Since(a.OldestPending).Seconds()))
		}
		if a.HeldOutcomes == nil {
			a.HeldOutcomes = map[string]int{}
		}
		report.Apps = append(report.Apps, webhookAppStatus{key, a.Pending, a.Held, a.Failed, a.Done, age, a.Bytes, a.HeldOutcomes})
		report.SpoolBytes += a.Bytes
	}
	if st == nil {
		report.Errors = append(report.Errors, "polling archive unavailable")
	}
	for _, profile := range cfg.FacebookComments {
		page := webhookPageStatus{Identifier: profile.Identifier, AppID: profile.AppID, PageID: profile.PageID}
		if st != nil && ctx.Err() == nil {
			source, err := st.GetSourceByTypeAndIdentifier(meta.SourceType, meta.SourceIdentifier(profile.AppID, profile.PageID))
			if err == nil {
				run, err := st.GetLastSuccessfulSyncByTypeContext(ctx, source.ID, meta.SourceType)
				if err == nil && run.CompletedAt.Valid {
					page.LastPollCompletedAt = &run.CompletedAt.Time
				} else if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
					report.Errors = append(report.Errors, "polling run unavailable for "+profile.Identifier)
				}
			} else if !errors.Is(err, store.ErrSourceNotFound) {
				report.Errors = append(report.Errors, "polling source unavailable for "+profile.Identifier)
			}
		}
		report.Pages = append(report.Pages, page)
	}
	return report
}

func newMetaWebhooksStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "status", Short: "Show Meta receiver status", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return metaWebhookCommandError(cmd, errors.New("configuration is unavailable"), 5)
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		st, _ := store.OpenReadOnlyContext(cmd.Context(), state.cfg.DatabaseDSN())
		if st != nil {
			defer func() { _ = st.Close() }()
		}
		report := metaWebhooksStatus(cmd.Context(), state.cfg, st)
		if asJSON {
			return writeWebhookJSON(cmd, report)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Receiver: %s %s\nSpool: %d / %d bytes\n", report.Listener, report.Addr, report.SpoolBytes, report.MaxSpoolBytes)
		if report.DisabledReason != "" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), report.DisabledReason)
		}
		for _, app := range report.Apps {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "App %s: pending=%d held=%d failed=%d done=%d oldest_pending=%ds bytes=%d held_outcomes=%v\n", app.AppID, app.Pending, app.Held, app.Failed, app.Done, app.OldestPendingAgeSeconds, app.Bytes, app.HeldOutcomes)
		}
		c := report.Counters
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Since start: accepted=%d duplicates=%d signature_failures=%d too_large=%d unavailable=%d last_accepted=%s\n", c.Accepted, c.Duplicates, c.SignatureFailures, c.TooLarge, c.Unavailable, c.LastAccepted.Format(time.RFC3339))
		for _, page := range report.Pages {
			last := "none"
			if page.LastPollCompletedAt != nil {
				last = page.LastPollCompletedAt.Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Facebook %s: last completed polling run=%s\n", page.Identifier, last)
		}
		for _, problem := range report.Errors {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), problem)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func replayMetaWebhooks(ctx context.Context, root string, filter metawebhook.RequeueFilter, dryRun bool) (int, error) {
	id, err := metawebhook.SubmitRequeue(root, filter, dryRun)
	if err != nil {
		return 0, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	n, err := metawebhook.AwaitRequeueResult(waitCtx, root, id)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return 0, fmt.Errorf("no replay result; daemon not running or worker unavailable (request may still execute): %w", err)
	}
	return n, err
}

func newReplayMetaWebhooksCmd() *cobra.Command {
	var filter metawebhook.RequeueFilter
	var states []string
	var since string
	var dryRun, asJSON bool
	cmd := &cobra.Command{Use: "replay", Short: "Requeue Meta deliveries", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if filter.AppID != "" && !meta.ValidAccountID(filter.AppID) {
			return metaWebhookCommandError(cmd, errors.New("--app must be a decimal app ID"), 3)
		}
		if filter.DeliveryID != "" {
			_, err := hex.DecodeString(filter.DeliveryID)
			if err != nil || len(filter.DeliveryID) != 64 || strings.ToLower(filter.DeliveryID) != filter.DeliveryID {
				return metaWebhookCommandError(cmd, errors.New("--delivery must be a lowercase SHA-256 delivery ID"), 3)
			}
		}
		filter.States = nil
		for _, state := range states {
			if metawebhook.State(state) != metawebhook.StateHeld && metawebhook.State(state) != metawebhook.StateFailed && metawebhook.State(state) != metawebhook.StateDone {
				return metaWebhookCommandError(cmd, errors.New("--state must be held, failed or done"), 3)
			}
			filter.States = append(filter.States, metawebhook.State(state))
		}
		if len(filter.States) == 0 {
			return metaWebhookCommandError(cmd, errors.New("--state must select at least one state"), 3)
		}
		if since != "" {
			var err error
			filter.Since, err = time.Parse(time.RFC3339, since)
			if err != nil {
				return metaWebhookCommandError(cmd, errors.New("--since must be RFC3339"), 3)
			}
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return metaWebhookCommandError(cmd, errors.New("configuration is unavailable"), 5)
		}
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		n, err := replayMetaWebhooks(cmd.Context(), filepath.Join(state.cfg.Data.DataDir, "meta-webhooks"), filter, dryRun)
		if err != nil {
			return metaWebhookCommandError(cmd, err, 7)
		}
		if asJSON {
			return writeWebhookJSON(cmd, struct {
				Count  int  `json:"count"`
				DryRun bool `json:"dry_run"`
			}{n, dryRun})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Matching deliveries: %d (dry-run=%t)\n", n, dryRun)
		if err != nil {
			return fmt.Errorf("write replay result: %w", err)
		}
		return nil
	}}
	cmd.Flags().StringVar(&filter.AppID, "app", "", "filter by app ID")
	cmd.Flags().StringSliceVar(&states, "state", []string{"held", "failed"}, "states to replay (repeatable or comma-separated)")
	cmd.Flags().StringVar(&filter.DeliveryID, "delivery", "", "filter by delivery ID")
	cmd.Flags().StringVar(&since, "since", "", "filter by receipt time (RFC3339)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "count matching deliveries without moving them")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func writeWebhookJSON(cmd *cobra.Command, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), string(body))
	if err != nil {
		return fmt.Errorf("write webhook JSON: %w", err)
	}
	return nil
}

func newMetaWebhooksCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "meta-webhooks", Short: "Manage Meta webhook delivery"}
	cmd.AddCommand(newSubscribeMetaWebhooksCmd(), newMetaWebhooksStatusCmd(), newReplayMetaWebhooksCmd())
	return cmd
}

type metaWebhookCLIError struct {
	err  error
	hint string
	exit int
}

func (e *metaWebhookCLIError) Error() string { return e.err.Error() }
func (e *metaWebhookCLIError) Unwrap() error { return e.err }
func (e *metaWebhookCLIError) Hint() string  { return e.hint }
func (e *metaWebhookCLIError) ExitCode() int { return e.exit }

// The shared CLI error renderer recognizes ExitCode and Hint after the command
// grammar change lands. Keep these errors compatible with that classifier.
func metaWebhookCommandError(cmd *cobra.Command, err error, exit int) error {
	if err == nil {
		return nil
	}
	return &metaWebhookCLIError{err: err, exit: exit, hint: "run 'msgvault meta-webhooks " + cmd.Name() + " --help' for flags and examples"}
}

func init() { rootCmd.AddCommand(newMetaWebhooksCmd()) }
