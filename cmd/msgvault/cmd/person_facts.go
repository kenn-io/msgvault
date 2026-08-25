package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personfacts"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type personFactsCLIOptions struct {
	jsonOutput       bool
	includeSensitive bool
	target           string
	limit            int64
	offset           int64
	evidenceKey      string
	supported        bool
}

func newPersonFactsCommand() *cobra.Command {
	options := &personFactsCLIOptions{}
	facts := &cobra.Command{
		Use:   "facts",
		Short: "Inspect person fact diagnostics and control pins",
	}

	catalog := &cobra.Command{
		Use:   "catalog",
		Short: "List eligible automatic person fact targets",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			query := &generated.ListPersonFactTargetsQuery{}
			if options.includeSensitive {
				includeSensitive := true
				query.IncludeSensitive = &includeSensitive
			}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonFactTargetsResp, error) {
					return api.ListPersonFactTargetsWithResponse(cmd.Context(),
						&generated.ListPersonFactTargetsRequestOptions{Query: query})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact catalog response was empty")
			}
			if options.jsonOutput {
				return writePersonFactCLIRawJSON(cmd, response.Body)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "KIND\tSLUG\tKEY\tREVISION\tTYPE\tCARDINALITY\tSENSITIVE")
			for _, target := range response.JSON200.Targets {
				_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%t\n",
					target.Kind, target.Slug, target.Key, target.Revision,
					target.ValueType, target.Cardinality, target.Sensitive)
			}
			return writer.Flush()
		},
	}
	catalog.Flags().BoolVar(&options.includeSensitive, "include-sensitive", false,
		"Include sensitive targets")

	evidence := &cobra.Command{
		Use:   "evidence <person-id>",
		Short: "List immutable person fact evidence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, query, err := personFactCLIHistoryQuery(cmd, args[0], options)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonFactEvidenceResp, error) {
					return api.ListPersonFactEvidenceWithResponse(cmd.Context(),
						&generated.ListPersonFactEvidenceRequestOptions{
							PathParams: &generated.ListPersonFactEvidencePath{ID: personID},
							Query: &generated.ListPersonFactEvidenceQuery{
								Target: query.target, Limit: query.limit, Offset: query.offset,
							},
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact evidence response was empty")
			}
			if options.jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200.Evidence)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tEVIDENCE KEY\tSOURCE\tSOURCE VERSION\tSUPPORTED\tRECORDED")
			for _, item := range response.JSON200.Evidence {
				_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%t\t%s\n",
					item.ID, item.EvidenceKey, item.SourceClass, pointerString(item.SourceVersion),
					item.Supported, item.RecordedTime.Format(time.RFC3339))
			}
			return writer.Flush()
		},
	}

	status := &cobra.Command{
		Use:   "evidence-status <person-id>",
		Short: "List immutable person fact evidence status events",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			page, err := personFactCLIPage(cmd, options)
			if err != nil {
				return err
			}
			query := &generated.ListPersonFactEvidenceStatusEventsQuery{
				Limit: page.limit, Offset: page.offset,
			}
			if evidenceKey := strings.TrimSpace(options.evidenceKey); evidenceKey != "" {
				query.EvidenceKey = &evidenceKey
			}
			if cmd.Flags().Changed("supported") {
				supported := options.supported
				query.Supported = &supported
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonFactEvidenceStatusEventsResp, error) {
					return api.ListPersonFactEvidenceStatusEventsWithResponse(cmd.Context(),
						&generated.ListPersonFactEvidenceStatusEventsRequestOptions{
							PathParams: &generated.ListPersonFactEvidenceStatusEventsPath{ID: personID},
							Query:      query,
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact evidence status response was empty")
			}
			if options.jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200.Events)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tGENERATION\tEVIDENCE KEY\tSOURCE VERSION\tSUPPORTED\tREASON\tTIMESTAMP")
			for _, item := range response.JSON200.Events {
				_, _ = fmt.Fprintf(writer, "%d\t%d\t%s\t%s\t%t\t%s\t%s\n",
					item.ID, item.GenerationID, item.EvidenceKey, pointerString(item.SourceVersion),
					item.Supported, item.Reason, item.CreatedAt.Format(time.RFC3339))
			}
			return writer.Flush()
		},
	}
	status.Flags().StringVar(&options.evidenceKey, "evidence-key", "", "Restrict to one evidence key")
	status.Flags().BoolVar(&options.supported, "supported", false,
		"Restrict to supported events; use --supported=false for unsupported events")

	claims := &cobra.Command{
		Use:   "claims <person-id>",
		Short: "List immutable person fact claims",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, query, err := personFactCLIHistoryQuery(cmd, args[0], options)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonFactClaimsResp, error) {
					return api.ListPersonFactClaimsWithResponse(cmd.Context(),
						&generated.ListPersonFactClaimsRequestOptions{
							PathParams: &generated.ListPersonFactClaimsPath{ID: personID},
							Query: &generated.ListPersonFactClaimsQuery{
								Target: query.target, Limit: query.limit, Offset: query.offset,
							},
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact claims response was empty")
			}
			if options.jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200.Claims)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tGENERATION\tTARGET\tRELATION\tORIGIN\tCLAIM KEY\tCREATED")
			for _, item := range response.JSON200.Claims {
				_, _ = fmt.Fprintf(writer, "%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
					item.ID, item.GenerationID, personFactCLIStoredTarget(item.Target), item.Relation,
					item.Origin, item.ClaimKey, item.CreatedAt.Format(time.RFC3339))
			}
			return writer.Flush()
		},
	}

	decisions := &cobra.Command{
		Use:   "decisions <person-id>",
		Short: "List immutable person fact decisions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, query, err := personFactCLIHistoryQuery(cmd, args[0], options)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonFactDecisionsResp, error) {
					return api.ListPersonFactDecisionsWithResponse(cmd.Context(),
						&generated.ListPersonFactDecisionsRequestOptions{
							PathParams: &generated.ListPersonFactDecisionsPath{ID: personID},
							Query: &generated.ListPersonFactDecisionsQuery{
								Target: query.target, Limit: query.limit, Offset: query.offset,
							},
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact decisions response was empty")
			}
			if options.jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200.Decisions)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "ID\tRESOLUTION\tCLAIM KEY\tACTION\tREASON\tPROJECTION\tCREATED")
			for _, item := range response.JSON200.Decisions {
				_, _ = fmt.Fprintf(writer, "%d\t%d\t%s\t%s\t%s\t%s\t%s\n",
					item.ID, item.ResolutionID, item.ClaimKey, item.Action, item.Reason,
					personFactCLIProjection(item.Projection), item.CreatedAt.Format(time.RFC3339))
			}
			return writer.Flush()
		},
	}

	pins := &cobra.Command{
		Use:   "pins <person-id>",
		Short: "List effective person fact pins",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonFactPinsResp, error) {
					return api.ListPersonFactPinsWithResponse(cmd.Context(),
						&generated.ListPersonFactPinsRequestOptions{
							PathParams: &generated.ListPersonFactPinsPath{ID: personID},
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact pins response was empty")
			}
			if options.jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(response.JSON200.Pins)
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "TARGET\tPINNED\tACTOR\tEVENT")
			for _, item := range response.JSON200.Pins {
				target, err := personFactCLITarget(item.Target)
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(writer, "%s\t%t\t%s\t%s\n",
					target, item.Pinned,
					personFactCLIOptionalString(item.Actor), personFactCLIOptionalInt64(item.EventID))
			}
			return writer.Flush()
		},
	}

	pin := newPersonFactPinCommand(options, true)
	unpin := newPersonFactPinCommand(options, false)

	for _, command := range []*cobra.Command{catalog, evidence, status, claims, decisions, pins, pin, unpin} {
		command.Flags().BoolVar(&options.jsonOutput, flagJSON, false, "Output as JSON")
	}
	for _, command := range []*cobra.Command{evidence, claims, decisions} {
		command.Flags().StringVar(&options.target, "target", "",
			"Exact target as kind:key:sha256:<64 lowercase hex characters>")
		addPersonFactCLIPageFlags(command, options)
	}
	addPersonFactCLIPageFlags(status, options)
	facts.AddCommand(catalog, evidence, status, claims, decisions, pins, pin, unpin)
	return facts
}

