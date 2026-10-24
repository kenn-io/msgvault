package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonInstructionsCommand() *cobra.Command {
	var jsonOutput bool
	root := &cobra.Command{Use: "instructions", Short: "Manage a person's standing instructions"}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	var all bool
	list := &cobra.Command{Use: "list <person-id>", Short: "List standing instructions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		query := &generated.ListPersonInstructionsQuery{}
		if all {
			status := "proposed,active,suspended,superseded,expired,revoked,rejected"
			include := true
			query.Status = &status
			query.IncludeExpired = &include
		}
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonInstructionsResp, error) {
			return api.ListPersonInstructionsWithResponse(cmd.Context(), &generated.ListPersonInstructionsRequestOptions{PathParams: &generated.ListPersonInstructionsPath{ID: id}, Query: query})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("instruction list response was empty")
		}
		return writeInstructionCLIList(cmd, response.Body, jsonOutput)
	}}
	list.Flags().BoolVar(&all, "all", false, "Include all statuses and expired instructions")
	var limit int64
	review := &cobra.Command{Use: "review", Short: "List instructions needing owner review", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if limit < 1 || limit > 200 {
			return usageErr(cmd, errors.New("limit must be 1–200"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonInstructionReviewResp, error) {
			return api.ListPersonInstructionReviewWithResponse(cmd.Context(), &generated.ListPersonInstructionReviewRequestOptions{Query: &generated.ListPersonInstructionReviewQuery{Limit: &limit}})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("instruction review response was empty")
		}
		return writeInstructionCLIList(cmd, response.Body, jsonOutput)
	}}
	review.Flags().Int64Var(&limit, "limit", 50, "Maximum rows (1–200)")
	var kind, text, value, until string
	var contact int64
	add := &cobra.Command{Use: "add <person-id>", Short: "Record a person's stated instruction", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		if !personfacts.InstructionKind(kind).Valid() {
			return usageErr(cmd, fmt.Errorf("unknown instruction kind %q", kind))
		}
		if strings.TrimSpace(text) == "" {
			return usageErr(cmd, errors.New("text is required"))
		}
		body := generated.AddPersonInstructionBody{Kind: kind, Text: text}
		if cmd.Flags().Changed("value") {
			body.Value = &value
		}
		if cmd.Flags().Changed("contact-point") {
			if contact <= 0 {
				return usageErr(cmd, errors.New("contact-point must be positive"))
			}
			body.ContactPointID = &contact
		}
		if until != "" {
			date, err := time.Parse(time.DateOnly, until)
			if err != nil {
				return usageErr(cmd, fmt.Errorf("until must be YYYY-MM-DD: %w", err))
			}
			now := time.Now().UTC()
			if !date.After(now) || date.Sub(now) > 183*24*time.Hour {
				return usageErr(cmd, errors.New("until must be in the future and no more than 183 days away"))
			}
			body.ValidUntil = &date
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusCreated}, func(api *apiclient.Client) (*generated.AddPersonInstructionResp, error) {
			return api.AddPersonInstructionWithResponse(cmd.Context(), &generated.AddPersonInstructionRequestOptions{PathParams: &generated.AddPersonInstructionPath{ID: id}, Body: &body})
		})
		if err != nil {
			return err
		}
		if response.JSON201 == nil {
			return errors.New("added instruction response was empty")
		}
		if jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		var row store.PersonInstruction
		if err := json.Unmarshal(response.Body, &row); err != nil {
			return err
		}
		return writeInstructionCLIRows(cmd, []store.PersonInstruction{row})
	}}
	add.Flags().StringVar(&kind, "kind", "", "Instruction kind")
	add.Flags().StringVar(&text, "text", "", "One instruction statement (up to 280 characters)")
	add.Flags().StringVar(&value, "value", "", "Name or channel value")
	add.Flags().Int64Var(&contact, "contact-point", 0, "Scope to a current contact point")
	add.Flags().StringVar(&until, "until", "", "Expiry date in UTC (YYYY-MM-DD)")
	root.AddCommand(list, review, add)
	for _, action := range []personfacts.InstructionAction{personfacts.InstructionActionConfirm, personfacts.InstructionActionReject, personfacts.InstructionActionUnsure, personfacts.InstructionActionRevoke, personfacts.InstructionActionAdopt} {
		root.AddCommand(newInstructionDecisionCommand(action, &jsonOutput))
	}
	return root
}
func newInstructionDecisionCommand(action personfacts.InstructionAction, jsonOutput *bool) *cobra.Command {
	var kind, text, value, reason string
	command := &cobra.Command{Use: string(action) + " <id>", Short: "Record an owner " + string(action) + " decision", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "instruction")
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("kind") && !personfacts.InstructionKind(kind).Valid() {
			return usageErr(cmd, fmt.Errorf("unknown instruction kind %q", kind))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetPersonInstructionResp, error) {
			return api.GetPersonInstructionWithResponse(cmd.Context(), &generated.GetPersonInstructionRequestOptions{PathParams: &generated.GetPersonInstructionPath{ID: id}})
		})
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("instruction response was empty")
		}
		body := generated.DecidePersonInstructionBody{Action: string(action), ExpectedRevision: current.JSON200.Revision}
		if reason != "" {
			body.Reason = &reason
		}
		if cmd.Flags().Changed("kind") || cmd.Flags().Changed("text") || cmd.Flags().Changed("value") {
			// Generated request models preserve explicit empty edits through pointers.
			body.Edit = &generated.PersonInstructionEdit{}
			if cmd.Flags().Changed("kind") {
				body.Edit.Kind = &kind
			}
			if cmd.Flags().Changed("text") {
				body.Edit.Text = &text
			}
			if cmd.Flags().Changed("value") {
				body.Edit.Value = &value
			}
		}
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.DecidePersonInstructionResp, error) {
			return api.DecidePersonInstructionWithResponse(cmd.Context(), &generated.DecidePersonInstructionRequestOptions{PathParams: &generated.DecidePersonInstructionPath{ID: id}, Body: &body})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("instruction decision response was empty")
		}
		if *jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		var result store.PersonInstructionDecisionResult
		if err := json.Unmarshal(response.Body, &result); err != nil {
			return err
		}
		if err := writeInstructionCLIRows(cmd, []store.PersonInstruction{result.Instruction}); err != nil {
			return err
		}
		for _, follow := range result.FollowOns {
			switch follow.Kind {
			case "save_nickname":
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Suggested: save '%s' as a nickname\n", textutil.SanitizeTerminal(follow.Value)); err != nil {
					return fmt.Errorf("write nickname suggestion: %w", err)
				}
			case "set_primary_channel":
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Suggested: set '%s' as the primary channel\n", textutil.SanitizeTerminal(follow.Value)); err != nil {
					return fmt.Errorf("write primary channel suggestion: %w", err)
				}
			}
		}
		return nil
	}}
	if action == personfacts.InstructionActionConfirm {
		command.Flags().StringVar(&kind, "kind", "", "Edit instruction kind")
		command.Flags().StringVar(&text, "text", "", "Edit instruction text")
		command.Flags().StringVar(&value, "value", "", "Edit instruction value")
	} else {
		command.Flags().StringVar(&reason, "reason", "", "Reason for the decision")
	}
	return command
}
func writeInstructionCLIList(cmd *cobra.Command, body []byte, jsonOutput bool) error {
	if jsonOutput {
		return writePersonFactCLIRawJSON(cmd, body)
	}
	var response struct {
		Instructions []store.PersonInstruction `json:"instructions"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return err
	}
	return writeInstructionCLIRows(cmd, response.Instructions)
}
func writeInstructionCLIRows(cmd *cobra.Command, rows []store.PersonInstruction) error {
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tPERSON\tSCOPE\tKIND\tSTATUS\tREVIEW\tVALUE\tTEXT"); err != nil {
		return fmt.Errorf("write instruction table header: %w", err)
	}
	for _, row := range rows {
		scope := row.ScopeKey
		if scope == "" {
			scope = "person"
		}
		if _, err := fmt.Fprintf(writer, "%d\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n", row.ID, row.PersonID, textutil.SanitizeTerminal(scope), row.Kind, row.Status, row.ReviewState, textutil.SanitizeTerminal(row.ValueText), textutil.SanitizeTerminal(row.NormalizedText)); err != nil {
			return fmt.Errorf("write instruction table row %d: %w", row.ID, err)
		}
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("flush instruction table: %w", err)
	}
	return nil
}
