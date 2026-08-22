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
	employmentJSON                                                                                   bool
	employmentPersonID, employmentOrganizationID                                                     int64
	employmentLimit, employmentOffset                                                                int64
	employmentTitle, employmentRole, employmentDepartment, employmentLocation, employmentDescription string
	employmentStartDate, employmentEndDate, employmentSource                                         string
	employmentPrimary, employmentNoPrimary, employmentNotCurrent, employmentCurrentOnly              bool
	employmentMarkCurrent, employmentClearStart, employmentClearEnd                                  bool
)

var employmentCmd = &cobra.Command{Use: "employment", Short: "Manage temporal employment records between people and organizations"}

var employmentAddCmd = &cobra.Command{Use: "add", Short: "Add an employment record", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
	body, err := employmentBodyFromFlags(cmd)
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	resp, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusCreated}, func(api *apiclient.Client) (*generated.CreateEmploymentResp, error) {
		return api.CreateEmploymentWithResponse(cmd.Context(), &generated.CreateEmploymentRequestOptions{Body: &body})
	})
	if err != nil {
		return err
	}
	return writeCLIEmployment(cmd, resp.JSON201)
}}

var employmentShowCmd = &cobra.Command{Use: "show <id>", Short: "Show an employment record", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "employment")
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	resp, err := getCLIEmployment(cmd, client, id)
	if err != nil {
		return err
	}
	return writeCLIEmployment(cmd, resp.JSON200)
}}

var employmentSetCmd = &cobra.Command{Use: "set <id>", Short: "Update an employment record's mutable fields", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "employment")
	if err != nil {
		return err
	}
	if employmentPrimary && employmentNoPrimary {
		return usageErr(cmd, errors.New("--primary and --no-primary are mutually exclusive"))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIEmployment(cmd, client, id)
	if err != nil {
		return err
	}
	if current.JSON200 == nil {
		return errors.New("employment response was empty")
	}
	body, err := employmentSetBody(cmd, current.JSON200)
	if err != nil {
		return err
	}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.PatchEmploymentResp, error) {
		return api.PatchEmploymentWithResponse(cmd.Context(), &generated.PatchEmploymentRequestOptions{PathParams: &generated.PatchEmploymentPath{ID: id}, Header: &generated.PatchEmploymentHeaders{IfMatch: employmentETag(id, current.JSON200.Revision)}, Body: &body})
	})
	if err != nil {
		return err
	}
	return writeCLIEmployment(cmd, resp.JSON200)
}}

var employmentEndCmd = &cobra.Command{Use: "end <id>", Short: "End an employment without deleting its history", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "employment")
	if err != nil {
		return err
	}
	if strings.TrimSpace(employmentEndDate) == "" {
		return usageErr(cmd, errors.New("--end is required"))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIEmployment(cmd, client, id)
	if err != nil {
		return err
	}
	if current.JSON200 == nil {
		return errors.New("employment response was empty")
	}
	body := generated.EndEmploymentBody{EndDate: employmentEndDate}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.EndEmploymentResp, error) {
		return api.EndEmploymentWithResponse(cmd.Context(), &generated.EndEmploymentRequestOptions{PathParams: &generated.EndEmploymentPath{ID: id}, Header: &generated.EndEmploymentHeaders{IfMatch: employmentETag(id, current.JSON200.Revision)}, Body: &body})
	})
	if err != nil {
		return err
	}
	return writeCLIEmployment(cmd, resp.JSON200)
}}

var employmentSetPrimaryCmd = &cobra.Command{Use: "set-primary <id>", Short: "Set the primary current employment", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "employment")
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIEmployment(cmd, client, id)
	if err != nil {
		return err
	}
	if current.JSON200 == nil {
		return errors.New("employment response was empty")
	}
	resp, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.SetPrimaryEmploymentResp, error) {
		return api.SetPrimaryEmploymentWithResponse(cmd.Context(), &generated.SetPrimaryEmploymentRequestOptions{PathParams: &generated.SetPrimaryEmploymentPath{ID: id}, Header: &generated.SetPrimaryEmploymentHeaders{IfMatch: employmentETag(id, current.JSON200.Revision)}})
	})
	if err != nil {
		return err
	}
	return writeCLIEmployment(cmd, resp.JSON200)
}}