func newPersonFactPinCommand(options *personFactsCLIOptions, pinned bool) *cobra.Command {
	action := "pin"
	if !pinned {
		action = "unpin"
	}
	return &cobra.Command{
		Use:   action + " <person-id> <kind> <key>",
		Short: action + " an automatic person fact target",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			kind := strings.TrimSpace(args[1])
			if kind != string(generated.Attribute) &&
				kind != string(generated.SetPersonFactPinPathKindEmployment) {
				return usageErr(cmd, fmt.Errorf("unknown person fact target kind %q", args[1]))
			}
			key := strings.TrimSpace(args[2])
			if key == "" {
				return usageErr(cmd, errors.New("person fact target key must not be empty"))
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.SetPersonFactPinBody{Pinned: pinned}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.SetPersonFactPinResp, error) {
					return api.SetPersonFactPinWithResponse(cmd.Context(),
						&generated.SetPersonFactPinRequestOptions{
							PathParams: &generated.SetPersonFactPinPath{
								ID: personID, Kind: generated.SetPersonFactPinPathKind(kind), Key: key,
							},
							Body: &body,
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person fact pin response was empty")
			}
			if options.jsonOutput {
				return writePersonFactCLIRawJSON(cmd, response.Body)
			}
			state := "unpinned"
			if response.JSON200.State.Pinned {
				state = "pinned"
			}
			target, err := personFactCLITarget(response.JSON200.State.Target)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n",
				target, state)
			for _, projection := range response.JSON200.Projections {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Projection: %s:%d\n",
					projection.Kind, projection.RowID)
			}
			return nil
		},
	}
}

