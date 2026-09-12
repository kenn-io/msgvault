package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	organizationJSON                                                                                   bool
	organizationLimit, organizationOffset                                                              int64
	organizationQuery, organizationName, organizationKind, organizationDomain, organizationDescription string
	organizationShowHistory, organizationIncludeRetired, organizationIncludeSuperseded                 bool
	organizationTextValue, organizationSource, organizationDefinitionSlugValue                         string
	organizationExpectedValueID, organizationAttributeOrdinal                                          int64
	organizationDryRun                                                                                 bool
)

var organizationCmd = &cobra.Command{Use: "organization", Aliases: []string{"org"}, Short: "Manage curated organizations and their employment records"}

var organizationListCmd = &cobra.Command{Use: cmdUseList, Short: "List curated organizations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	query := &generated.ListOrganizationsQuery{}
	if cmd.Flags().Changed("limit") {
		query.Limit = &organizationLimit
	}
	if cmd.Flags().Changed("offset") {
		query.Offset = &organizationOffset
	}
	if cmd.Flags().Changed("query") {
		query.Q = &organizationQuery
	}
	if cmd.Flags().Changed("include-retired") {
		query.IncludeRetired = &organizationIncludeRetired
	}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListOrganizationsResp, error) {
		return api.ListOrganizationsWithResponse(cmd.Context(), &generated.ListOrganizationsRequestOptions{Query: query})
	})
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return errors.New("organization list response was empty")
	}
	if organizationJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tNAME\tKIND\tDOMAIN\tSTATUS\tREVISION")
	for _, org := range resp.JSON200.Organizations {
		status := "active"
		if org.RetiredAt != nil {
			status = "retired"
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%d\n", org.ID, org.Name, org.Kind, cliString(org.PrimaryDomain), status, org.Revision)
	}
	return w.Flush()
}}

var organizationCreateCmd = &cobra.Command{Use: "create <name>", Short: "Create a curated organization", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	name := strings.TrimSpace(args[0])
	if name == "" {
		return usageErr(cmd, errors.New("name must not be empty"))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	body := generated.CreateOrganizationBody{Name: name}
	if cmd.Flags().Changed("kind") {
		kind := generated.OrganizationCreateBodyKind(organizationKind)
		body.Kind = &kind
	}
	if cmd.Flags().Changed("domain") {
		body.PrimaryDomain = &organizationDomain
	}
	if cmd.Flags().Changed("description") {
		body.Description = &organizationDescription
	}
	resp, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusCreated}, func(api *apiclient.Client) (*generated.CreateOrganizationResp, error) {
		return api.CreateOrganizationWithResponse(cmd.Context(), &generated.CreateOrganizationRequestOptions{Body: &body})
	})
	if err != nil {
		return err
	}
	return writeCLIOrganization(cmd, resp.JSON201)
}}

var organizationShowCmd = &cobra.Command{Use: "show <id>", Short: "Show an organization", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if organizationShowHistory {
		resp, getErr := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetOrganizationHistoryResp, error) {
			return api.GetOrganizationHistoryWithResponse(cmd.Context(), &generated.GetOrganizationHistoryRequestOptions{PathParams: &generated.GetOrganizationHistoryPath{ID: id}})
		})
		if getErr != nil {
			return getErr
		}
		if resp.JSON200 == nil {
			return errors.New("organization history response was empty")
		}
		return writeCLIOrganizationHistory(cmd, resp.JSON200)
	}
	resp, err := getCLIOrganization(cmd, client, id)
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return errors.New("organization response was empty")
	}
	return writeCLIOrganizationProfile(cmd, resp.JSON200)
}}

