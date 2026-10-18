package cmd

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const personTierMinAPISchemaVersion = "3.24.0"

func newPersonTiersCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "tiers", Short: "Inspect and manage tiers inferred from archived interactions"}
	command.PersistentFlags().BoolVar(&jsonOutput, flagJSON, false, "Forward daemon responses as JSON (apply emits one JSON object per page)")
	command.AddCommand(
		newPersonTierPreviewCommand(&jsonOutput), newPersonTierExplainCommand(&jsonOutput),
		newPersonTierApplyCommand(&jsonOutput), newPersonTierResetCommand(&jsonOutput),
		newPersonTierHistoryCommand(&jsonOutput), newPersonTierCandidatesCommand(&jsonOutput),
		newPersonTierOptOutCommand(&jsonOutput, true), newPersonTierOptOutCommand(&jsonOutput, false),
	)
	return command
}

func openPersonTierClient(command *cobra.Command) (*daemonclient.Client, error) {
	client, _, err := OpenHTTPStore(command.Context())
	if err != nil {
		return nil, err
	}
	supported, err := client.SupportsAPISchemaVersion(command.Context(), personTierMinAPISchemaVersion)
	if err == nil && !supported {
		err = fmt.Errorf("person tiers requires daemon API schema %s or newer; upgrade the daemon", personTierMinAPISchemaVersion)
	}
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("check people tiers capability: %w", err)
	}
	return client, nil
}

func personTierLimit(command *cobra.Command, limit, maximum int) error {
	if limit < 1 || limit > maximum {
		return usageErr(command, fmt.Errorf("--limit must be between 1 and %d", maximum))
	}
	return nil
}

func previewPersonTierPage(command *cobra.Command, client *daemonclient.Client, query *generated.PreviewPersonTiersQuery) (*generated.PreviewPersonTiersResp, error) {
	response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PreviewPersonTiersResp, error) {
		return api.PreviewPersonTiersWithResponse(command.Context(), &generated.PreviewPersonTiersRequestOptions{Query: query})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("person tiers preview response was empty")
	}
	return response, nil
}

func newPersonTierPreviewCommand(jsonOutput *bool) *cobra.Command {
	var limit, tier int
	var cursor string
	var changesOnly bool
	command := &cobra.Command{Use: "preview", Short: "Preview one page without writing tier state", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := personTierLimit(command, limit, 500); err != nil {
				return err
			}
			if command.Flags().Changed("tier") && (tier < 1 || tier > 3) {
				return usageErr(command, errors.New("--tier must be 1, 2 or 3"))
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			bounded := int64(limit)
			query := &generated.PreviewPersonTiersQuery{Limit: &bounded}
			if command.Flags().Changed("tier") {
				value := int64(tier)
				query.Tier = &value
			}
			if command.Flags().Changed("changes-only") {
				query.ChangesOnly = &changesOnly
			}
			if cursor != "" {
				query.Cursor = &cursor
			}
			response, err := previewPersonTierPage(command, client, query)
			if err != nil {
				return err
			}
			if *jsonOutput {
				return writePersonBriefRawJSON(command, response.Body)
			}
			return writePersonTierPage(command.OutOrStdout(), *response.JSON200)
		}}
	command.Flags().IntVar(&limit, "limit", 200, "Maximum people in this page (1-500)")
	command.Flags().IntVar(&tier, "tier", 0, "Only show tier 1, 2 or 3")
	command.Flags().BoolVar(&changesOnly, "changes-only", false, "Only show changed decisions")
	command.Flags().StringVar(&cursor, "cursor", "", "Continue from a preview next_cursor")
	return command
}

func newPersonTierExplainCommand(jsonOutput *bool) *cobra.Command {
	return &cobra.Command{Use: "explain <person-id>", Short: "Explain signals, thresholds, grants and preferences for one person", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			id, err := positivePersonCLIArg(command, args[0], personValue)
			if err != nil {
				return err
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetPersonTierResp, error) {
				return api.GetPersonTierWithResponse(command.Context(), &generated.GetPersonTierRequestOptions{PathParams: &generated.GetPersonTierPath{ID: id}})
			})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person tier explanation response was empty")
			}
			if *jsonOutput {
				return writePersonBriefRawJSON(command, response.Body)
			}
			return writePersonTierExplanation(command.OutOrStdout(), *response.JSON200)
		}}
}