var employmentDeleteCmd = &cobra.Command{Use: "delete <id>", Short: "Permanently delete an employment record", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
	id, err := positivePersonCLIArg(cmd, args[0], "employment")
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	current, err := getCLIEmployment(cmd, client, id)
	if err != nil {
		return err
	}
	if current.JSON200 == nil {
		return errors.New("employment response was empty")
	}
	_, err = daemonclient.APIResponseWithStatuses(client, []int{http.StatusNoContent}, func(api *apiclient.Client) (*generated.DeleteEmploymentResp, error) {
		return api.DeleteEmploymentWithResponse(cmd.Context(), &generated.DeleteEmploymentRequestOptions{PathParams: &generated.DeleteEmploymentPath{ID: id}, Header: &generated.DeleteEmploymentHeaders{IfMatch: employmentETag(id, current.JSON200.Revision)}})
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted employment %d\n", id)
	return nil
}}

var employmentListCmd = &cobra.Command{Use: cmdUseList, Short: "List employment records", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
	personSet, organizationSet := cmd.Flags().Changed("person"), cmd.Flags().Changed("organization")
	if !personSet && !organizationSet {
		return usageErr(cmd, errors.New("--person or --organization is required"))
	}
	if personSet && organizationSet {
		return usageErr(cmd, errors.New("--person and --organization are mutually exclusive"))
	}
	var id int64
	if personSet {
		id = employmentPersonID
	} else {
		id = employmentOrganizationID
	}
	if id <= 0 {
		kind := "organization"
		if personSet {
			kind = personValue
		}
		return usageErr(cmd, fmt.Errorf("%s ID must be a positive integer", kind))
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	currentOnly := employmentCurrentOnly
	var limit, offset *int64
	if cmd.Flags().Changed("limit") {
		limit = &employmentLimit
	}
	if cmd.Flags().Changed("offset") {
		offset = &employmentOffset
	}
	if personSet {
		resp, getErr := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonEmploymentsResp, error) {
			return api.ListPersonEmploymentsWithResponse(cmd.Context(), &generated.ListPersonEmploymentsRequestOptions{PathParams: &generated.ListPersonEmploymentsPath{ID: id}, Query: &generated.ListPersonEmploymentsQuery{CurrentOnly: &currentOnly, Limit: limit, Offset: offset}})
		})
		if getErr != nil {
			return getErr
		}
		if resp.JSON200 == nil {
			return errors.New("employment list response was empty")
		}
		return writeCLIEmploymentList(cmd, resp.JSON200, true, employmentJSON)
	}
	resp, getErr := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListOrganizationEmploymentsResp, error) {
		return api.ListOrganizationEmploymentsWithResponse(cmd.Context(), &generated.ListOrganizationEmploymentsRequestOptions{PathParams: &generated.ListOrganizationEmploymentsPath{ID: id}, Query: &generated.ListOrganizationEmploymentsQuery{CurrentOnly: &currentOnly, Limit: limit, Offset: offset}})
	})
	if getErr != nil {
		return getErr
	}
	if resp.JSON200 == nil {
		return errors.New("employment list response was empty")
	}
	return writeCLIEmploymentList(cmd, resp.JSON200, false, employmentJSON)
}}

func getCLIEmployment(cmd *cobra.Command, client *daemonclient.Client, id int64) (*generated.GetEmploymentResp, error) {
	return daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetEmploymentResp, error) {
		return api.GetEmploymentWithResponse(cmd.Context(), &generated.GetEmploymentRequestOptions{PathParams: &generated.GetEmploymentPath{ID: id}})
	})
}
func employmentETag(id, revision int64) string {
	return fmt.Sprintf(`"employment-%d-r%d"`, id, revision)
}

