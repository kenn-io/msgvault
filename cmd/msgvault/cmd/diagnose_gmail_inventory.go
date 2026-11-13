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
		Long: `Compare Gmail message IDs without syncing or repairing the archive.
Gmail is listed without search or label filters, including Spam and Trash.
The archive includes hidden messages and messages deleted from Gmail.
History retained only from merged accounts is excluded. Matching IDs do not
verify saved original emails, attachments, extracted data or stored files.

A complete result means all Gmail pages and archived IDs were read, and
Gmail's history IDs (mailbox change markers) were nonzero and unchanged.
The archive and Gmail are read at different times; differences may be temporary.
A changed or unavailable history ID makes the result inconclusive. Interrupted
listing never establishes that an archived ID is absent from Gmail.

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
	command.Flags().IntVar(&opts.MaxMessages, "max-messages", 1000000, "maximum archived messages examined and Gmail ID observations per side")
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
		Scope:  "all Gmail messages, including Spam and Trash; archived messages, including hidden messages and those deleted from Gmail; history retained only from merged accounts is excluded",
		Limits: gmailInventoryLimits{Messages: opts.MaxMessages, Pages: opts.MaxPages, Details: opts.Limit},
		Caveats: []string{
			"Gmail pages and the archive are read at different times; syncing, deleting messages or changing accounts during the comparison can make differences temporary.",
			"Matching Gmail history IDs (mailbox change markers) do not guarantee that all listed pages describe the mailbox at one instant.",
			"Messages found only in Gmail may have been excluded by earlier sync filters or limits. This report does not infer which messages were intentionally excluded.",
			"Matching message IDs do not verify saved original emails, attachments, extracted message data, search indexes or stored files.",
		},
	}
	local, source, err := readGmailInventoryLocal(ctx, st, opts, &result)
	if err != nil {
		return result, err
	}
	client, err := newClient(ctx, source)
	if err != nil {
		result.Reason = "provider_unavailable"
		return result, fmt.Errorf("prepare Gmail access: %w", err)
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
		return result, fmt.Errorf("verify Gmail account before listing messages: %w", err)
	}
	if before == nil || !strings.EqualFold(before.EmailAddress, source.Identifier) {
		result.Reason = "account_mismatch"
		result.RemoteFinishedAt = time.Now().UTC()
		return result, errors.New("authenticated Gmail account does not match the selected source")
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
			collectionErr = errors.New("message listing from Gmail reached --max-pages")
			break
		}
		if _, seen := tokens[token]; seen {
			result.Reason = "repeated_page_token"
			collectionErr = errors.New("next-page marker from Gmail repeated; listing stopped to avoid a loop")
			break
		}
		tokens[token] = struct{}{}
		page, err := lister.ListCompleteMessageSnapshot(ctx, token)
		if err != nil {
			result.Reason = "page_failed"
			collectionErr = fmt.Errorf("read a page of Gmail message IDs: %w", err)
			break
		}
		if page == nil {
			result.Reason = "invalid_page"
			collectionErr = errors.New("invalid message-list page returned by Gmail")
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
				collectionErr = errors.New("message listing from Gmail reached --max-messages")
				break
			}
			if message.ID == "" {
				result.Reason = "invalid_message_id"
				collectionErr = errors.New("empty message ID returned by Gmail")
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
			collectionErr = fmt.Errorf("verify Gmail account after listing messages: %w", err)
		case after == nil || !strings.EqualFold(after.EmailAddress, source.Identifier):
			result.Status = "inconclusive"
			result.Reason = "account_mismatch"
			collectionErr = errors.New("authenticated Gmail account changed during comparison")
		default:
			result.HistoryAfter = after.HistoryID
			result.Status = "complete"
			result.Reason = "pagination_finished"
			if before.HistoryID == 0 || after.HistoryID == 0 || before.HistoryID != after.HistoryID {
				result.Status = "inconclusive"
				result.Reason = "mailbox_changed_or_history_unknown"
				collectionErr = errors.New("all Gmail pages were read, but Gmail changed during listing or its change marker is unavailable")
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
		return nil, nil, errors.New("select a current Gmail source; merged accounts and accounts kept only as history cannot be compared with a live mailbox")
	}
	identities, err := st.ListGmailInventoryIdentitiesContext(ctx, source.ID, opts.MaxMessages+1)
	if err != nil {
		return nil, nil, err
	}
	if len(identities) > opts.MaxMessages {
		result.Reason = "local_message_limit"
		return nil, nil, errors.New("archived message count exceeds --max-messages; no comparison performed")
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
			return nil, nil, errors.New("an archived message has an empty Gmail message ID")
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

func gmailInventoryReasonDescription(reason string) string {
	switch reason {
	case "pagination_finished":
		return "all Gmail pages read"
	case "local_read_failed":
		return "could not read the local archive"
	case "provider_unavailable":
		return "Gmail access is unavailable"
	case "listing_unavailable":
		return "Gmail message listing is unavailable"
	case "profile_failed":
		return "could not verify the Gmail account"
	case "account_mismatch":
		return "the authenticated Gmail account does not match the selected source"
	case "interrupted":
		return "collection was canceled or timed out"
	case "page_limit":
		return "reached --max-pages"
	case "message_limit":
		return "reached --max-messages"
	case "local_message_limit":
		return "archived message count exceeds --max-messages"
	case "repeated_page_token":
		return "Gmail repeated its next-page marker; listing stopped to avoid a loop"
	case "page_failed":
		return "could not read a Gmail page"
	case "invalid_page":
		return "Gmail returned an invalid page"
	case "invalid_message_id":
		return "Gmail returned an empty message ID"
	case "invalid_local_id":
		return "an archived message has an empty Gmail message ID"
	case "mailbox_changed_or_history_unknown":
		return "Gmail changed during listing or its mailbox change marker is unavailable"
	case "not_gmail":
		return "the selected source is not a Gmail account"
	case "retired_source":
		return "the selected account was merged or is kept only as history"
	default:
		return strings.ReplaceAll(reason, "_", " ")
	}
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
	fmt.Fprintf(&text, "Gmail inventory for source %d: %s (%s)\n", result.SourceID, result.Status, gmailInventoryReasonDescription(result.Reason))
	fmt.Fprintf(&text, "Scope: %s\n", result.Scope)
	fmt.Fprintf(&text, "Archived message IDs: %d (collection complete: %t); Gmail IDs observed: %d (listing complete: %t); found in both: %d\n", result.LocalCount, result.LocalComplete, result.RemoteCount, result.RemoteComplete, result.SharedCount)
	fmt.Fprintf(&text, "Gmail pages: %d; repeated message IDs: %d; excluded historical messages from merged accounts: %d; included hidden messages: %d\n", result.Pages, result.DuplicateObservations, result.ExcludedArchiveOnly, result.IncludedHidden)
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
		fmt.Fprintf(&text, "Archived message IDs with unknown Gmail presence: %d\n", result.LocalUnassessed)
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
