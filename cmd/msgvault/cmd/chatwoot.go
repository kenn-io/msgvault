package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/chatwoot"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

const chatwootConfigHint = `Add a [[chatwoot]] profile with identifier, url, account_id and api_key_env to config.toml.
Set the named token environment variable on the daemon host`

var (
	rebuildChatwootCacheAfterWrite         = rebuildCacheAfterManualSync
	rebuildChatwootCacheAfterScheduledSync = rebuildCacheAfterScheduledSync
)

type chatwootRunOptions struct {
	Inboxes []int64
	Limit   int
	Full    bool
	NoMedia bool
}

func resolveChatwootProfiles(args []string, cfg *config.Config) ([]config.ChatwootSource, error) {
	if len(cfg.Chatwoot) == 0 {
		return nil, errors.New("no [[chatwoot]] profiles configured\n\n" + chatwootConfigHint)
	}
	if len(args) > 0 {
		src := cfg.GetChatwootSource(args[0])
		if src == nil {
			return nil, fmt.Errorf("no [[chatwoot]] profile with identifier %q", args[0])
		}
		return []config.ChatwootSource{*src}, nil
	}
	return cfg.Chatwoot, nil
}

func configuredChatwootClient(src config.ChatwootSource) (*chatwoot.Client, error) {
	name := src.APIKeyEnv
	if name == "" {
		name = "MSGVAULT_CHATWOOT_TOKEN"
	}
	token := os.Getenv(name)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("chatwoot profile %q needs %s in the daemon environment", src.Identifier, name)
	}
	return chatwoot.NewClient(src.URL, src.AccountID, token)
}

func registerChatwootProfile(ctx context.Context, st *store.Store, src config.ChatwootSource) ([]*store.Source, error) {
	client, err := configuredChatwootClient(src)
	if err != nil {
		return nil, err
	}
	inboxes, err := client.ListInboxes(ctx)
	if err != nil {
		return nil, fmt.Errorf("discover Chatwoot inboxes: %w", err)
	}
	var ids []int64
	for _, inbox := range inboxes {
		if src.InboxIncluded(inbox.ID) {
			ids = append(ids, inbox.ID)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("no accessible Chatwoot inboxes selected by this profile")
	}
	return chatwoot.NewImporter(st, client).Register(ctx, ids)
}

func resolveChatwootSyncInboxes(st *store.Store, src config.ChatwootSource, requested []int64) ([]int64, error) {
	sources, err := st.ListSources(chatwoot.SourceType)
	if err != nil {
		return nil, fmt.Errorf("list registered Chatwoot sources: %w", err)
	}
	canonical, err := chatwoot.CanonicalURL(src.URL)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("%s/accounts/%d/inboxes/", canonical, src.AccountID)
	registered := map[int64]bool{}
	var ids []int64
	for _, source := range sources {
		value, ok := strings.CutPrefix(source.Identifier, prefix)
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil || id <= 0 || source.Identifier != chatwoot.SourceIdentifier(canonical, src.AccountID, id) {
			continue
		}
		registered[id] = true
		if src.InboxIncluded(id) {
			ids = append(ids, id)
		}
	}
	if len(requested) > 0 {
		ids = nil
		for _, id := range requested {
			if !src.InboxIncluded(id) {
				return nil, fmt.Errorf("chatwoot inbox %d is excluded by profile %q", id, src.Identifier)
			}
			if !registered[id] {
				return nil, fmt.Errorf("chatwoot inbox %d is not registered; run msgvault add-chatwoot %s", id, src.Identifier)
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no included Chatwoot inboxes registered for %q; run msgvault add-chatwoot %s", src.Identifier, src.Identifier)
	}
	slices.Sort(ids)
	return ids, nil
}

func chatwootImportOptions(src config.ChatwootSource, inboxID int64, cfg *config.Config) chatwoot.ImportOptions {
	return chatwoot.ImportOptions{
		InboxID: inboxID, SelfAgentIDs: src.SelfAgentIDs, IncludePrivate: src.PrivateIncluded(),
		ReconcileInterval: src.ReconcileInterval(), Media: src.MediaEnabled(), MaxMediaBytes: src.MaxMediaBytes(),
		AttachmentsDir: cfg.AttachmentsDir(),
	}
}

func importChatwootProfile(ctx context.Context, st *store.Store, src config.ChatwootSource, run chatwootRunOptions, cfg *config.Config) (*chatwoot.ImportSummary, error) {
	ids, err := resolveChatwootSyncInboxes(st, src, run.Inboxes)
	if err != nil {
		return nil, err
	}
	client, err := configuredChatwootClient(src)
	if err != nil {
		return nil, err
	}
	imp := chatwoot.NewImporter(st, client)
	sum := &chatwoot.ImportSummary{}
	var errs []error
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		opts := chatwootImportOptions(src, id, cfg)
		opts.Limit, opts.Full, opts.NoMedia = run.Limit, run.Full, run.NoMedia
		got, err := imp.Import(ctx, opts)
		if got != nil {
			sum.Sources += got.Sources
			sum.MessagesAdded += got.MessagesAdded
			sum.MessagesProcessed += got.MessagesProcessed
			sum.Meetings += got.Meetings
			sum.MediaFailures += got.MediaFailures
			sum.Partial = sum.Partial || got.Partial
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("chatwoot inbox %d: %w", id, err))
		}
	}
	return sum, errors.Join(errs...)
}

func printChatwootSummary(out io.Writer, identifier string, sum *chatwoot.ImportSummary) {
	_, _ = fmt.Fprintf(out, "Chatwoot %s: %d inbox(es), %d messages processed (%d added), %d meetings, %d media failures\n", identifier, sum.Sources, sum.MessagesProcessed, sum.MessagesAdded, sum.Meetings, sum.MediaFailures)
	if sum.Partial {
		_, _ = fmt.Fprintln(out, "Unfinished history or artifact work will resume on the next sync.")
	}
}

func runConfiguredChatwootSync(ctx context.Context, st *store.Store, src config.ChatwootSource) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	sum, err := importChatwootProfile(ctx, st, src, chatwootRunOptions{}, state.cfg)
	if sum == nil {
		return err
	}
	job, ok := api.ChatwootJobNameForAccount(src.URL, src.AccountID)
	if !ok {
		return errors.Join(err, errors.New("invalid Chatwoot scheduler identity"))
	}
	return errors.Join(err, rebuildChatwootCacheAfterScheduledSync(context.WithoutCancel(ctx), job))
}