func employmentBodyFromFlags(cmd *cobra.Command) (generated.CreateEmploymentBody, error) {
	if employmentPrimary && employmentNoPrimary {
		return generated.CreateEmploymentBody{}, usageErr(cmd, errors.New("--primary and --no-primary are mutually exclusive"))
	}
	if employmentPersonID <= 0 {
		return generated.CreateEmploymentBody{}, usageErr(cmd, errors.New("--person is required and must be a positive integer"))
	}
	if employmentOrganizationID <= 0 {
		return generated.CreateEmploymentBody{}, usageErr(cmd, errors.New("--organization is required and must be a positive integer"))
	}
	source := strings.TrimSpace(employmentSource)
	if source == "" {
		source = "user"
	}
	body := generated.CreateEmploymentBody{PersonID: employmentPersonID, OrganizationID: employmentOrganizationID, Source: generated.EmploymentBodySource(source)}
	if cmd.Flags().Changed("title") {
		body.Title = &employmentTitle
	}
	if cmd.Flags().Changed("role") {
		body.Role = &employmentRole
	}
	if cmd.Flags().Changed("department") {
		body.Department = &employmentDepartment
	}
	if cmd.Flags().Changed("location") {
		body.Location = &employmentLocation
	}
	if cmd.Flags().Changed("description") {
		body.Description = &employmentDescription
	}
	if cmd.Flags().Changed("start") {
		body.StartDate = &employmentStartDate
	}
	if cmd.Flags().Changed("end") {
		body.EndDate = &employmentEndDate
	}
	if employmentPrimary {
		value := true
		body.IsPrimary = &value
	}
	if employmentNoPrimary {
		value := false
		body.IsPrimary = &value
	}
	if employmentNotCurrent {
		value := false
		body.IsCurrent = &value
	}
	return body, nil
}