var organizationSetCmd = &cobra.Command{Use: "set <id>", Short: "Replace an organization's mutable fields", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	if !cmd.Flags().Changed("name") {
		return usageErr(cmd, errors.New("--name is required"))
	}
	id, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	if strings.TrimSpace(organizationName) == "" {
		return usageErr(cmd, errors.New("name must not be empty"))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIOrganization(cmd, client, id)
	if err != nil {
		return err
	}
	if current.JSON200 == nil {
		return errors.New("organization response was empty")
	}
	body := generated.PatchOrganizationBody{Name: organizationName}
	if cmd.Flags().Changed("kind") {
		kind := generated.OrganizationBodyKind(organizationKind)
		body.Kind = &kind
	} else {
		kind := generated.OrganizationBodyKind(current.JSON200.Organization.Kind)
		body.Kind = &kind
	}
	if cmd.Flags().Changed("domain") {
		body.PrimaryDomain = &organizationDomain
	} else {
		body.PrimaryDomain = current.JSON200.Organization.PrimaryDomain
	}
	if cmd.Flags().Changed("description") {
		body.Description = &organizationDescription
	} else {
		body.Description = current.JSON200.Organization.Description
	}
	retired := current.JSON200.Organization.RetiredAt != nil
	body.Retired = &retired
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PatchOrganizationResp, error) {
		return api.PatchOrganizationWithResponse(cmd.Context(), &generated.PatchOrganizationRequestOptions{PathParams: &generated.PatchOrganizationPath{ID: id}, Header: &generated.PatchOrganizationHeaders{IfMatch: organizationETag(id, current.JSON200.Organization.Revision)}, Body: &body})
	})
	if err != nil {
		return err
	}
	return writeCLIOrganization(cmd, resp.JSON200)
}}

func runOrganizationRetired(retired bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "organization")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIOrganization(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("organization response was empty")
		}
		kind := generated.OrganizationBodyKind(current.JSON200.Organization.Kind)
		body := generated.PatchOrganizationBody{
			Name: current.JSON200.Organization.Name, Kind: &kind,
			PrimaryDomain: current.JSON200.Organization.PrimaryDomain,
			Description:   current.JSON200.Organization.Description,
			Retired:       &retired,
		}
		resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PatchOrganizationResp, error) {
			return api.PatchOrganizationWithResponse(cmd.Context(), &generated.PatchOrganizationRequestOptions{PathParams: &generated.PatchOrganizationPath{ID: id}, Header: &generated.PatchOrganizationHeaders{IfMatch: organizationETag(id, current.JSON200.Organization.Revision)}, Body: &body})
		})
		if err != nil {
			return err
		}
		return writeCLIOrganization(cmd, resp.JSON200)
	}
}

var organizationRetireCmd = &cobra.Command{Use: "retire <id>", Short: "Retire an organization", Args: cobra.ExactArgs(1), RunE: runOrganizationRetired(true)}
var organizationUnretireCmd = &cobra.Command{Use: "unretire <id>", Short: "Unretire an organization", Args: cobra.ExactArgs(1), RunE: runOrganizationRetired(false)}

var organizationDeleteCmd = &cobra.Command{Use: "delete <id>", Short: "Permanently delete an organization", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIOrganization(cmd, client, id)
	if err != nil {
		return err
	}
	if current.JSON200 == nil {
		return errors.New("organization response was empty")
	}
	_, err = daemonclient.APIResponseWithStatuses(client, []int{http.StatusNoContent}, func(api *apiclient.Client) (*generated.DeleteOrganizationResp, error) {
		return api.DeleteOrganizationWithResponse(cmd.Context(), &generated.DeleteOrganizationRequestOptions{PathParams: &generated.DeleteOrganizationPath{ID: id}, Header: &generated.DeleteOrganizationHeaders{IfMatch: organizationETag(id, current.JSON200.Organization.Revision)}})
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted organization %d\n", id)
	return nil
}}

var organizationMergeCmd = &cobra.Command{Use: "merge <survivor-id> <losing-id>", Short: "Merge a losing organization into a survivor", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	survivor, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	losing, err := positivePersonCLIArg(cmd, args[1], "organization")
	if err != nil {
		return err
	}
	if survivor == losing {
		return usageErr(cmd, errors.New("survivor and losing organization must differ"))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIOrganization(cmd, client, survivor)
	if err != nil {
		return err
	}
	loser, err := getCLIOrganization(cmd, client, losing)
	if err != nil {
		return err
	}
	if current.JSON200 == nil || loser.JSON200 == nil {
		return errors.New("organization response was empty")
	}
	body := generated.MergeOrganizationBody{LosingOrganizationID: losing, LosingRevision: loser.JSON200.Organization.Revision}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.MergeOrganizationResp, error) {
		return api.MergeOrganizationWithResponse(cmd.Context(), &generated.MergeOrganizationRequestOptions{PathParams: &generated.MergeOrganizationPath{ID: survivor}, Header: &generated.MergeOrganizationHeaders{IfMatch: organizationETag(survivor, current.JSON200.Organization.Revision)}, Body: &body})
	})
	if err != nil {
		return err
	}
	return writeCLIOrganization(cmd, resp.JSON200)
}}

