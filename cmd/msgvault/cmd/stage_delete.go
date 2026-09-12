package cmd

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/search"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var stageDeleteDryRun bool

const (
	stageDeleteMinAPISchemaVersion = "2.18.0"
	analyticalCacheUnavailableCode = "analytical_cache_unavailable"
)

func newStageDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stage-delete [query]",
		Short: "Stage active messages matching search criteria or explicit IDs for deletion",
		Long: `Stage the deletable messages matching a search query for deletion.

The search runs with the same semantics as msgvault search. Matches that no
source supports deleting, such as chats, meetings, and non-Gmail mail, are
reported and skipped rather than rejecting the whole search. Use --dry-run to
see the same staged subset and counts without creating a batch.
Alternatively, pass a comma-separated --ids list to stage explicit internal
message IDs; this form bypasses search and analytical-cache readiness.
Review a created batch with show-deletion before running delete-staged.`,
		Args: cobra.ArbitraryArgs,
		RunE: runStageDelete,
	}
	cmd.Flags().BoolVar(&stageDeleteDryRun, "dry-run", false, "Show the staged subset and skipped counts without creating a deletion batch")
	cmd.Flags().Int64("source-id", 0, "Restrict staging to one exact source ID")
	cmd.Flags().String("ids", "", "Stage these comma-separated internal message IDs instead of a query")
	cmd.MarkFlagsMutuallyExclusive("ids", "source-id")
	return cmd
}

func runStageDelete(cmd *cobra.Command, args []string) error {
	queryText := strings.TrimSpace(strings.Join(args, " "))
	idsChanged := cmd.Flags().Changed("ids")
	if idsChanged && queryText != "" {
		return usageErr(cmd, errors.New("--ids cannot be combined with a search query"))
	}
	if idsChanged {
		return runStageDeleteFromIDs(cmd)
	}
	return runStageDeleteFromQuery(cmd, queryText)
}

