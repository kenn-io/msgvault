package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	personAttributesJSONOutput   bool
	personAttributesHistory      bool
	personAttributesSlug         string
	personAttributeValue         string
	personAttributeValueJSON     string
	personAttributeOrdinal       int64
	personAttributeOrdinalSet    bool
	personAttributeSource        string
	personAttributeSourceRef     string
	personAttributeConfidence    float64
	personAttributeConfidenceSet bool
	personAttributeActor         string
	personAttributeExpectedID    int64
	personAttributeDryRun        bool
)

var personAttributesCmd = &cobra.Command{
	Use:   "attributes",
	Short: "Inspect and set a person's typed attribute values",
	Long: "Inspect and set a person's typed attribute values. Setting a value\n" +
		"supersedes the previous one rather than overwriting it, so history stays\n" +
		"readable. Read-only derived definitions are listed with no value until\n" +
		"their producing subsystem supplies one.",
}

var personAttributesListCmd = &cobra.Command{
	Use:   cmdUseList + " <person-id>",
	Short: "List a person's attribute definitions and values",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		personID, err := positivePersonCLIArg(cmd, args[0], "person")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := listCLIPersonAttributes(cmd, client, personID,
			personAttributesHistory, personAttributesSlug)
		if err != nil {
			return err
		}
		if resp.JSON200 == nil {
			return errors.New("person attributes response was empty")
		}
		if personAttributesJSONOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "SLUG\tORDINAL\tVALUE\tSOURCE\tACTIVE FROM\tACTIVE UNTIL\tMODE")
		for _, group := range resp.JSON200.Attributes {
			mode := attributeDefinitionMode(group.Definition)
			if len(group.Current) == 0 && len(group.History) == 0 {
				_, _ = fmt.Fprintf(w, "%s\t-\t-\t-\t-\t-\t%s\n", group.Definition.Slug, mode)
				continue
			}
			rows := group.Current
			if personAttributesHistory {
				rows = group.History
			}
			for _, value := range rows {
				_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
					value.DefinitionSlug, value.Ordinal,
					formatCLIAttributeValue(value.Value), value.Source,
					value.ActiveFrom.Format(time.RFC3339),
					formatCLIOptionalTime(value.ActiveUntil), mode)
			}
		}
		return w.Flush()
	},
}

var personAttributesSetCmd = &cobra.Command{
	Use:   "set <person-id> <slug>",
	Short: "Set a person's attribute value",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		personAttributeOrdinalSet = cmd.Flags().Changed("ordinal")
		personAttributeConfidenceSet = cmd.Flags().Changed("confidence")
		personID, err := positivePersonCLIArg(cmd, args[0], "person")
		if err != nil {
			return err
		}
		slug := strings.TrimSpace(args[1])
		if slug == "" {
			return usageErr(cmd, errors.New("attribute slug must not be empty"))
		}
		hasScalar := cmd.Flags().Changed("value")
		hasJSON := cmd.Flags().Changed("value-json")
		expectedValueIDSet := cmd.Flags().Changed("expected-value-id")
		switch {
		case hasScalar && hasJSON:
			return usageErr(cmd, errors.New("--value and --value-json are mutually exclusive"))
		case !hasScalar && !hasJSON:
			return usageErr(cmd, errors.New("--value or --value-json is required"))
		}
		if expectedValueIDSet && personAttributeExpectedID < 1 {
			return usageErr(cmd, errors.New("--expected-value-id must be a positive integer"))
		}

		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()

		body := generated.SetPersonAttributeBody{}
		if hasJSON {
			document, readErr := readCLIDocument(cmd, personAttributeValueJSON)
			if readErr != nil {
				return readErr
			}
			decoder := json.NewDecoder(strings.NewReader(string(document)))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&body.Value); err != nil {
				return usageErr(cmd, fmt.Errorf("invalid --value-json document: %w", err))
			}
		} else {
			definition, defErr := cliPersonAttributeDefinition(cmd, client, personID, slug)
			if defErr != nil {
				return defErr
			}
			if err := applyCLIScalarAttributeValue(
				cmd, &body.Value, definition.ValueType, personAttributeValue); err != nil {
				return err
			}
		}
		source := strings.TrimSpace(personAttributeSource)
		if source == "" {
			source = "user"
		}
		typedSource := generated.SetPersonAttributeRequestSource(source)
		body.Source = &typedSource
		if sourceRef := strings.TrimSpace(personAttributeSourceRef); sourceRef != "" {
			body.SourceRef = &sourceRef
		}
		if personAttributeConfidenceSet {
			body.Confidence = &personAttributeConfidence
		}
		if actor := strings.TrimSpace(personAttributeActor); actor != "" {
			body.Actor = &actor
		}
		if personAttributeOrdinalSet {
			body.Ordinal = &personAttributeOrdinal
		}
		if expectedValueIDSet {
			body.ExpectedValueID = &personAttributeExpectedID
		}
		if err := body.Validate(); err != nil {
			return usageErr(cmd, fmt.Errorf("invalid person attribute value: %w", err))
		}

		query := &generated.SetPersonAttributeQuery{}
		if personAttributeDryRun {
			dryRun := true
			query.DryRun = &dryRun
		}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.SetPersonAttributeResp, error) {
				return api.SetPersonAttributeWithResponse(cmd.Context(),
					&generated.SetPersonAttributeRequestOptions{
						PathParams: &generated.SetPersonAttributePath{ID: personID, Slug: slug},
						Query:      query,
						Body:       &body,
					})
			})
		if err != nil {
			return err
		}
		return writeCLIPersonAttributeWrite(cmd, resp.JSON200)
	},
}