// employmentSetBody merges changed flags over the fetched record so the
// full-replace PATCH never wipes fields the user did not mention.
func employmentSetBody(cmd *cobra.Command, current *generated.Employment) (generated.CreateEmploymentBody, error) {
	body := generated.CreateEmploymentBody{
		PersonID:       current.PersonID,
		OrganizationID: current.OrganizationID,
		Source:         generated.EmploymentBodySource(current.Source),
		SourceRef:      current.SourceRef,
		Confidence:     current.Confidence,
		AddressID:      current.AddressID,
		Title:          current.Title,
		Role:           current.Role,
		Department:     current.Department,
		Location:       current.Location,
		Description:    current.Description,
	}
	if cmd.Flags().Changed("person") {
		if employmentPersonID <= 0 {
			return body, usageErr(cmd, errors.New("--person must be a positive integer"))
		}
		body.PersonID = employmentPersonID
	}
	if cmd.Flags().Changed("organization") {
		if employmentOrganizationID <= 0 {
			return body, usageErr(cmd, errors.New("--organization must be a positive integer"))
		}
		body.OrganizationID = employmentOrganizationID
		if body.OrganizationID != current.OrganizationID {
			// The stored address belongs to the previous organization.
			body.AddressID = nil
		}
	}
	if cmd.Flags().Changed("source") {
		body.Source = generated.EmploymentBodySource(strings.TrimSpace(employmentSource))
		// The stored reference and confidence describe the previous provenance.
		body.SourceRef = nil
		body.Confidence = nil
	}
	if cmd.Flags().Changed("title") {
		body.Title = &employmentTitle
	}
	if cmd.Flags().Changed("role") {
		body.Role = &employmentRole
	}
	if cmd.Flags().Changed("department") {
		body.Department = &employmentDepartment
	}
	if cmd.Flags().Changed("location") {
		body.Location = &employmentLocation
	}
	if cmd.Flags().Changed("description") {
		body.Description = &employmentDescription
	}
	switch {
	case employmentClearStart:
		if cmd.Flags().Changed("start") {
			return body, usageErr(cmd, errors.New("--start and --clear-start are mutually exclusive"))
		}
	case cmd.Flags().Changed("start"):
		if strings.TrimSpace(employmentStartDate) == "" {
			return body, usageErr(cmd, errors.New("--start must not be empty; use --clear-start to remove the start date"))
		}
		body.StartDate = &employmentStartDate
	default:
		if date, ok := cliPartialDateString(current.StartDate); ok {
			body.StartDate = &date
		}
	}
	switch {
	case employmentClearEnd:
		if cmd.Flags().Changed("end") {
			return body, usageErr(cmd, errors.New("--end and --clear-end are mutually exclusive"))
		}
	case cmd.Flags().Changed("end"):
		if strings.TrimSpace(employmentEndDate) == "" {
			return body, usageErr(cmd, errors.New("--end must not be empty; use --clear-end to remove the end date"))
		}
		body.EndDate = &employmentEndDate
	default:
		if date, ok := cliPartialDateString(current.EndDate); ok {
			body.EndDate = &date
		}
	}
	if employmentMarkCurrent && employmentNotCurrent {
		return body, usageErr(cmd, errors.New("--current and --not-current are mutually exclusive"))
	}
	isCurrent := current.IsCurrent
	if employmentNotCurrent {
		isCurrent = false
	}
	if cmd.Flags().Changed("end") {
		isCurrent = false
	}
	if employmentMarkCurrent {
		if body.EndDate != nil {
			return body, usageErr(cmd, errors.New("--current requires the employment to have no end date; combine it with --clear-end"))
		}
		isCurrent = true
	}
	body.IsCurrent = &isCurrent
	isPrimary := current.IsPrimary
	if employmentPrimary {
		if !isCurrent {
			return body, usageErr(cmd,
				errors.New("--primary cannot be combined with --not-current or --end: a historical employment cannot be primary"))
		}
		isPrimary = true
	}
	if employmentNoPrimary {
		isPrimary = false
	}
	if !isCurrent {
		// The store rejects historical primaries; ending an employment
		// demotes it, matching the server-side end operation.
		isPrimary = false
	}
	body.IsPrimary = &isPrimary
	return body, nil
}

// cliPartialDateString renders a partial date for request bodies; the second
// return is false when there is no date to send.
func cliPartialDateString(value *generated.PartialDate) (string, bool) {
	if value == nil || value.Year == nil {
		return "", false
	}
	return formatCLIPartialDate(value), true
}

func writeCLIEmployment(cmd *cobra.Command, employment *generated.Employment) error {
	if employment == nil {
		return errors.New("employment response was empty")
	}
	if employmentJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(employment)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Employment: %d\nPerson: %d\nOrganization: %d\nTitle: %s\nRole: %s\nDepartment: %s\nStart date: %s\nEnd date: %s\nCurrent: %t\nPrimary: %t\nSource: %s\nRevision: %d\n", employment.ID, employment.PersonID, employment.OrganizationID, cliString(employment.Title), cliString(employment.Role), cliString(employment.Department), formatCLIPartialDate(employment.StartDate), formatCLIPartialDate(employment.EndDate), employment.IsCurrent, employment.IsPrimary, employment.Source, employment.Revision)
	return nil
}
func writeCLIEmploymentList(
	cmd *cobra.Command, response *generated.EmploymentsResponse,
	personScoped, jsonOutput bool,
) error {
	if response == nil {
		return errors.New("employment list response was empty")
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(response)
	}
	// A person-scoped listing distinguishes rows by employer; an
	// organization-scoped listing distinguishes them by employee.
	counterpartHeader := "ORGANIZATION"
	counterpartID := func(employment generated.Employment) int64 { return employment.OrganizationID }
	if !personScoped {
		counterpartHeader = "PERSON"
		counterpartID = func(employment generated.Employment) int64 { return employment.PersonID }
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\t"+counterpartHeader+"\tTITLE\tSTART\tEND\tCURRENT\tPRIMARY")
	for _, employment := range response.Employments {
		_, _ = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%t\t%t\n", employment.ID, counterpartID(employment), cliString(employment.Title), formatCLIPartialDate(employment.StartDate), formatCLIPartialDate(employment.EndDate), employment.IsCurrent, employment.IsPrimary)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush employment table: %w", err)
	}
	if personScoped && response.Projection != nil {
		projection := response.Projection
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Current company: %s\nCurrent title: %s\n", projection.OrganizationName, cliString(projection.Title))
		if projection.Role != nil && *projection.Role != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Current role: %s\n", *projection.Role)
		}
		if len(projection.Vcard.Org) > 0 {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "vCard ORG: %s\n", strings.Join(projection.Vcard.Org, ";"))
		}
	}
	return nil
}