func runStageDeleteFromQuery(cmd *cobra.Command, queryText string) error {
	parsed := search.Parse(queryText)
	if err := parsed.Err(); err != nil {
		return usageErr(cmd, err)
	}
	if parsed.IsEmpty() {
		return usageErr(cmd, errors.New("empty search query"))
	}
	sourceID, err := cmd.Flags().GetInt64("source-id")
	if err != nil {
		return usageErr(cmd, fmt.Errorf("invalid source ID: %w", err))
	}
	if cmd.Flags().Changed("source-id") && sourceID <= 0 {
		return usageErr(cmd, errors.New("source ID must be positive"))
	}

	// Default cache intent: a daemon this command has to spawn builds its
	// analytical cache before Explore runs, instead of answering 503.
	store, _, err := openHTTPStoreWithStartupCacheIntent(cmd.Context(), startupCacheBuildIntentDefault)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()
	supported, err := store.SupportsAPISchemaVersion(cmd.Context(), stageDeleteMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("check daemon query staging capability: %w", err)
	}
	if !supported {
		return fmt.Errorf("query staging requires daemon API schema %s or newer; upgrade the daemon", stageDeleteMinAPISchemaVersion)
	}

	// Staging trusts the full-text index as the complete match set, so an
	// index still being verified or backfilled must block staging instead of
	// silently omitting messages. The probe also starts the daemon's
	// background completeness check, so a retry succeeds once it finishes.
	indexProbe, err := store.GetCLISearch(cmd.Context(), daemonclient.CLISearchRequest{
		Query: queryText,
		Limit: 1,
	})
	if err != nil {
		return fmt.Errorf("check search index readiness: %w", err)
	}
	switch indexProbe.IndexState {
	case "building":
		return errors.New("the full-text search index is being rebuilt in the background, " +
			"so staging from search criteria could miss matching messages; " +
			"retry when the rebuild finishes")
	case "checking":
		return errors.New("full-text search index completeness is still being verified, " +
			"so staging from search criteria could miss matching messages; retry shortly")
	}

	searchMode := generated.ExploreHTTPRequestSearchModeFullText
	limit := int64(1)
	filters := []generated.ExploreFilter{{
		Dimension: generated.ExploreFilterDimensionDeletion,
		Values:    []string{"active"},
	}}
	if cmd.Flags().Changed("source-id") {
		filters = append(filters, generated.ExploreFilter{
			Dimension: generated.ExploreFilterDimensionSource,
			Values:    []string{strconv.FormatInt(sourceID, 10)},
		})
	}
	predicate := generated.ExploreHTTPRequest{
		Query:      &queryText,
		Filters:    filters,
		SearchMode: &searchMode,
	}
	exploreResp, err := daemonclient.APIResponse(store, func(client *apiclient.Client) (*generated.ExploreResp, error) {
		return client.ExploreWithResponse(cmd.Context(), &generated.ExploreRequestOptions{
			Body: &generated.ExploreBody{
				Filters:    predicate.Filters,
				Query:      predicate.Query,
				SearchMode: predicate.SearchMode,
				Limit:      &limit,
			},
		})
	})
	if err != nil {
		return stageDeleteDaemonErr("explore search", err)
	}
	if exploreResp == nil || exploreResp.JSON200 == nil {
		return errors.New("explore search returned no response")
	}

	selection := generated.ExploreSelection{
		CacheRevision:       exploreResp.JSON200.CacheRevision,
		CandidateSnapshotID: exploreResp.JSON200.CandidateSnapshotID,
		Mode:                generated.ExploreSelectionModeAllMatching,
		Predicate:           predicate,
		SearchProvenance:    exploreResp.JSON200.SearchProvenance,
	}
	preflightResp, err := daemonclient.APIResponse(store, func(client *apiclient.Client) (*generated.PreflightExploreSelectionResp, error) {
		return client.PreflightExploreSelectionWithResponse(cmd.Context(), &generated.PreflightExploreSelectionRequestOptions{
			Body: &generated.PreflightExploreSelectionBody{Selection: selection},
		})
	})
	if err != nil {
		return stageDeleteDaemonErr("preflight search selection", err)
	}
	if preflightResp == nil || preflightResp.JSON200 == nil {
		return errors.New("preflight search selection returned no response")
	}

	reviewed := preflightResp.JSON200
	if reviewed.DeletableCount < reviewed.Count {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(),
			"Preflight: %d matching item(s); %d message(s) can be staged; %d item(s) will be skipped.\n",
			reviewed.Count, reviewed.DeletableCount, reviewed.Count-reviewed.DeletableCount); err != nil {
			return fmt.Errorf("write preflight summary: %w", err)
		}
	}

	description := "staged from CLI search"
	operationToken := preflightResp.JSON200.OperationToken
	stageResp, err := daemonclient.APIResponseWithStatuses(store, []int{200, 201}, func(client *apiclient.Client) (*generated.StageDeletionResp, error) {
		return client.StageDeletionWithResponse(cmd.Context(), &generated.StageDeletionRequestOptions{
			Body: &generated.StageDeletionBody{
				Description:    &description,
				DryRun:         &stageDeleteDryRun,
				OperationToken: &operationToken,
				Selection:      &selection,
			},
		})
	})
	if err != nil {
		return stageDeleteDaemonErr("stage deletion", err)
	}
	if stageResp == nil {
		return errors.New("stage deletion returned no response")
	}

	var result *generated.StageDeletionResponse
	switch {
	case stageResp.JSON200 != nil:
		result = stageResp.JSON200
	case stageResp.JSON201 != nil:
		result = stageResp.JSON201
	default:
		return errors.New("stage deletion returned no response body")
	}
	if result == nil {
		return errors.New("stage deletion returned no response body")
	}

	return writeStageDeleteOutcome(cmd.OutOrStdout(), result)
}

func runStageDeleteFromIDs(cmd *cobra.Command) error {
	rawIDs, err := cmd.Flags().GetString("ids")
	if err != nil {
		return usageErr(cmd, fmt.Errorf("invalid --ids: %w", err))
	}
	messageIDs, err := parseStageDeleteMessageIDs(rawIDs)
	if err != nil {
		return usageErr(cmd, err)
	}

	store, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = store.Close() }()

	description := "staged from CLI message IDs"
	stageResp, err := daemonclient.APIResponseWithStatuses(store, []int{200, 201}, func(client *apiclient.Client) (*generated.StageDeletionResp, error) {
		return client.StageDeletionWithResponse(cmd.Context(), &generated.StageDeletionRequestOptions{
			Body: &generated.StageDeletionBody{
				Description: &description,
				DryRun:      &stageDeleteDryRun,
				MessageIds:  messageIDs,
			},
		})
	})
	if err != nil {
		return stageDeleteMessageIDsDaemonErr(err)
	}
	if stageResp == nil {
		return errors.New("stage deletion returned no response")
	}

	var result *generated.StageDeletionResponse
	switch {
	case stageResp.JSON200 != nil:
		result = stageResp.JSON200
	case stageResp.JSON201 != nil:
		result = stageResp.JSON201
	default:
		return errors.New("stage deletion returned no response body")
	}
	if result == nil {
		return errors.New("stage deletion returned no response body")
	}

	return writeStageDeleteOutcome(cmd.OutOrStdout(), result)
}

