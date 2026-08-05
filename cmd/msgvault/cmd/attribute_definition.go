package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	attributeDefinitionJSON             bool
	attributeDefinitionObjectType       string
	attributeDefinitionIncludeHidden    bool
	attributeDefinitionDocument         string
	attributeDefinitionDryRun           bool
	attributeDefinitionLabel            string
	attributeDefinitionDescription      string
	attributeDefinitionClearDescription bool
)

var attributeDefinitionCmd = &cobra.Command{
	Use:   "attribute-definition",
	Short: "Manage portable attribute definitions",
	Long: "Manage portable attribute definitions. Definitions are metadata rows:\n" +
		"creating one never changes the database schema. The registry does not\n" +
		"offer a uniqueness flag, because Msgvault only claims constraints a\n" +
		"portable database index enforces.",
}

var attributeDefinitionListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List attribute definitions",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		objectType, err := attributeObjectTypeArg(cmd)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()

		options := &generated.ListAttributeDefinitionsRequestOptions{
			Query: &generated.ListAttributeDefinitionsQuery{},
		}
		if objectType != "" {
			options.Query.ObjectType = &objectType
		}
		if attributeDefinitionIncludeHidden {
			includeHidden := true
			options.Query.IncludeHidden = &includeHidden
		}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ListAttributeDefinitionsResp, error) {
				return api.ListAttributeDefinitionsWithResponse(cmd.Context(), options)
			})
		if err != nil {
			return err
		}
		if resp.JSON200 == nil {
			return errors.New("attribute definitions response was empty")
		}
		if attributeDefinitionJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.Definitions)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w,
			"ID\tSLUG\tLABEL\tOBJECT\tVALUE TYPE\tWIDGET\tCARDINALITY\tOWNER\tMODE\tUNIVERSAL ID")
		for _, definition := range resp.JSON200.Definitions {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				definition.ID, definition.Slug, definition.Label,
				definition.ObjectType, definition.ValueType, definition.FieldType,
				definition.Cardinality, definition.Ownership,
				attributeDefinitionMode(definition), definition.UniversalID)
		}
		return w.Flush()
	},
}

func attributeDefinitionMode(definition generated.AttributeDefinition) string {
	if definition.DerivedSource != nil {
		return "derived (" + *definition.DerivedSource + ")"
	}
	if !definition.IsActive {
		return "inactive"
	}
	return "writable"
}

var attributeDefinitionGetCmd = &cobra.Command{
	Use:   "get <definition-id>",
	Short: "Get one attribute definition",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "attribute definition")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := getCLIAttributeDefinition(cmd, client, id)
		if err != nil {
			return err
		}
		return writeCLIAttributeDefinition(cmd, resp.JSON200)
	},
}

var attributeDefinitionCreateCmd = &cobra.Command{
	Use:   "create --definition <json|@path|->",
	Short: "Create a user attribute definition from a JSON document",
	Long: "Create a user attribute definition from a JSON document. Pass the\n" +
		"document inline, as @path to read a file, or - to read standard input.\n" +
		"--dry-run validates the document locally and prints what would be sent.",
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := decodeCLIAttributeDefinitionDocument(cmd, attributeDefinitionDocument)
		if err != nil {
			return err
		}
		if attributeDefinitionDryRun {
			encoded, marshalErr := json.MarshalIndent(body, "", "  ")
			if marshalErr != nil {
				return marshalErr
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Would create attribute definition:\n%s\n", encoded)
			return nil
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := daemonclient.APIResponseWithStatuses(client,
			[]int{http.StatusCreated},
			func(api *apiclient.Client) (*generated.CreateAttributeDefinitionResp, error) {
				return api.CreateAttributeDefinitionWithResponse(cmd.Context(),
					&generated.CreateAttributeDefinitionRequestOptions{Body: body})
			})
		if err != nil {
			return err
		}
		return writeCLIAttributeDefinition(cmd, resp.JSON201)
	},
}

var attributeDefinitionRenameCmd = &cobra.Command{
	Use:   "rename <definition-id> --label <text>",
	Short: "Change an attribute definition's human label",
	Long: "Change an attribute definition's human label. The universal identifier\n" +
		"and slug are immutable, so a rename never breaks a stored reference.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "attribute definition")
		if err != nil {
			return err
		}
		label := strings.TrimSpace(attributeDefinitionLabel)
		description := strings.TrimSpace(attributeDefinitionDescription)
		if label == "" && !attributeDefinitionClearDescription && description == "" {
			return usageErr(cmd, errors.New("--label or --description is required"))
		}
		if attributeDefinitionClearDescription && description != "" {
			return usageErr(cmd, errors.New(
				"--clear-description cannot be combined with --description"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIAttributeDefinition(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("attribute definition response was empty")
		}
		etag := fmt.Sprintf(`"attribute-definition-%d-r%d"`, id, current.JSON200.Revision)
		body := generated.PatchAttributeDefinitionBody{}
		if label != "" {
			body.Label = &label
		}
		switch {
		case attributeDefinitionClearDescription:
			empty := ""
			body.Description = &empty
		case description != "":
			body.Description = &description
		}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.PatchAttributeDefinitionResp, error) {
				return api.PatchAttributeDefinitionWithResponse(cmd.Context(),
					&generated.PatchAttributeDefinitionRequestOptions{
						PathParams: &generated.PatchAttributeDefinitionPath{ID: id},
						Header:     &generated.PatchAttributeDefinitionHeaders{IfMatch: etag},
						Body:       &body,
					})
			})
		if err != nil {
			return err
		}
		return writeCLIAttributeDefinition(cmd, resp.JSON200)
	},
}

var attributeDefinitionDeleteCmd = &cobra.Command{
	Use:   "delete <definition-id>",
	Short: "Delete a user attribute definition",
	Long: "Delete a user attribute definition. The daemon refuses definitions that\n" +
		"ship with Msgvault, definitions marked not deletable, and definitions\n" +
		"that still have stored values.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "attribute definition")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIAttributeDefinition(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("attribute definition response was empty")
		}
		etag := fmt.Sprintf(`"attribute-definition-%d-r%d"`, id, current.JSON200.Revision)
		_, err = daemonclient.APIResponseWithStatuses(client,
			[]int{http.StatusNoContent},
			func(api *apiclient.Client) (*generated.DeleteAttributeDefinitionResp, error) {
				return api.DeleteAttributeDefinitionWithResponse(cmd.Context(),
					&generated.DeleteAttributeDefinitionRequestOptions{
						PathParams: &generated.DeleteAttributeDefinitionPath{ID: id},
						Header:     &generated.DeleteAttributeDefinitionHeaders{IfMatch: etag},
					})
			})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted attribute definition %d\n", id)
		return nil
	},
}

