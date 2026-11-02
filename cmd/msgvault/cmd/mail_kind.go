package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newMailCommand() *cobra.Command {
	mail := &cobra.Command{Use: "mail", Short: "Inspect optional transactional-mail classification"}
	kind := &cobra.Command{Use: "kind", Short: "Manage disclosure and consent"}
	kind.AddCommand(newMailKindStatusCommand(), newMailKindConsentCommand(true), newMailKindConsentCommand(false))
	mail.AddCommand(kind)
	return mail
}

func newMailKindStatusCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: statusSubcommand, Short: "Show readiness and disclosure", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			status, err := client.GetMailKindStatus(cmd.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonScoringJSON(cmd, status)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Enabled: %t\nReady: %t\nConsent active: %t\nCredential available: %t\nData sent: %s\n",
				status.Enabled, status.Ready, status.ConsentActive, status.CredentialAvailable, status.DataFields)
			if status.DisclosureFingerprint != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Disclosure fingerprint: %s\n", *status.DisclosureFingerprint)
			}
			if status.Disclosure != nil {
				d := status.Disclosure
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Provider endpoint: %s\nModel: %s\nRetention: %s\nMinimum transactional probability: %.2f\nMinimum choice probability: %.2f\n",
					d.Endpoint, d.ModelID, d.RetentionDeclaration, d.MinimumTransactional, d.MinimumChoiceProbability)
			}
			if status.Blocker != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Blocker: %s\n", *status.Blocker)
			}
			return nil
		}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print complete status JSON")
	return command
}

func newMailKindConsentCommand(grant bool) *cobra.Command {
	var jsonOutput bool
	action, short := "consent", "Consent to the current disclosure"
	if !grant {
		action, short = "revoke", "Revoke consent for a current or older disclosure"
	}
	command := &cobra.Command{Use: action + " <disclosure-fingerprint>", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			var result *generated.MailKindConsentResponse
			if grant {
				result, err = client.GrantMailKindConsent(cmd.Context(), args[0])
			} else {
				result, err = client.RevokeMailKindConsent(cmd.Context(), args[0])
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return writePersonScoringJSON(cmd, result)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Consent active: %t\nChanged: %t\nDisclosure fingerprint: %s\n", result.ConsentActive, result.Changed, result.DisclosureFingerprint)
			return nil
		}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "Print consent result JSON")
	return command
}

func init() {
	rootCmd.AddCommand(newMailCommand())
}
