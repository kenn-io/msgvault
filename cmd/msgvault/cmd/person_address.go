package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func newPersonAddressCommand() *cobra.Command {
	command := &cobra.Command{Use: "address", Short: "Review and explicitly apply postal addresses"}
	list := &cobra.Command{Use: "list <person-id>", Short: "Read postal addresses and local assessments", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetPersonStructuredProfileResp, error) {
			return api.GetPersonStructuredProfileWithResponse(cmd.Context(), &generated.GetPersonStructuredProfileRequestOptions{PathParams: &generated.GetPersonStructuredProfilePath{ID: id}})
		})
		if err != nil {
			return err
		}
		addresses := make([]generated.PersonAddress, 0)
		for _, address := range response.JSON200.Addresses {
			if address.AddressKind == "postal" {
				addresses = append(addresses, address)
			}
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(addresses)
	}}
	var assessInput string
	var external bool
	assess := &cobra.Command{Use: "assess <person-id>", Short: "Assess a JSON address without saving", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		var input generated.Input
		if err := readPostalCLIInput(assessInput, &input); err != nil {
			return err
		}
		if input.AddressKind == "" {
			input.AddressKind = "postal"
		}
		if input.AddressKind != "postal" {
			return errors.New("only postal addresses can be assessed")
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.AssessPersonPostalAddressBody{Address: input, AllowExternal: &external}
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.AssessPersonPostalAddressResp, error) {
			return api.AssessPersonPostalAddressWithResponse(cmd.Context(), &generated.AssessPersonPostalAddressRequestOptions{PathParams: &generated.AssessPersonPostalAddressPath{ID: id}, Body: &body})
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200)
	}}
	assess.Flags().StringVar(&assessInput, "input", "", "Path to raw postal input JSON")
	assess.Flags().BoolVar(&external, "external", false, "Explicitly send postal components to the configured provider")
	var applyInput string
	var revision, supersede int64
	var add bool
	apply := &cobra.Command{Use: "apply <person-id>", Short: "Add or supersede one address at a reviewed person revision", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		if revision < 1 {
			return errors.New("--revision must be the positive reviewed person revision")
		}
		if add == cmd.Flags().Changed("supersede") || (!add && supersede < 1) {
			return errors.New("choose exactly one of --add or --supersede <positive-address-id>")
		}
		var input generated.PersonAddressInputRequest
		if err := readPostalCLIInput(applyInput, &input); err != nil {
			return err
		}
		if input.AddressKind == "" {
			input.AddressKind = "postal"
		}
		if input.AddressKind != "postal" {
			return errors.New("only postal addresses can be applied")
		}
		// A correction is a new owner declaration. Preserve presentation and an
		// exact claim origin, rather than copying imported identity/confidence.
		envelope := input.Envelope
		input.Envelope = generated.ValueEnvelopeInput{Source: "user", Ordinal: envelope.Ordinal, Pref: envelope.Pref, TypeLabel: envelope.TypeLabel, TypeTokens: envelope.TypeTokens}
		if envelope.SourceRef != nil && strings.HasPrefix(*envelope.SourceRef, "person-fact-claim:") {
			input.Envelope.SourceRef = envelope.SourceRef
		}
		patch := &generated.PersonAddressPatchRequest{Add: []generated.PersonAddressInputRequest{input}}
		if !add {
			patch.Supersede = []int64{supersede}
		}
		body := generated.PatchPersonStructuredProfileBody{Addresses: patch}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PatchPersonStructuredProfileResp, error) {
			return api.PatchPersonStructuredProfileWithResponse(cmd.Context(), &generated.PatchPersonStructuredProfileRequestOptions{PathParams: &generated.PatchPersonStructuredProfilePath{ID: id}, Header: &generated.PatchPersonStructuredProfileHeaders{IfMatch: personCLIETag(id, revision)}, Body: &body})
		})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200)
	}}
	apply.Flags().StringVar(&applyInput, "input", "", "Path to postal address request JSON; original_input preserves the initial draft")
	apply.Flags().Int64Var(&revision, "revision", 0, "Reviewed person revision (required)")
	apply.Flags().BoolVar(&add, "add", false, "Add a new address")
	apply.Flags().Int64Var(&supersede, "supersede", 0, "Explicit address row to supersede")
	command.AddCommand(list, assess, apply)
	return command
}

func readPostalCLIInput(path string, value any) error {
	if path == "" {
		return errors.New("--input requires a JSON file path")
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open address input: %w", err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode address input: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("address input must contain one JSON object")
	}
	return nil
}