var organizationAttributeCmd = &cobra.Command{Use: "attribute", Short: "Inspect and set organization attributes"}
var organizationAttributeListCmd = &cobra.Command{Use: "list <id>", Short: "List organization attribute values", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	query := &generated.ListOrganizationAttributesQuery{}
	if cmd.Flags().Changed("include-superseded") {
		query.IncludeSuperseded = &organizationIncludeSuperseded
	}
	if cmd.Flags().Changed("definition") {
		query.DefinitionSlug = &organizationDefinitionSlugValue
	}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListOrganizationAttributesResp, error) {
		return api.ListOrganizationAttributesWithResponse(cmd.Context(), &generated.ListOrganizationAttributesRequestOptions{PathParams: &generated.ListOrganizationAttributesPath{ID: id}, Query: query})
	})
	if err != nil {
		return err
	}
	if resp.JSON200 == nil {
		return errors.New("organization attributes response was empty")
	}
	if organizationJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SLUG\tORDINAL\tVALUE\tSOURCE\tACTIVE FROM\tACTIVE UNTIL")
	for _, value := range resp.JSON200.Values {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n", value.DefinitionSlug, value.Ordinal, formatCLIAttributeValue(value.Value), value.Source, value.ActiveFrom.Format("2006-01-02T15:04:05Z07:00"), formatCLIOptionalTime(value.ActiveUntil))
	}
	return w.Flush()
}}
var organizationAttributeSetCmd = &cobra.Command{Use: "set <id>", Short: "Set a text organization attribute", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	if !cmd.Flags().Changed("definition") {
		return usageErr(cmd, errors.New("--definition is required"))
	}
	if !cmd.Flags().Changed("text") {
		return usageErr(cmd, errors.New("--text is required"))
	}
	source := strings.TrimSpace(organizationSource)
	if source == "" {
		source = "user"
	}
	typedSource := generated.SetOrganizationAttributeBodySource(source)
	value := generated.AttributeValue{Type: "text", Text: &organizationTextValue}
	body := generated.SetOrganizationAttributeBody{DefinitionSlug: organizationDefinitionSlugValue, Source: typedSource, Value: value}
	if cmd.Flags().Changed("ordinal") {
		if organizationAttributeOrdinal < 0 {
			return usageErr(cmd, errors.New("--ordinal must be a non-negative integer"))
		}
		body.Ordinal = &organizationAttributeOrdinal
	}
	if cmd.Flags().Changed("expected-value-id") {
		if organizationExpectedValueID <= 0 {
			return usageErr(cmd, errors.New("--expected-value-id must be a positive integer"))
		}
		body.ExpectedValueID = &organizationExpectedValueID
	}
	if organizationDryRun {
		body.DryRun = &organizationDryRun
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	resp, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusOK, http.StatusCreated}, func(api *apiclient.Client) (*generated.SetOrganizationAttributeResp, error) {
		return api.SetOrganizationAttributeWithResponse(cmd.Context(), &generated.SetOrganizationAttributeRequestOptions{PathParams: &generated.SetOrganizationAttributePath{ID: id}, Body: &body})
	})
	if err != nil {
		return err
	}
	write := resp.JSON200
	if write == nil {
		write = resp.JSON201
	}
	if organizationJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(write)
	}
	return writeCLIOrganizationAttribute(cmd, write)
}}