func registerScheduledChatwootJob(sched *scheduler.Scheduler, src config.ChatwootSource, maintenance *attachmentMaintenance, run func(context.Context) error) error {
	job, ok := api.ChatwootJobNameForAccount(src.URL, src.AccountID)
	if !ok {
		return errors.New("invalid Chatwoot scheduler identity")
	}
	return sched.AddJob(scheduler.Job{Name: job, Schedule: src.Schedule, Run: func(ctx context.Context) error {
		return runScheduledSource(ctx, maintenance, true, run)
	}})
}

func newAddChatwootCmd() *cobra.Command {
	return &cobra.Command{
		Use: "add-chatwoot [identifier]", Short: "Register accessible Chatwoot inboxes",
		Long: "Register inboxes selected by configured [[chatwoot]] profiles. With no identifier, process all profiles. Tokens are read from the daemon environment.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			cfg := state.cfg
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveChatwootProfiles(args, cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			var errs []error
			for _, src := range profiles {
				if cmd.Context().Err() != nil {
					errs = append(errs, cmd.Context().Err())
					break
				}
				sources, err := registerChatwootProfile(cmd.Context(), st, src)
				if err != nil {
					errs = append(errs, fmt.Errorf("chatwoot profile %s: %w", src.Identifier, err))
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Registered %d Chatwoot inbox(es) for %s\n", len(sources), src.Identifier)
			}
			if err := runPostSourceCreateMigrationsForInvocation(st, state); err != nil {
				errs = append(errs, fmt.Errorf("post-source-create migrations: %w", err))
			}
			return errors.Join(errs...)
		}}
}

func newSyncChatwootCmd() *cobra.Command {
	run := chatwootRunOptions{}
	cmd := &cobra.Command{
		Use: "sync-chatwoot [identifier]", Short: "Sync Chatwoot messages, media and call meetings",
		Long: "Sync all selected registered inboxes. With no identifier, process all configured profiles. Interrupted history resumes; old audio and calls are reconciled independently.",
		Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if run.Limit < 0 {
				return usageErr(cmd, errors.New("--limit must be nonnegative"))
			}
			for _, id := range run.Inboxes {
				if id <= 0 {
					return usageErr(cmd, errors.New("--inbox IDs must be positive"))
				}
			}
			state := invocationFromCommand(cmd)
			if state == nil || state.cfg == nil {
				return errors.New("configuration is unavailable")
			}
			cfg := state.cfg
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			profiles, err := resolveChatwootProfiles(args, cfg)
			if err != nil {
				return err
			}
			st, cleanup, err := openWritableStoreAndInitForIngestInvocation(state)
			if err != nil {
				return err
			}
			defer cleanup()
			ctx, stop := withInterruptCancel(cmd, "\nInterrupted. Saving Chatwoot checkpoints...")
			defer stop()
			var errs []error
			for _, src := range profiles {
				if err := ctx.Err(); err != nil {
					errs = append(errs, err)
					break
				}
				sum, err := importChatwootProfile(ctx, st, src, run, cfg)
				if sum != nil {
					printChatwootSummary(cmd.OutOrStdout(), src.Identifier, sum)
				}
				if err != nil {
					errs = append(errs, fmt.Errorf("chatwoot profile %s: %w", src.Identifier, err))
				}
			}
			// Refresh successful partial writes even after cancellation or another profile fails.
			if err := rebuildChatwootCacheAfterWrite(cfg.DatabaseDSN(), state); err != nil {
				errs = append(errs, err)
			}
			return errors.Join(errs...)
		}}
	cmd.Flags().Int64SliceVar(&run.Inboxes, "inbox", nil, "inbox ID to sync (repeatable; default: included registered inboxes)")
	cmd.Flags().IntVar(&run.Limit, "limit", 0, "max messages per conversation this run (0 = no limit)")
	cmd.Flags().BoolVar(&run.Full, "full", false, "reconcile all history and update existing rows in place")
	cmd.Flags().BoolVar(&run.NoMedia, "no-media", false, "skip attachment downloads this run")
	return cmd
}

func init() { rootCmd.AddCommand(newAddChatwootCmd(), newSyncChatwootCmd()) }