func init() {
	rootCmd.AddCommand(employmentCmd)
	employmentCmd.AddCommand(employmentAddCmd, employmentShowCmd, employmentSetCmd, employmentEndCmd, employmentSetPrimaryCmd, employmentDeleteCmd, employmentListCmd)
	for _, command := range []*cobra.Command{employmentAddCmd, employmentShowCmd, employmentSetCmd, employmentEndCmd, employmentSetPrimaryCmd, employmentListCmd} {
		command.Flags().BoolVar(&employmentJSON, flagJSON, false, "Output as JSON")
	}
	employmentSetCmd.Flags().BoolVar(&employmentMarkCurrent, "current", false, "Mark the employment current again")
	employmentSetCmd.Flags().BoolVar(&employmentClearStart, "clear-start", false, "Remove the start date")
	employmentSetCmd.Flags().BoolVar(&employmentClearEnd, "clear-end", false, "Remove the end date")
	for _, command := range []*cobra.Command{employmentAddCmd, employmentSetCmd} {
		command.Flags().Int64Var(&employmentPersonID, "person", 0, "Person ID")
		command.Flags().Int64Var(&employmentOrganizationID, "organization", 0, "Organization ID")
		command.Flags().StringVar(&employmentTitle, "title", "", "Employment title")
		command.Flags().StringVar(&employmentRole, "role", "", "Employment role")
		command.Flags().StringVar(&employmentDepartment, "department", "", "Department")
		command.Flags().StringVar(&employmentLocation, "location", "", "Location")
		command.Flags().StringVar(&employmentDescription, "description", "", "Description")
		command.Flags().StringVar(&employmentStartDate, "start", "", "Partial start date")
		command.Flags().StringVar(&employmentEndDate, "end", "", "Partial end date")
		command.Flags().StringVar(&employmentSource, "source", "", "Value source")
		command.Flags().BoolVar(&employmentPrimary, "primary", false, "Make primary")
		command.Flags().BoolVar(&employmentNoPrimary, "no-primary", false, "Do not make primary")
		command.Flags().BoolVar(&employmentNotCurrent, "not-current", false, "Mark not current")
	}
	employmentEndCmd.Flags().StringVar(&employmentEndDate, "end", "", "Partial end date")
	employmentListCmd.Flags().Int64Var(&employmentPersonID, "person", 0, "Person ID")
	employmentListCmd.Flags().Int64Var(&employmentOrganizationID, "organization", 0, "Organization ID")
	employmentListCmd.Flags().BoolVar(&employmentCurrentOnly, "current-only", false, "Only current employments")
	employmentListCmd.Flags().Int64Var(&employmentLimit, "limit", 0, "Maximum results")
	employmentListCmd.Flags().Int64Var(&employmentOffset, "offset", 0, "Results to skip")
}