var organizationAttributeClearCmd = &cobra.Command{Use: "clear <id> <slug>", Short: "Supersede an organization attribute value", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "organization")
	if err != nil {
		return err
	}
	slug := strings.TrimSpace(args[1])
	if slug == "" {
		return usageErr(cmd, errors.New("attribute slug must not be empty"))
	}
	if cmd.Flags().Changed("expected-value-id") && organizationExpectedValueID <= 0 {
		return usageErr(cmd, errors.New("--expected-value-id must be a positive integer"))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	query := &generated.ClearOrganizationAttributeQuery{}
	if cmd.Flags().Changed("ordinal") {
		if organizationAttributeOrdinal < 0 {
			return usageErr(cmd, errors.New("--ordinal must be a non-negative integer"))
		}
		query.Ordinal = &organizationAttributeOrdinal
	}
	if cmd.Flags().Changed("expected-value-id") {
		query.ExpectedValueID = &organizationExpectedValueID
	}
	if organizationDryRun {
		dryRun := true
		query.DryRun = &dryRun
	}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ClearOrganizationAttributeResp, error) {
		return api.ClearOrganizationAttributeWithResponse(cmd.Context(), &generated.ClearOrganizationAttributeRequestOptions{
			PathParams: &generated.ClearOrganizationAttributePath{ID: id, Slug: slug}, Query: query,
		})
	})
	if err != nil {
		return err
	}
	return writeCLIOrganizationAttribute(cmd, resp.JSON200)
}}

func getCLIOrganization(cmd *cobra.Command, client *daemonclient.Client, id int64) (*generated.GetOrganizationResp, error) {
	return daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetOrganizationResp, error) {
		return api.GetOrganizationWithResponse(cmd.Context(), &generated.GetOrganizationRequestOptions{PathParams: &generated.GetOrganizationPath{ID: id}})
	})
}
func organizationETag(id, revision int64) string {
	return fmt.Sprintf(`"organization-%d-r%d"`, id, revision)
}
func cliString(v *string) string {
	if v == nil || *v == "" {
		return "-"
	}
	return *v
}
func writeCLIOrganization(cmd *cobra.Command, org *generated.Organization) error {
	if org == nil {
		return errors.New("organization response was empty")
	}
	if organizationJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(org)
	}
	status := "active"
	if org.RetiredAt != nil {
		status = "retired"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Organization: %d\nName: %s\nKind: %s\nPrimary domain: %s\nDescription: %s\nStatus: %s\nRevision: %d\n", org.ID, org.Name, org.Kind, cliString(org.PrimaryDomain), cliString(org.Description), status, org.Revision)
	return nil
}
func writeCLIOrganizationProfile(cmd *cobra.Command, profile *generated.OrganizationProfile) error {
	if profile == nil {
		return errors.New("organization response was empty")
	}
	if organizationJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(profile)
	}
	if err := writeCLIOrganization(cmd, &profile.Organization); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Names: %d\nIdentifiers: %d\nAddresses: %d\nContact points: %d\nMedia: %d\nCategories: %d\n", len(profile.Names), len(profile.Identifiers), len(profile.Addresses), len(profile.ContactPoints), len(profile.Media), len(profile.Categories))
	return nil
}

func writeCLIOrganizationHistory(cmd *cobra.Command, profile *generated.OrganizationProfile) error {
	if err := writeCLIOrganizationProfile(cmd, profile); err != nil || organizationJSON {
		return err
	}
	for _, name := range profile.Names {
		if name.Envelope.ActiveUntil != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Name: %s (active until %s)\n", name.Name, formatCLIOptionalTime(name.Envelope.ActiveUntil))
		}
	}
	for _, identifier := range profile.Identifiers {
		if identifier.Envelope.ActiveUntil != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Identifier: %s (active until %s)\n", identifier.IdentifierValue, formatCLIOptionalTime(identifier.Envelope.ActiveUntil))
		}
	}
	for _, address := range profile.Addresses {
		if address.Envelope.ActiveUntil != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Address: %s (active until %s)\n", address.OriginalValue, formatCLIOptionalTime(address.Envelope.ActiveUntil))
		}
	}
	for _, point := range profile.ContactPoints {
		if point.Envelope.ActiveUntil != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Contact point: %s (active until %s)\n", point.OriginalValue, formatCLIOptionalTime(point.Envelope.ActiveUntil))
		}
	}
	for _, medium := range profile.Media {
		if medium.Envelope.ActiveUntil != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Media: %s (active until %s)\n", medium.OriginalValue, formatCLIOptionalTime(medium.Envelope.ActiveUntil))
		}
	}
	for _, category := range profile.Categories {
		if category.Envelope.ActiveUntil != nil {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Category: %s (active until %s)\n", category.Category, formatCLIOptionalTime(category.Envelope.ActiveUntil))
		}
	}
	return nil
}
func writeCLIOrganizationAttribute(cmd *cobra.Command, write *generated.OrganizationAttributeWrite) error {
	if write == nil {
		return errors.New("organization attribute response was empty")
	}
	prefix := ""
	if write.DryRun {
		prefix = "Dry run: "
	}
	if write.Superseded != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%sSuperseded %s ordinal %d: %s (active until %s)\n",
			prefix, write.Superseded.DefinitionSlug, write.Superseded.Ordinal,
			formatCLIAttributeValue(write.Superseded.Value), formatCLIOptionalTime(write.Superseded.ActiveUntil))
	}
	if write.Value != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%sSet %s: %s\n",
			prefix, write.Value.DefinitionSlug, formatCLIAttributeValue(write.Value.Value))
	}
	return nil
}