func newPersonTierApplyCommand(jsonOutput *bool) *cobra.Command {
	var dryRun, yes bool
	var limit, maximum int
	command := &cobra.Command{Use: "apply", Short: "Apply tier changes in bounded pages", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := personTierLimit(command, limit, 500); err != nil {
				return err
			}
			if maximum < 0 {
				return usageErr(command, errors.New("--max must be nonnegative (0 means unlimited)"))
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			pageLimit := limit
			if maximum > 0 {
				pageLimit = min(pageLimit, maximum)
			}
			if dryRun || !yes {
				bounded := int64(pageLimit)
				preview, err := previewPersonTierPage(command, client, &generated.PreviewPersonTiersQuery{Limit: &bounded})
				if err != nil {
					return err
				}
				if dryRun {
					if *jsonOutput {
						return writePersonBriefRawJSON(command, preview.Body)
					}
					return writePersonTierPage(command.OutOrStdout(), *preview.JSON200)
				}
				if err := writePersonTierPage(command.ErrOrStderr(), *preview.JSON200); err != nil {
					return err
				}
				scope := "all people"
				if maximum > 0 {
					scope = "at most " + strconv.Itoa(maximum) + " people"
				}
				if err := personTierPrintf(command.ErrOrStderr(), "Apply scope: %s, in pages of up to %d people.\n", scope, limit); err != nil {
					return err
				}
				if !commandStdinIsTerminal(command) {
					return errors.New("apply requires --yes when standard input is not a terminal")
				}
				if !confirmEmbed(command, "Apply automatic people tier changes? ") {
					return errors.New("person tiers apply cancelled")
				}
			}
			var runID, cursor string
			considered := 0
			totals := map[string]int64{}
			seenCursors := map[string]bool{}
			for {
				pageLimit = limit
				if maximum > 0 {
					pageLimit = min(pageLimit, maximum-considered)
				}
				bounded := int64(pageLimit)
				body := generated.ApplyPersonTiersBody{Limit: &bounded}
				if cursor != "" {
					body.Cursor = &cursor
				}
				if runID != "" {
					body.RunID = &runID
				}
				response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ApplyPersonTiersResp, error) {
					return api.ApplyPersonTiersWithResponse(command.Context(), &generated.ApplyPersonTiersRequestOptions{Body: &body})
				})
				if err != nil {
					return err
				}
				if response.JSON200 == nil {
					return errors.New("person tiers apply response was empty")
				}
				page := response.JSON200
				if page.RunID == "" {
					return errors.New("person tiers apply response omitted run_id")
				}
				if runID != "" && page.RunID != runID {
					return errors.New("person tiers apply response changed run_id during continuation")
				}
				if page.NextCursor != "" && seenCursors[page.NextCursor] {
					return errors.New("person tiers apply response repeated a continuation cursor")
				}
				if *jsonOutput {
					if err := writePersonBriefRawJSON(command, response.Body); err != nil {
						return err
					}
				}
				considered += len(page.People)
				for key, value := range page.Counts {
					totals[key] += value
				}
				if page.NextCursor == "" || maximum > 0 && considered >= maximum {
					if *jsonOutput {
						return nil
					}
					totals["evaluated"] = int64(considered)
					return writePersonTierCounts(command.OutOrStdout(), totals)
				}
				runID, cursor = page.RunID, page.NextCursor
				seenCursors[cursor] = true
			}
		}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Preview one page without applying changes")
	command.Flags().BoolVar(&yes, "yes", false, "Apply without an interactive confirmation")
	command.Flags().IntVar(&limit, "limit", 500, "Maximum people per page (1-500)")
	command.Flags().IntVar(&maximum, "max", 0, "Maximum people to consider (0 means unlimited)")
	return command
}

func newPersonTierResetCommand(jsonOutput *bool) *cobra.Command {
	var yes bool
	command := &cobra.Command{Use: "reset", Short: "Remove automatic tier grants and state, retaining manual choices", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !yes {
				return usageErr(command, errors.New("reset requires --yes"))
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.ResetPersonTiersBody{Confirm: true}
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ResetPersonTiersResp, error) {
				return api.ResetPersonTiersWithResponse(command.Context(), &generated.ResetPersonTiersRequestOptions{Body: &body})
			})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person tiers reset response was empty")
			}
			if *jsonOutput {
				return writePersonBriefRawJSON(command, response.Body)
			}
			if _, err := fmt.Fprintf(command.OutOrStdout(), "Cleared automatic state for %d people.\n", response.JSON200.Cleared); err != nil {
				return fmt.Errorf("write people tiers reset: %w", err)
			}
			for _, deferred := range response.JSON200.Deferred {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "Person %d: deferred for manual run %s\n", deferred.PersonID, textutil.SanitizeTerminal(deferred.RunID)); err != nil {
					return fmt.Errorf("write deferred people tiers reset: %w", err)
				}
			}
			return nil
		}}
	command.Flags().BoolVar(&yes, "yes", false, "Confirm removal of automatic tier grants and state")
	return command
}

