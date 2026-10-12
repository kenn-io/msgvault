package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newPersonDeliveryPolicyCommand(operation string) *cobra.Command {
	var endpoint, reason string
	var revision int64
	var jsonOutput, allRoutes bool
	use := operation + "-delivery-policy <person-uid>"
	if operation == "set" {
		use += " <policy>"
	}
	command := &cobra.Command{
		Use: use,
		Short: map[string]string{
			"read":  "Read a person's delivery policy",
			"set":   "Set a person's delivery policy",
			"clear": "Clear a person's delivery policy override",
		}[operation],
		Long: "Use the canonical vcard_uid from 'person get' or 'person list --json'.\nOmit --endpoint for the person default. Only the owner can manage policies.\nWrites use the revision from read-delivery-policy and return before/after states.",
		Args: cobra.ExactArgs(map[string]int{"read": 1, "set": 2, "clear": 1}[operation]),
		RunE: func(cmd *cobra.Command, args []string) error {
			if operation != "read" {
				if err := positivePersonCLIRevision(cmd, revision, "delivery policy"); err != nil {
					return err
				}
				if operation == "set" && args[1] != string(store.DeliveryDraftOnly) && args[1] != string(store.DeliverySendAllowed) {
					return usageErr(cmd, errors.New("policy must be draft_only or send_allowed"))
				}
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			supported, err := client.SupportsAPISchemaVersion(cmd.Context(), deliveryPolicyMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("check delivery policy capability: %w", err)
			}
			if !supported {
				return fmt.Errorf("delivery policies require daemon API schema %s or newer; upgrade the daemon", deliveryPolicyMinAPISchemaVersion)
			}
			var result any
			var state *store.DeliveryPolicyState
			if operation == "read" {
				state, err = client.GetDeliveryPolicy(cmd.Context(), store.DeliveryPolicyQuery{PersonUID: args[0], Endpoint: endpoint})
				result = state
			} else {
				write := store.DeliveryPolicyWrite{PersonUID: args[0], Endpoint: endpoint, ExpectedRevision: revision, Reason: reason}
				if allRoutes {
					write.ScopeAcknowledgement = store.DeliveryPersonAllRoutes
				}
				var receipt *store.DeliveryPolicyReceipt
				if operation == "set" {
					write.Policy = store.DeliveryPolicy(args[1])
					receipt, err = client.SetDeliveryPolicy(cmd.Context(), write)
				} else {
					receipt, err = client.ClearDeliveryPolicy(cmd.Context(), write)
				}
				if err == nil {
					state, result = &receipt.After, receipt
				}
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout()), result, json.Deterministic(true))
			}
			stored := "-"
			if state.StoredPolicy != nil {
				stored = string(*state.StoredPolicy)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "PERSON UID\tENDPOINT\tREVISION\tSTORED POLICY\tEFFECTIVE POLICY\tSOURCE\tREASON")
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", textutil.SanitizeTerminal(state.PersonUID), textutil.SanitizeTerminal(state.Endpoint), state.Revision, textutil.SanitizeTerminal(stored), textutil.SanitizeTerminal(string(state.EffectivePolicy)), textutil.SanitizeTerminal(state.InheritanceSource), textutil.SanitizeTerminal(state.Reason))
			return writer.Flush()
		},
	}
	command.Flags().StringVar(&endpoint, "endpoint", "", "Saved email address; omit for the person default")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	if operation != "read" {
		command.Flags().Int64Var(&revision, "expected-revision", 0, "Revision from read-delivery-policy")
		command.Flags().StringVar(&reason, "reason", "", "Owner's reason for the change")
		command.Flags().BoolVar(&allRoutes, "person-all-routes", false, "Acknowledge approval of the person default across routes")
		_ = command.MarkFlagRequired("expected-revision")
		_ = command.MarkFlagRequired("reason")
	}
	return command
}