func init() {
	rootCmd.AddCommand(organizationCmd)
	organizationCmd.AddCommand(organizationListCmd, organizationCreateCmd, organizationShowCmd, organizationSetCmd, organizationRetireCmd, organizationUnretireCmd, organizationDeleteCmd, organizationMergeCmd, organizationAttributeCmd)
	organizationAttributeCmd.AddCommand(organizationAttributeListCmd, organizationAttributeSetCmd, organizationAttributeClearCmd)
	for _, command := range []*cobra.Command{organizationListCmd, organizationCreateCmd, organizationShowCmd, organizationSetCmd, organizationRetireCmd, organizationUnretireCmd, organizationMergeCmd, organizationAttributeListCmd, organizationAttributeSetCmd, organizationAttributeClearCmd} {
		command.Flags().BoolVar(&organizationJSON, flagJSON, false, "Output as JSON")
	}
	organizationListCmd.Flags().Int64Var(&organizationLimit, "limit", 0, "Maximum results")
	organizationListCmd.Flags().Int64Var(&organizationOffset, "offset", 0, "Results to skip")
	organizationListCmd.Flags().StringVar(&organizationQuery, "query", "", "Normalized-name search")
	organizationListCmd.Flags().BoolVar(&organizationIncludeRetired, "include-retired", false, "Include retired organizations")
	for _, command := range []*cobra.Command{organizationCreateCmd, organizationSetCmd} {
		command.Flags().StringVar(&organizationKind, "kind", "", "Organization kind")
		command.Flags().StringVar(&organizationDomain, "domain", "", "Primary domain")
		command.Flags().StringVar(&organizationDescription, "description", "", "Description")
	}
	organizationSetCmd.Flags().StringVar(&organizationName, "name", "", "Organization name")
	organizationShowCmd.Flags().BoolVar(&organizationShowHistory, "history", false, "Include superseded profile rows")
	organizationAttributeListCmd.Flags().BoolVar(&organizationIncludeSuperseded, "include-superseded", false, "Include superseded values")
	organizationAttributeListCmd.Flags().StringVar(&organizationDefinitionSlugValue, "definition", "", "Restrict to definition slug")
	organizationAttributeSetCmd.Flags().StringVar(&organizationDefinitionSlugValue, "definition", "", "Definition slug")
	organizationAttributeSetCmd.Flags().StringVar(&organizationTextValue, "text", "", "Text value")
	organizationAttributeSetCmd.Flags().StringVar(&organizationSource, "source", "", "Value source")
	organizationAttributeSetCmd.Flags().Int64Var(
		&organizationExpectedValueID, "expected-value-id", 0,
		"Expected current value ID for compare-and-swap")
	organizationAttributeSetCmd.Flags().BoolVar(&organizationDryRun, "dry-run", false, "Validate without writing")
	organizationAttributeSetCmd.Flags().Int64Var(
		&organizationAttributeOrdinal, "ordinal", 0,
		"Ordinal for a multi-valued definition")
	organizationAttributeClearCmd.Flags().Int64Var(&organizationAttributeOrdinal, "ordinal", 0, "Ordinal for a multi-valued definition")
	organizationAttributeClearCmd.Flags().Int64Var(
		&organizationExpectedValueID, "expected-value-id", 0,
		"Expected current value ID for compare-and-swap")
	organizationAttributeClearCmd.Flags().BoolVar(&organizationDryRun, "dry-run", false, "Validate without writing")
}
