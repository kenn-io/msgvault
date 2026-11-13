package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

type gmailInventoryOptions struct {
	SourceID    int64
	Limit       int
	MaxMessages int
	MaxPages    int
	Timeout     time.Duration
	JSON        bool
}

type gmailInventoryDifference struct {
	Count   int      `json:"count"`
	IDs     []string `json:"ids"`
	Omitted int      `json:"omitted"`
}

type gmailInventoryResult struct {
	SourceID              int64                     `json:"source_id"`
	Status                string                    `json:"status"`
	Reason                string                    `json:"reason"`
	Scope                 string                    `json:"scope"`
	LocalSnapshotAt       time.Time                 `json:"local_snapshot_at"`
	RemoteStartedAt       time.Time                 `json:"remote_started_at"`
	RemoteFinishedAt      time.Time                 `json:"remote_finished_at"`
	LocalComplete         bool                      `json:"local_complete"`
	RemoteComplete        bool                      `json:"remote_complete"`
	Pages                 int                       `json:"pages"`
	RemoteObservations    int                       `json:"remote_observations"`
	DuplicateObservations int                       `json:"duplicate_observations"`
	LocalCount            int                       `json:"local_count"`
	RemoteCount           int                       `json:"remote_count"`
	SharedCount           int                       `json:"shared_count"`
	ExcludedArchiveOnly   int                       `json:"excluded_archive_only"`
	IncludedHidden        int                       `json:"included_hidden"`
	RemoteOnly            *gmailInventoryDifference `json:"remote_only"`
	LocalNotObserved      *gmailInventoryDifference `json:"local_not_observed"`
	LocalUnassessed       int                       `json:"local_unassessed"`
	HistoryBefore         uint64                    `json:"history_before"`
	HistoryAfter          uint64                    `json:"history_after"`
	Limits                gmailInventoryLimits      `json:"limits"`
	Caveats               []string                  `json:"caveats"`
}

type gmailInventoryLimits struct {
	Messages int `json:"messages"`
	Pages    int `json:"pages"`
	Details  int `json:"details"`
}

type gmailInventoryDeps struct {
	openStore func(context.Context) (*store.Store, func(), error)
	newClient func(context.Context, *store.Source) (gmail.API, error)
}

func defaultGmailInventoryDeps() gmailInventoryDeps {
	return gmailInventoryDeps{
		openStore: func(ctx context.Context) (*store.Store, func(), error) {
			return openReadOnlyStoreForInvocation(ctx, invocationFromContext(ctx))
		},
		newClient: func(ctx context.Context, source *store.Source) (gmail.API, error) {
			return buildAPIClient(ctx, source, oauthManagerCache(invocationFromContext(ctx)), nil)
		},
	}
}