func newPersonTierHistoryCommand(jsonOutput *bool) *cobra.Command {
	var limit, offset int
	var personID int64
	command := &cobra.Command{Use: "history", Short: "Read tier decision history", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := personTierLimit(command, limit, 200); err != nil {
				return err
			}
			if offset < 0 {
				return usageErr(command, errors.New("--offset must be nonnegative"))
			}
			if command.Flags().Changed("person") && personID < 1 {
				return usageErr(command, errors.New("--person must be a positive integer"))
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			bounded, start := int64(limit), int64(offset)
			query := &generated.ListPersonTierEventsQuery{Limit: &bounded, Offset: &start}
			if command.Flags().Changed("person") {
				query.PersonID = &personID
			}
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonTierEventsResp, error) {
				return api.ListPersonTierEventsWithResponse(command.Context(), &generated.ListPersonTierEventsRequestOptions{Query: query})
			})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person tier history response was empty")
			}
			if *jsonOutput {
				return writePersonBriefRawJSON(command, response.Body)
			}
			return writePersonTierEvents(command.OutOrStdout(), response.JSON200.Events)
		}}
	command.Flags().Int64Var(&personID, "person", 0, "Only this person's events")
	command.Flags().IntVar(&limit, "limit", 20, "Maximum events (1-200)")
	command.Flags().IntVar(&offset, "offset", 0, "Skip this many events")
	return command
}

func newPersonTierCandidatesCommand(jsonOutput *bool) *cobra.Command {
	var limit int
	var cursor string
	command := &cobra.Command{Use: "candidates", Short: "List unlinked participant approximations for manual review", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := personTierLimit(command, limit, 500); err != nil {
				return err
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			bounded := int64(limit)
			query := &generated.ListPersonTierCandidatesQuery{Limit: &bounded}
			if cursor != "" {
				query.Cursor = &cursor
			}
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonTierCandidatesResp, error) {
				return api.ListPersonTierCandidatesWithResponse(command.Context(), &generated.ListPersonTierCandidatesRequestOptions{Query: query})
			})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person tier candidates response was empty")
			}
			if *jsonOutput {
				return writePersonBriefRawJSON(command, response.Body)
			}
			return writePersonTierCandidates(command.OutOrStdout(), *response.JSON200)
		}}
	command.Flags().IntVar(&limit, "limit", 50, "Maximum participant groups (1-500)")
	command.Flags().StringVar(&cursor, "cursor", "", "Continue from the returned next_cursor")
	return command
}

func newPersonTierOptOutCommand(jsonOutput *bool, optedOut bool) *cobra.Command {
	var lane, reason string
	action, description := "opt-in", "Clear an explicit automation opt-out"
	if optedOut {
		action, description = "opt-out", "Opt out of one automatic tier lane"
	}
	command := &cobra.Command{Use: action + " <person-id>", Short: description, Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			id, err := positivePersonCLIArg(command, args[0], personValue)
			if err != nil {
				return err
			}
			switch lane {
			case "tracking", "briefs", "enrichment":
			default:
				return usageErr(command, errors.New("--lane must be tracking, briefs or enrichment"))
			}
			client, err := openPersonTierClient(command)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.SetPersonTierOptOutBody{Lane: generated.PutPersonTierOptOutRequestLane(lane), OptedOut: optedOut}
			if reason != "" {
				body.Reason = &reason
			}
			response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.SetPersonTierOptOutResp, error) {
				return api.SetPersonTierOptOutWithResponse(command.Context(), &generated.SetPersonTierOptOutRequestOptions{PathParams: &generated.SetPersonTierOptOutPath{ID: id}, Body: &body})
			})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person tier opt-out response was empty")
			}
			if *jsonOutput {
				return writePersonBriefRawJSON(command, response.Body)
			}
			state := "opt-out cleared"
			if optedOut {
				state = "opt-out recorded"
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Person %d %s: %s\n", id, lane, state)
			if err != nil {
				return fmt.Errorf("write person tier opt-out: %w", err)
			}
			return nil
		}}
	command.Flags().StringVar(&lane, "lane", "", "Automation lane: tracking, briefs or enrichment")
	_ = command.MarkFlagRequired("lane")
	if optedOut {
		command.Flags().StringVar(&reason, "reason", "", "Reason for this opt-out")
	}
	return command
}

func writePersonTierCounts(w io.Writer, counts map[string]int64) error {
	_, err := fmt.Fprintf(w, "Tier totals: 1=%d 2=%d 3=%d\nLane actions: created=%d removed=%d blocked_manual=%d blocked_opt_out=%d\nPeople: evaluated=%d deferred=%d failed=%d superseded=%d\n",
		counts["tier_1"], counts["tier_2"], counts["tier_3"], counts["created"], counts["removed"], counts["blocked_manual"], counts["blocked_opt_out"], counts["evaluated"], counts["deferred"], counts["failed"], counts["superseded"])
	if err != nil {
		return fmt.Errorf("write people tier counts: %w", err)
	}
	return nil
}

func personTierActor(actor string) string {
	actor = textutil.SanitizeTerminal(actor)
	if strings.TrimSpace(actor) == "" {
		return "unknown (legacy row)"
	}
	return actor
}