var personAttributesClearCmd = &cobra.Command{
	Use:   "clear <person-id> <slug>",
	Short: "Supersede a person's current attribute value",
	Long: "Supersede a person's current attribute value. The historical row is\n" +
		"retained; this is not a delete.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		personAttributeOrdinalSet = cmd.Flags().Changed("ordinal")
		expectedValueIDSet := cmd.Flags().Changed("expected-value-id")
		personID, err := positivePersonCLIArg(cmd, args[0], "person")
		if err != nil {
			return err
		}
		slug := strings.TrimSpace(args[1])
		if slug == "" {
			return usageErr(cmd, errors.New("attribute slug must not be empty"))
		}
		if expectedValueIDSet && personAttributeExpectedID < 1 {
			return usageErr(cmd, errors.New("--expected-value-id must be a positive integer"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()

		query := &generated.ClearPersonAttributeQuery{}
		if personAttributeOrdinalSet {
			query.Ordinal = &personAttributeOrdinal
		}
		if expectedValueIDSet {
			query.ExpectedValueID = &personAttributeExpectedID
		}
		if personAttributeDryRun {
			dryRun := true
			query.DryRun = &dryRun
		}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ClearPersonAttributeResp, error) {
				return api.ClearPersonAttributeWithResponse(cmd.Context(),
					&generated.ClearPersonAttributeRequestOptions{
						PathParams: &generated.ClearPersonAttributePath{ID: personID, Slug: slug},
						Query:      query,
					})
			})
		if err != nil {
			return err
		}
		return writeCLIPersonAttributeWrite(cmd, resp.JSON200)
	},
}

func listCLIPersonAttributes(
	cmd *cobra.Command, client *daemonclient.Client,
	personID int64, history bool, slug string,
) (*generated.ListPersonAttributesResp, error) {
	query := &generated.ListPersonAttributesQuery{}
	if history {
		includeHistory := true
		query.History = &includeHistory
	}
	if trimmed := strings.TrimSpace(slug); trimmed != "" {
		query.Slug = &trimmed
	}
	return daemonclient.APIResponse(client,
		func(api *apiclient.Client) (*generated.ListPersonAttributesResp, error) {
			return api.ListPersonAttributesWithResponse(cmd.Context(),
				&generated.ListPersonAttributesRequestOptions{
					PathParams: &generated.ListPersonAttributesPath{ID: personID},
					Query:      query,
				})
		})
}

func cliPersonAttributeDefinition(
	cmd *cobra.Command, client *daemonclient.Client, personID int64, slug string,
) (*generated.AttributeDefinition, error) {
	resp, err := listCLIPersonAttributes(cmd, client, personID, false, slug)
	if err != nil {
		return nil, err
	}
	if resp.JSON200 == nil {
		return nil, errors.New("person attributes response was empty")
	}
	for i := range resp.JSON200.Attributes {
		if resp.JSON200.Attributes[i].Definition.Slug == slug {
			return &resp.JSON200.Attributes[i].Definition, nil
		}
	}
	return nil, fmt.Errorf("no active attribute definition with slug %q", slug)
}

func applyCLIScalarAttributeValue(
	cmd *cobra.Command, value *generated.AttributeValue, valueType, raw string,
) error {
	value.Type = valueType
	trimmed := strings.TrimSpace(raw)
	switch valueType {
	case "text":
		value.Text = &trimmed
	case "integer":
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return usageErr(cmd, fmt.Errorf("--value %q must be an integer", trimmed))
		}
		value.Integer = &parsed
	case "real":
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return usageErr(cmd, fmt.Errorf("--value %q must be a number", trimmed))
		}
		value.Real = &parsed
	case "boolean":
		parsed, err := strconv.ParseBool(trimmed)
		if err != nil {
			return usageErr(cmd, fmt.Errorf("--value %q must be a boolean", trimmed))
		}
		value.Boolean = &parsed
	case "date":
		if _, err := time.Parse("2006-01-02", trimmed); err != nil {
			return usageErr(cmd, fmt.Errorf("--value %q must be a YYYY-MM-DD date", trimmed))
		}
		value.Date = &trimmed
	case "timestamp":
		parsed, err := time.Parse(time.RFC3339, trimmed)
		if err != nil {
			return usageErr(cmd, fmt.Errorf(
				"--value %q must be an RFC3339 timestamp", trimmed))
		}
		utc := parsed.UTC()
		value.Timestamp = &utc
	default:
		return usageErr(cmd, fmt.Errorf(
			"value type %s needs a structured value; use --value-json", valueType))
	}
	return nil
}