type personFactCLIPageQuery struct {
	target *string
	limit  *int64
	offset *int64
}

func personFactCLIHistoryQuery(
	cmd *cobra.Command, personIDRaw string, options *personFactsCLIOptions,
) (int64, personFactCLIPageQuery, error) {
	personID, err := positivePersonCLIArg(cmd, personIDRaw, personValue)
	if err != nil {
		return 0, personFactCLIPageQuery{}, err
	}
	query, err := personFactCLIPage(cmd, options)
	if err != nil {
		return 0, query, err
	}
	if options.target != "" {
		target, err := personfacts.DecodeTargetRef(options.target)
		if err != nil {
			return 0, query, usageErr(cmd, fmt.Errorf("--target: %w", err))
		}
		encoded, err := personfacts.EncodeTargetRef(target)
		if err != nil {
			return 0, query, usageErr(cmd, fmt.Errorf("--target: %w", err))
		}
		query.target = &encoded
	}
	return personID, query, nil
}

func personFactCLIPage(
	cmd *cobra.Command, options *personFactsCLIOptions,
) (personFactCLIPageQuery, error) {
	query := personFactCLIPageQuery{}
	if cmd.Flags().Changed("limit") {
		if options.limit < 1 || options.limit > 200 {
			return query, usageErr(cmd, errors.New("--limit must be between 1 and 200"))
		}
		limit := options.limit
		query.limit = &limit
	}
	if cmd.Flags().Changed("offset") {
		if options.offset < 0 {
			return query, usageErr(cmd, errors.New("--offset must not be negative"))
		}
		offset := options.offset
		query.offset = &offset
	}
	return query, nil
}

func addPersonFactCLIPageFlags(command *cobra.Command, options *personFactsCLIOptions) {
	command.Flags().Int64Var(&options.limit, "limit", 0, "Maximum rows to return (1-200)")
	command.Flags().Int64Var(&options.offset, "offset", 0, "Zero-based row offset")
}

func personFactCLITarget(target generated.TargetRef) (string, error) {
	return personfacts.EncodeTargetRef(personfacts.TargetRef{
		Kind: personfacts.TargetKind(target.Kind), Key: target.Key, Revision: target.Revision,
	})
}

func personFactCLIStoredTarget(target generated.TargetRef) string {
	encoded, err := personFactCLITarget(target)
	if err == nil {
		return encoded
	}
	return target.Kind + ":" + target.Key + ":" + target.Revision
}

func personFactCLIProjection(projection *generated.ProjectionRef) string {
	if projection == nil {
		return "-"
	}
	return projection.Kind + ":" + strconv.FormatInt(projection.RowID, 10)
}

func personFactCLIOptionalString(value *string) string {
	if value == nil || *value == "" {
		return "-"
	}
	return *value
}

func personFactCLIOptionalInt64(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}

func writePersonFactCLIRawJSON(cmd *cobra.Command, body []byte) error {
	if _, err := cmd.OutOrStdout().Write(body); err != nil {
		return err
	}
	if !bytes.HasSuffix(body, []byte("\n")) {
		_, err := cmd.OutOrStdout().Write([]byte("\n"))
		return err
	}
	return nil
}