func attributeObjectTypeArg(cmd *cobra.Command) (string, error) {
	value := strings.TrimSpace(attributeDefinitionObjectType)
	if value == "" || value == "person" || value == "organization" {
		return value, nil
	}
	return "", usageErr(cmd, errors.New("object type must be person or organization"))
}

func readCLIDocument(cmd *cobra.Command, value string) ([]byte, error) {
	trimmed := strings.TrimSpace(value)
	switch {
	case trimmed == "":
		return nil, usageErr(cmd, errors.New("a JSON document is required"))
	case trimmed == "-":
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("read document from standard input: %w", err)
		}
		return data, nil
	case strings.HasPrefix(trimmed, "@"):
		filename := strings.TrimPrefix(trimmed, "@")
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("read document %s: %w", filename, err)
		}
		return data, nil
	default:
		return []byte(trimmed), nil
	}
}

func decodeCLIAttributeDefinitionDocument(
	cmd *cobra.Command, value string,
) (*generated.CreateAttributeDefinitionBody, error) {
	data, err := readCLIDocument(cmd, value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, usageErr(cmd, errors.New(
			"attribute definition document must be a JSON object"))
	}
	for _, reserved := range []string{"is_unique", "unique"} {
		if _, present := fields[reserved]; present {
			return nil, usageErr(cmd, errors.New(
				"attribute definition uniqueness is not supported: a uniqueness claim "+
					"must be backed by a portable database index"))
		}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var body generated.CreateAttributeDefinitionBody
	if err := decoder.Decode(&body); err != nil {
		return nil, usageErr(cmd, fmt.Errorf("invalid attribute definition document: %w", err))
	}
	if err := body.Validate(); err != nil {
		return nil, usageErr(cmd, fmt.Errorf("invalid attribute definition document: %w", err))
	}
	return &body, nil
}

func getCLIAttributeDefinition(
	cmd *cobra.Command, client *daemonclient.Client, id int64,
) (*generated.GetAttributeDefinitionResp, error) {
	return daemonclient.APIResponse(client,
		func(api *apiclient.Client) (*generated.GetAttributeDefinitionResp, error) {
			return api.GetAttributeDefinitionWithResponse(cmd.Context(),
				&generated.GetAttributeDefinitionRequestOptions{
					PathParams: &generated.GetAttributeDefinitionPath{ID: id},
				})
		})
}

func writeCLIAttributeDefinition(
	cmd *cobra.Command, definition *generated.AttributeDefinition,
) error {
	if definition == nil {
		return errors.New("attribute definition response was empty")
	}
	if attributeDefinitionJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(definition)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Definition: %d\nSlug: %s\nLabel: %s\nObject: %s\nValue type: %s\n"+
			"Widget: %s\nCardinality: %s\nOwner: %s\nMode: %s\nUniversal ID: %s\n"+
			"Revision: %d\n",
		definition.ID, definition.Slug, definition.Label, definition.ObjectType,
		definition.ValueType, definition.FieldType, definition.Cardinality,
		definition.Ownership, attributeDefinitionMode(*definition),
		definition.UniversalID, definition.Revision)
	return nil
}

func init() {
	rootCmd.AddCommand(attributeDefinitionCmd)
	attributeDefinitionCmd.AddCommand(attributeDefinitionListCmd,
		attributeDefinitionGetCmd, attributeDefinitionCreateCmd,
		attributeDefinitionRenameCmd, attributeDefinitionDeleteCmd)
	for _, command := range []*cobra.Command{
		attributeDefinitionListCmd, attributeDefinitionGetCmd,
		attributeDefinitionCreateCmd, attributeDefinitionRenameCmd,
	} {
		command.Flags().BoolVar(&attributeDefinitionJSON, flagJSON, false, "Output as JSON")
	}
	attributeDefinitionListCmd.Flags().StringVar(&attributeDefinitionObjectType,
		"object-type", "", "Filter by object type: person or organization")
	attributeDefinitionListCmd.Flags().BoolVar(&attributeDefinitionIncludeHidden,
		"include-hidden", false, "Include deactivated definitions")
	attributeDefinitionCreateCmd.Flags().StringVar(&attributeDefinitionDocument,
		"definition", "", "Definition JSON document, @path, or - for standard input")
	attributeDefinitionCreateCmd.Flags().BoolVar(&attributeDefinitionDryRun,
		"dry-run", false, "Validate and print the request without sending it")
	attributeDefinitionRenameCmd.Flags().StringVar(&attributeDefinitionLabel,
		"label", "", "New human label")
	attributeDefinitionRenameCmd.Flags().StringVar(&attributeDefinitionDescription,
		"description", "", "New description")
	attributeDefinitionRenameCmd.Flags().BoolVar(&attributeDefinitionClearDescription,
		"clear-description", false, "Clear the description")
}