func newDiagnoseGmailInventoryCmd(deps gmailInventoryDeps) *cobra.Command {
	opts := gmailInventoryOptions{Limit: 100, MaxMessages: 1000000, MaxPages: 10000, Timeout: 30 * time.Minute}
	command := &cobra.Command{
		Use:          "diagnose-gmail-inventory --source-id <id>",
		SilenceUsage: true,
		Short:        "Compare Gmail message IDs with one archived source",
		Long: `Compare message identities without syncing or repairing the archive.
Gmail is listed without search or label filters, including Spam and Trash.
Local presence includes hidden and remotely deleted rows. Merge-only history
is excluded. Presence does not verify RAW, attachments, projections or files.

A complete result means pagination ended and the local set was read. Gmail
and SQL are not one atomic snapshot; differences may be transient. A changed
Gmail history ID makes the completed traversal inconclusive. An interrupted
listing never establishes that an unobserved local ID is absent from Gmail.

--limit bounds each list of IDs, not collection. Counts and omitted totals
remain visible. Resource limits stop collection with a non-success result.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.SourceID <= 0 || opts.Limit < 0 || opts.MaxMessages < 1 || opts.MaxMessages > 10000000 || opts.MaxPages < 1 || opts.MaxPages > 1000000 || opts.Timeout <= 0 {
				return usageErr(cmd, errors.New("require a positive --source-id, --max-messages (1..10000000), --max-pages (1..1000000), --timeout, and nonnegative --limit"))
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(cmd, args)
			}
			ctx, stop := withInterruptCancel(cmd, "")
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
			defer cancel()
			st, cleanup, err := deps.openStore(ctx)
			if err != nil {
				return err
			}
			defer cleanup()
			result, diagnoseErr := collectGmailInventory(ctx, st, deps.newClient, opts)
			if err := writeGmailInventory(cmd.OutOrStdout(), result, opts.JSON); err != nil {
				return err
			}
			return diagnoseErr
		},
	}
	command.Flags().Int64Var(&opts.SourceID, "source-id", 0, "one Gmail source ID (required)")
	command.Flags().IntVar(&opts.Limit, "limit", 100, "maximum IDs per difference; 0 prints counts only")
	command.Flags().IntVar(&opts.MaxMessages, "max-messages", 1000000, "maximum local rows and remote observations collected per side")
	command.Flags().IntVar(&opts.MaxPages, "max-pages", 10000, "maximum Gmail pages collected")
	command.Flags().DurationVar(&opts.Timeout, "timeout", 30*time.Minute, "maximum diagnostic duration")
	command.Flags().BoolVar(&opts.JSON, "json", false, "output one JSON object")
	return command
}

// collectGmailInventory uses the same provider listing as presence recovery,
// but never invokes the Syncer or its reconciliation and mutation paths.
func collectGmailInventory(ctx context.Context, st *store.Store, newClient func(context.Context, *store.Source) (gmail.API, error), opts gmailInventoryOptions) (gmailInventoryResult, error) {
	result := gmailInventoryResult{
		SourceID: opts.SourceID, Status: "inconclusive", Reason: "local_read_failed",
		Scope:  "all Gmail IDs including Spam/Trash; local canonical identities including hidden and remotely deleted rows; merge-only history excluded",
		Limits: gmailInventoryLimits{Messages: opts.MaxMessages, Pages: opts.MaxPages, Details: opts.Limit},
		Caveats: []string{
			"Gmail pagination and the local snapshot are not atomic; concurrent acquisition, deletion or source changes can make differences transient.",
			"Unchanged Gmail history IDs are an observation, not a transactional snapshot guarantee.",
			"Prior filtered or limited syncs may explain remote-only IDs; no historical query exclusions are inferred.",
			"Local-only IDs may be legitimate retained history. Presence and equal sets do not verify RAW, attachments, projections, FTS or physical files.",
		},
	}
	local, source, err := readGmailInventoryLocal(ctx, st, opts, &result)
	if err != nil {
		return result, err
	}
	client, err := newClient(ctx, source)
	if err != nil {
		result.Reason = "provider_unavailable"
		return result, fmt.Errorf("create Gmail inventory client: %w", err)
	}
	defer func() { _ = client.Close() }()
	lister, ok := client.(gmail.CompleteMessageSnapshotReader)
	if !ok {
		result.Reason = "listing_unavailable"
		return result, errors.New("complete Gmail listing is unavailable")
	}
	result.RemoteStartedAt = time.Now().UTC()
	before, err := client.GetProfile(ctx)
	if err != nil {
		result.Reason = "profile_failed"
		result.RemoteFinishedAt = time.Now().UTC()
		return result, fmt.Errorf("read Gmail profile before inventory: %w", err)
	}
	if before == nil || !strings.EqualFold(before.EmailAddress, source.Identifier) {
		result.Reason = "account_mismatch"
		result.RemoteFinishedAt = time.Now().UTC()
		return result, errors.New("gmail profile does not match selected source identifier")
	}
	result.HistoryBefore = before.HistoryID
	remote := make(map[string]struct{})
	onlyRemote := make([]string, 0)
	tokens := make(map[string]struct{})
	token := ""
	result.Status = "partial"
	var collectionErr error
	for {
		if err := ctx.Err(); err != nil {
			result.Reason = "interrupted"
			collectionErr = err
			break
		}
		if result.Pages == opts.MaxPages {
			result.Reason = "page_limit"
			collectionErr = errors.New("gmail inventory reached --max-pages")
			break
		}
		if _, seen := tokens[token]; seen {
			result.Reason = "repeated_page_token"
			collectionErr = errors.New("gmail inventory page token repeated")
			break
		}
		tokens[token] = struct{}{}
		page, err := lister.ListCompleteMessageSnapshot(ctx, token)
		if err != nil {
			result.Reason = "page_failed"
			collectionErr = fmt.Errorf("list Gmail inventory page: %w", err)
			break
		}
		if page == nil {
			result.Reason = "invalid_page"
			collectionErr = errors.New("gmail inventory returned a nil page")
			break
		}
		result.Pages++
		for _, message := range page.Messages {
			if err := ctx.Err(); err != nil {
				result.Reason = "interrupted"
				collectionErr = err
				break
			}
			if result.RemoteObservations == opts.MaxMessages {
				result.Reason = "message_limit"
				collectionErr = errors.New("gmail inventory reached --max-messages")
				break
			}
			if message.ID == "" {
				result.Reason = "invalid_message_id"
				collectionErr = errors.New("gmail inventory returned an empty message ID")
				break
			}
			result.RemoteObservations++
			if _, seen := remote[message.ID]; seen {
				result.DuplicateObservations++
				continue
			}
			remote[message.ID] = struct{}{}
			if _, present := local[message.ID]; present {
				local[message.ID] = true
				result.SharedCount++
			} else {
				onlyRemote = append(onlyRemote, message.ID)
			}
		}
		if collectionErr != nil {
			break
		}
		if page.NextPageToken == "" {
			result.RemoteComplete = true
			break
		}
		token = page.NextPageToken
	}
	result.RemoteCount = len(remote)
	result.RemoteOnly = inventoryDifference(onlyRemote, opts.Limit)
	result.LocalUnassessed = result.LocalCount - result.SharedCount
	if collectionErr == nil {
		after, err := client.GetProfile(ctx)
		switch {
		case err != nil:
			result.Status = "inconclusive"
			result.Reason = "profile_failed"
			collectionErr = fmt.Errorf("read Gmail profile after inventory: %w", err)
		case after == nil || !strings.EqualFold(after.EmailAddress, source.Identifier):
			result.Status = "inconclusive"
			result.Reason = "account_mismatch"
			collectionErr = errors.New("gmail profile changed account during inventory")
		default:
			result.HistoryAfter = after.HistoryID
			result.Status = "complete"
			result.Reason = "pagination_finished"
			if before.HistoryID == 0 || after.HistoryID == 0 || before.HistoryID != after.HistoryID {
				result.Status = "inconclusive"
				result.Reason = "mailbox_changed_or_history_unknown"
				collectionErr = errors.New("gmail inventory traversed all pages but mailbox history changed or is unknown")
			}
		}
	}
	if result.Status == "complete" {
		onlyLocal := make([]string, 0, result.LocalUnassessed)
		for id, seen := range local {
			if !seen {
				onlyLocal = append(onlyLocal, id)
			}
		}
		result.LocalNotObserved = inventoryDifference(onlyLocal, opts.Limit)
		result.LocalUnassessed = 0
	}
	result.RemoteFinishedAt = time.Now().UTC()
	return result, collectionErr
}

func readGmailInventoryLocal(ctx context.Context, st *store.Store, opts gmailInventoryOptions, result *gmailInventoryResult) (map[string]bool, *store.Source, error) {
	ctx, release, err := st.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	result.LocalSnapshotAt = time.Now().UTC()
	source, err := st.GetSourceByIDContext(ctx, opts.SourceID)
	if err != nil {
		return nil, nil, err
	}
	if source.SourceType != sourceTypeGmail && source.SourceType != "" {
		result.Reason = "not_gmail"
		return nil, nil, errors.New("selected source is not Gmail")
	}
	if source.MergedIntoSourceID != 0 || source.HistoryOnly {
		result.Reason = "retired_source"
		return nil, nil, errors.New("select a current Gmail source; merged and history-only sources cannot be compared with a live mailbox")
	}
	identities, err := st.ListGmailInventoryIdentitiesContext(ctx, source.ID, opts.MaxMessages+1)
	if err != nil {
		return nil, nil, err
	}
	if len(identities) > opts.MaxMessages {
		result.Reason = "local_message_limit"
		return nil, nil, errors.New("local inventory exceeds --max-messages; no comparison performed")
	}
	local := make(map[string]bool, len(identities))
	for _, identity := range identities {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if identity.ArchiveOnly {
			result.ExcludedArchiveOnly++
			continue
		}
		if identity.ID == "" {
			result.Reason = "invalid_local_id"
			return nil, nil, errors.New("local Gmail identity is empty")
		}
		if identity.Hidden {
			result.IncludedHidden++
		}
		local[identity.ID] = false
	}
	result.LocalCount = len(local)
	result.LocalUnassessed = result.LocalCount
	result.LocalComplete = true
	return local, source, nil
}

func inventoryDifference(ids []string, limit int) *gmailInventoryDifference {
	slices.Sort(ids)
	count := len(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return &gmailInventoryDifference{Count: count, IDs: ids, Omitted: count - len(ids)}
}

func writeGmailInventory(out io.Writer, result gmailInventoryResult, jsonOut bool) error {
	if jsonOut {
		data, err := json.Marshal(result, jsontext.WithIndent("  "))
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintln(out, string(data)); err != nil {
			return fmt.Errorf("write Gmail inventory JSON: %w", err)
		}
		return nil
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Gmail inventory for source %d: %s (%s)\n", result.SourceID, result.Status, result.Reason)
	fmt.Fprintf(&text, "Scope: %s\n", result.Scope)
	fmt.Fprintf(&text, "Local identities: %d (complete: %t); Gmail observed: %d (pagination complete: %t); shared: %d\n", result.LocalCount, result.LocalComplete, result.RemoteCount, result.RemoteComplete, result.SharedCount)
	fmt.Fprintf(&text, "Gmail pages: %d; repeated IDs: %d; excluded merge-only rows: %d; included hidden rows: %d\n", result.Pages, result.DuplicateObservations, result.ExcludedArchiveOnly, result.IncludedHidden)
	for _, item := range []struct {
		label string
		diff  *gmailInventoryDifference
	}{
		{"Gmail present, locally absent", result.RemoteOnly},
		{"Local present, not observed in Gmail (may be retained history)", result.LocalNotObserved},
	} {
		if item.diff == nil {
			fmt.Fprintf(&text, "%s: not assessed\n", item.label)
			continue
		}
		fmt.Fprintf(&text, "%s: %d\n", item.label, item.diff.Count)
		for _, id := range item.diff.IDs {
			fmt.Fprintf(&text, "  %s\n", id)
		}
		if item.diff.Omitted > 0 {
			fmt.Fprintf(&text, "  ... %d IDs omitted; increase --limit to inspect more\n", item.diff.Omitted)
		}
	}
	if result.LocalNotObserved == nil {
		fmt.Fprintf(&text, "Local IDs unassessed for remote absence: %d\n", result.LocalUnassessed)
	}
	for _, caveat := range result.Caveats {
		fmt.Fprintln(&text, caveat)
	}
	if _, err := io.WriteString(out, text.String()); err != nil {
		return fmt.Errorf("write Gmail inventory: %w", err)
	}
	return nil
}

func init() { rootCmd.AddCommand(newDiagnoseGmailInventoryCmd(defaultGmailInventoryDeps())) }