func formatCLIAttributeValue(value generated.AttributeValue) string {
	switch {
	case value.Text != nil:
		return *value.Text
	case value.Integer != nil:
		return strconv.FormatInt(*value.Integer, 10)
	case value.Real != nil:
		return strconv.FormatFloat(*value.Real, 'g', -1, 64)
	case value.Boolean != nil:
		return strconv.FormatBool(*value.Boolean)
	case value.Date != nil:
		return *value.Date
	case value.Timestamp != nil:
		return value.Timestamp.Format(time.RFC3339)
	case value.RecordType != nil && value.RecordID != nil:
		return fmt.Sprintf("%s:%d", *value.RecordType, *value.RecordID)
	case len(value.JSON) > 0:
		return string(value.JSON)
	default:
		return "-"
	}
}

func formatCLIOptionalTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func writeCLIPersonAttributeWrite(
	cmd *cobra.Command, write *generated.PersonAttributeWrite,
) error {
	if write == nil {
		return errors.New("person attribute response was empty")
	}
	if personAttributesJSONOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(write)
	}
	prefix := ""
	if write.DryRun {
		prefix = "Dry run: "
	}
	if write.Superseded != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"%sSuperseded %s ordinal %d: %s (active until %s)\n",
			prefix, write.Superseded.DefinitionSlug, write.Superseded.Ordinal,
			formatCLIAttributeValue(write.Superseded.Value),
			formatCLIOptionalTime(write.Superseded.ActiveUntil))
	}
	if write.Value != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"%sSet %s ordinal %d: %s (source %s, active from %s)\n",
			prefix, write.Value.DefinitionSlug, write.Value.Ordinal,
			formatCLIAttributeValue(write.Value.Value), write.Value.Source,
			write.Value.ActiveFrom.Format(time.RFC3339))
	}
	return nil
}

func init() {
	personCmd.AddCommand(personAttributesCmd)
	personAttributesCmd.AddCommand(personAttributesListCmd,
		personAttributesSetCmd, personAttributesClearCmd)
	for _, command := range []*cobra.Command{
		personAttributesListCmd, personAttributesSetCmd, personAttributesClearCmd,
	} {
		command.Flags().BoolVar(&personAttributesJSONOutput, flagJSON, false, "Output as JSON")
	}
	personAttributesListCmd.Flags().BoolVar(&personAttributesHistory,
		"history", false, "Include superseded values")
	personAttributesListCmd.Flags().StringVar(&personAttributesSlug,
		"slug", "", "Restrict the listing to one definition slug")
	personAttributesSetCmd.Flags().StringVar(&personAttributeValue,
		"value", "", "Scalar value coerced to the definition's storage type")
	personAttributesSetCmd.Flags().StringVar(&personAttributeValueJSON,
		"value-json", "", "Typed value envelope as JSON, @path, or - for standard input")
	personAttributesSetCmd.Flags().StringVar(&personAttributeSource,
		"source", "", "Provenance: user, carddav_import, vcard_import, "+
			"archive_observation, extraction, enrichment, or system")
	personAttributesSetCmd.Flags().StringVar(&personAttributeSourceRef,
		"source-ref", "", "Reference to the resource or message that produced the value")
	personAttributesSetCmd.Flags().Float64Var(&personAttributeConfidence,
		"confidence", 0, "Confidence between 0 and 1; only for derived or suggested values")
	personAttributesSetCmd.Flags().StringVar(&personAttributeActor,
		"actor", "", "Actor recorded with the value")
	for _, command := range []*cobra.Command{personAttributesSetCmd, personAttributesClearCmd} {
		command.Flags().Int64Var(&personAttributeExpectedID,
			"expected-value-id", 0,
			"Compare-and-swap: the current value ID expected to be superseded")
		command.Flags().Int64Var(&personAttributeOrdinal, "ordinal", 0,
			"Ordinal for a multi-valued definition")
		command.Flags().BoolVar(&personAttributeDryRun, "dry-run", false,
			"Validate and preview without writing")
	}
	personAttributesSetCmd.MarkFlagsMutuallyExclusive("value", "value-json")
}
