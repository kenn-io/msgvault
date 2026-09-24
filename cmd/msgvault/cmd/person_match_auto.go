package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonMatchAutoCommand() *cobra.Command {
	command := &cobra.Command{Use: "match-auto", Short: "Inspect and dry-run optional identity scoring"}
	command.AddCommand(newPersonMatchAutoStatusCommand(),
		newPersonMatchAutoConsentCommand("consent"), newPersonMatchAutoConsentCommand("revoke"),
		newPersonMatchAutoRunCommand(), newPersonMatchAutoHistoryCommand())
	return command
}

func newPersonMatchAutoStatusCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "status", Short: "Show scoring readiness and exact disclosure", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			status, err := client.GetIdentityScoringStatus(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonMatchAutoJSON(cmd, status)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Enabled: %t\nDry run ready: %t\nConsent active: %t\nCredential available: %t\nLive acceptance available: %t\n",
				status.Enabled, status.DryRunReady, status.ConsentActive, status.CredentialAvailable, status.LiveAutomaticAcceptanceAvailable)
			if status.DisclosureFingerprint != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Disclosure fingerprint: %s\n", *status.DisclosureFingerprint)
			}
			if status.Disclosure != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Provider: %s\nModel: %s\nPacket: %s\nRetention: %s\nPolicy: %s\n",
					status.Disclosure.Endpoint, status.Disclosure.ModelID, status.Disclosure.PacketSchema,
					status.Disclosure.RetentionDeclaration, status.Disclosure.PolicyVersion)
			}
			if status.Blocker != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Dry-run blocker: %s\n", *status.Blocker)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Live blocker: %s\n", status.LiveBlocker)
			return nil
		}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print complete status JSON")
	return command
}

func newPersonMatchAutoConsentCommand(action string) *cobra.Command {
	var jsonOutput bool
	short := "Consent to the exact current disclosure"
	if action == "revoke" {
		short = "Revoke consent for the exact current disclosure"
	}
	command := &cobra.Command{Use: action + " <disclosure-fingerprint>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fingerprint := strings.TrimSpace(args[0])
			if fingerprint == "" {
				return usageErr(cmd, errors.New("disclosure fingerprint is required; run status first"))
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			var result *generated.PersonMatchConsentDecisionResponse
			if action == "consent" {
				result, err = client.GrantIdentityScoringConsent(cmd.Context(), fingerprint)
			} else {
				result, err = client.RevokeIdentityScoringConsent(cmd.Context(), fingerprint)
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonMatchAutoJSON(cmd, result)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Consent active: %t\nChanged: %t\nDisclosure fingerprint: %s\n",
				result.ConsentActive, result.Changed, result.DisclosureFingerprint)
			return nil
		}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print complete decision JSON")
	return command
}

func newPersonMatchAutoRunCommand() *cobra.Command {
	var dryRun, jsonOutput bool
	var limit int64
	command := &cobra.Command{Use: "run", Short: "Score one bounded batch; live acceptance requires a later evaluation gate", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 0 {
				return usageErr(cmd, errors.New("--limit must be positive"))
			}
			var limitValue *int64
			if limit > 0 {
				limitValue = &limit
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := client.RunIdentityScoring(cmd.Context(), limitValue, dryRun)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonMatchAutoJSON(cmd, response)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Processed: %d\n", response.Processed)
			for _, row := range response.Results {
				score := "n/a"
				if row.Probability != nil {
					score = fmt.Sprintf("%.3f", *row.Probability)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Candidate %d: score=%s action=%s status=%s blockers=%s\n",
					row.CandidateID, score, row.ProposedAction, row.Status, strings.Join(row.Blockers, ","))
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Review token: %s\n", row.ReviewToken)
			}
			return nil
		}}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "Score and journal without changing identities")
	command.Flags().Int64Var(&limit, "limit", 0, "Maximum candidates (defaults to configured batch size)")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print complete dry-run JSON")
	return command
}

func newPersonMatchAutoHistoryCommand() *cobra.Command {
	var candidateID, limit, beforeID int64
	var jsonOutput bool
	command := &cobra.Command{Use: "history", Short: "List redacted scoring judgments", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if candidateID < 0 || beforeID < 0 || limit < 1 || limit > 100 {
				return usageErr(cmd, errors.New("--candidate-id and --before-id must be nonnegative; --limit must be 1–100"))
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := client.ListIdentityJudgments(cmd.Context(), candidateID, limit, beforeID)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonMatchAutoJSON(cmd, response)
			}
			for _, row := range response.Judgments {
				score := "n/a"
				if row.Probability != nil {
					score = fmt.Sprintf("%.3f", *row.Probability)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d candidate=%d score=%s outcome=%s status=%s\n",
					row.ID, row.CandidateID, score, row.Outcome, row.Status)
			}
			if response.NextBeforeID != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Next before ID: %d\n", *response.NextBeforeID)
			}
			return nil
		}}
	command.Flags().Int64Var(&candidateID, "candidate-id", 0, "Filter by candidate ID (default all)")
	command.Flags().Int64Var(&limit, "limit", 100, "Maximum judgments (1–100)")
	command.Flags().Int64Var(&beforeID, "before-id", 0, "Return older judgments below this ID")
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print complete journal JSON")
	return command
}

func writePersonMatchAutoJSON(cmd *cobra.Command, value any) error {
	return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), value, json.Deterministic(true))
}
