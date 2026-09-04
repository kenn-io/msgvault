package cmd

import (
	"errors"
	"fmt"
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
	stageDeleteListIDMinAPISchemaVersion = "2.14.0"
	analyticalCacheUnavailableCode       = "analytical_cache_unavailable"
)

func newStageDeleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stage-delete <query>",
		Short: "Stage active messages matching search criteria for deletion",
		Long: `Stage every active message matching a search query for deletion.

The daemon resolves and preflights the search before creating a pending
deletion batch. Use --dry-run to report the match count without creating one.
Review a created batch with show-deletion before running delete-staged.`,
		Args: cobra.ArbitraryArgs,
		RunE: runStageDelete,
	}
	cmd.Flags().BoolVar(&stageDeleteDryRun, "dry-run", false, "Show the match count without creating a deletion batch")
	cmd.Flags().Int64("source-id", 0, "Restrict staging to one exact source ID")
	return cmd
}

func runStageDelete(cmd *cobra.Command, args []string) error {
	queryText := strings.TrimSpace(strings.Join(args, " "))
	parsed := search.Parse(queryText)
	if err := parsed.Err(); err != nil {
		return usageErr(cmd, err)
	}
	if parsed.IsEmpty() {
		return usageErr(cmd, errors.New("empty search query"))
	}
	if hasEmptyAddressFilter(parsed) {
		return usageErr(cmd, errors.New("empty address filter"))
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
	if len(parsed.ListIDs) > 0 {
		supported, err := store.SupportsAPISchemaVersion(cmd.Context(), stageDeleteListIDMinAPISchemaVersion)
		if err != nil {
			return fmt.Errorf("check daemon List-ID filter capability: %w", err)
		}
		if !supported {
			return fmt.Errorf("List-ID filter requires daemon API schema %s or newer", stageDeleteListIDMinAPISchemaVersion)
		}
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

	w := cmd.OutOrStdout()
	if result.DryRun {
		if _, err = fmt.Fprintf(w, "Dry run: %d message(s) match the search; no deletion batch was created.\n", result.MessageCount); err != nil {
			return fmt.Errorf("write dry-run summary: %w", err)
		}
		return nil
	}
	if result.ID == nil || strings.TrimSpace(*result.ID) == "" {
		return errors.New("stage deletion response did not include a batch ID")
	}
	if _, err = fmt.Fprintf(w, "Staged %d message(s) for deletion in batch %s.\n", result.MessageCount, *result.ID); err != nil {
		return fmt.Errorf("write staging summary: %w", err)
	}
	if _, err = fmt.Fprintf(w, "Review with 'msgvault show-deletion %s', then execute with 'msgvault delete-staged %s'.\n", *result.ID, *result.ID); err != nil {
		return fmt.Errorf("write staging summary: %w", err)
	}
	return nil
}

// stageDeleteDaemonErr turns the daemon's structured cache-unavailable
// response into an actionable message instead of a bare API error.
func stageDeleteDaemonErr(op string, err error) error {
	var apiErr *daemonclient.APIError
	if errors.As(err, &apiErr) && apiErr.APIErrorCode() == analyticalCacheUnavailableCode {
		return fmt.Errorf("%s: %s; retry shortly if the analytical cache is still building, "+
			"or run 'msgvault build-cache' and rerun stage-delete", op, apiErr.Message)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func hasEmptyAddressFilter(q *search.Query) bool {
	for _, values := range [][]string{q.FromAddrs, q.ToAddrs, q.CcAddrs, q.BccAddrs} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" {
				return true
			}
		}
	}
	return false
}

func init() {
	rootCmd.AddCommand(newStageDeleteCommand())
}