func parseStageDeleteMessageIDs(raw string) ([]int64, error) {
	parts := strings.Split(raw, ",")
	messageIDs := make([]int64, 0, len(parts))
	seen := make(map[int64]struct{}, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			return nil, errors.New("--ids must not contain empty message IDs")
		}
		messageID, err := strconv.ParseInt(value, 10, 64)
		if err != nil || messageID <= 0 {
			return nil, fmt.Errorf("message ID %q must be a positive integer", value)
		}
		if _, ok := seen[messageID]; ok {
			return nil, fmt.Errorf("duplicate message ID %d", messageID)
		}
		seen[messageID] = struct{}{}
		messageIDs = append(messageIDs, messageID)
	}
	return messageIDs, nil
}

func stageDeleteMessageIDsDaemonErr(err error) error {
	var apiErr *daemonclient.APIError
	if errors.As(err, &apiErr) && apiErr.APIErrorCode() == "multi_account_selection" {
		return fmt.Errorf("stage deletion: %s; stage IDs from one source per invocation", apiErr.Message)
	}
	return stageDeleteDaemonErr("stage deletion", err)
}

func writeStageDeleteOutcome(w io.Writer, result *generated.StageDeletionResponse) error {
	if result.DryRun {
		if _, err := fmt.Fprintf(w, "Dry run: %d message(s) would be staged; no deletion batch was created.\n", result.MessageCount); err != nil {
			return fmt.Errorf("write dry-run summary: %w", err)
		}
		if err := writeStageDeleteSkipped(w, result); err != nil {
			return err
		}
		return nil
	}
	if result.ID == nil || strings.TrimSpace(*result.ID) == "" {
		return errors.New("stage deletion response did not include a batch ID")
	}
	if _, err := fmt.Fprintf(w, "Staged %d message(s) for deletion in batch %s.\n", result.MessageCount, *result.ID); err != nil {
		return fmt.Errorf("write staging summary: %w", err)
	}
	if err := writeStageDeleteSkipped(w, result); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Review with 'msgvault show-deletion %s', then execute with 'msgvault delete-staged %s'.\n", *result.ID, *result.ID); err != nil {
		return fmt.Errorf("write staging summary: %w", err)
	}
	return nil
}

// writeStageDeleteSkipped names the matches the daemon left out, so a
// narrower staged count than the search reported is never silent.
func writeStageDeleteSkipped(w io.Writer, result *generated.StageDeletionResponse) error {
	skipped := int64(0)
	if result.SkippedCount != nil {
		skipped = *result.SkippedCount
	}
	if skipped <= 0 {
		return nil
	}
	matched := skipped + result.MessageCount
	if result.MatchedCount != nil {
		matched = *result.MatchedCount
	}
	if _, err := fmt.Fprintf(w,
		"%d of %d matching item(s) cannot be deleted from their source (chats, meetings, or non-Gmail mail) and were skipped.\n",
		skipped, matched); err != nil {
		return fmt.Errorf("write skipped summary: %w", err)
	}
	return nil
}

// stageDeleteDaemonErr turns the daemon's structured rejections into
// actionable messages instead of bare API errors.
func stageDeleteDaemonErr(op string, err error) error {
	var apiErr *daemonclient.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("%s: %w", op, err)
	}
	switch apiErr.APIErrorCode() {
	case analyticalCacheUnavailableCode:
		return fmt.Errorf("%s: %s; retry shortly if the analytical cache is still building, "+
			"or run 'msgvault build-cache' and rerun stage-delete", op, apiErr.Message)
	case "selection_not_deletable":
		return fmt.Errorf("%s: %s; nothing the search matched can be deleted from its "+
			"source. Deletion currently covers Gmail mail only, so widen or retarget the "+
			"search, for example with --source-id <gmail-source-id>",
			op, apiErr.Message)
	case "multi_account_selection":
		return fmt.Errorf("%s: %s; rerun stage-delete once per source with --source-id",
			op, apiErr.Message)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func init() {
	rootCmd.AddCommand(newStageDeleteCommand())
}
