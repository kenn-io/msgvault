package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var (
	relationshipStartDate              string
	relationshipEndDate                string
	relationshipNotes                  string
	relationshipIncludeEnded           bool
	relationshipTypeSymmetric          bool
	relationshipTypeColor              string
	relationshipTypeIcon               string
	relationshipTypeUpdateForwardLabel string
	relationshipTypeUpdateReverseLabel string
	relationshipTypeUpdateRelatedType  string
	relationshipTypeUpdateColor        string
	relationshipTypeUpdateIcon         string
	relationshipTypeUpdateDescription  string
	relationshipReviewStatus           string
)

var relationshipTypeCmd = &cobra.Command{
	Use:   "relationship-type",
	Short: "Manage person relationship types",
	Long: "Manage person relationship types.\n\n" +
		"A type carries a forward and a reverse label so one stored relationship renders\n" +
		"correctly from both people's profiles; Msgvault never stores a mirrored second row.\n" +
		"The forward label completes the sentence \"<source> is the ___ of <target>\".",
}

var relationshipTypeListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List person relationship types",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ListRelationshipTypesResp, error) {
				return api.ListRelationshipTypesWithResponse(cmd.Context())
			})
		if err != nil {
			return err
		}
		if resp.JSON200 == nil {
			return errors.New("relationship types response was empty")
		}
		if personJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.RelationshipTypes)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tSLUG\tFORWARD\tREVERSE\tSYMMETRIC\tVCARD TYPE\tOWNER\tREVISION")
		for _, relationshipType := range resp.JSON200.RelationshipTypes {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%t\t%s\t%s\t%d\n",
				relationshipType.ID, relationshipType.Slug, relationshipType.ForwardLabel,
				relationshipType.ReverseLabel, relationshipType.IsSymmetric,
				optionalCLIString(relationshipType.VcardRelatedType), relationshipType.Ownership,
				relationshipType.Revision)
		}
		return w.Flush()
	},
}

var relationshipTypeCreateCmd = &cobra.Command{
	Use:   "create <slug> <forward-label> <reverse-label>",
	Short: "Create a user-owned relationship type",
	Long: "Create a user-owned relationship type.\n\n" +
		"The forward label completes \"<source> is the ___ of <target>\"; the reverse label\n" +
		"completes the same sentence for the other person. For --symmetric types both labels\n" +
		"must be identical.",
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		slug, forward, reverse := strings.TrimSpace(args[0]), strings.TrimSpace(args[1]), strings.TrimSpace(args[2])
		if slug == "" || forward == "" || reverse == "" {
			return usageErr(cmd, errors.New("slug, forward label, and reverse label are required"))
		}
		if relationshipTypeSymmetric && forward != reverse {
			return usageErr(cmd, errors.New("a symmetric type requires identical forward and reverse labels"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.CreateRelationshipTypeBody{
			Slug: slug, ForwardLabel: forward, ReverseLabel: reverse, IsSymmetric: &relationshipTypeSymmetric,
			Color: optionalCLIFlag(relationshipTypeColor), Icon: optionalCLIFlag(relationshipTypeIcon),
		}
		resp, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusCreated},
			func(api *apiclient.Client) (*generated.CreateRelationshipTypeResp, error) {
				return api.CreateRelationshipTypeWithResponse(cmd.Context(),
					&generated.CreateRelationshipTypeRequestOptions{Body: &body})
			})
		if err != nil {
			return err
		}
		return writeCLIRelationshipType(cmd, resp.JSON201)
	},
}

var relationshipTypeUpdateCmd = &cobra.Command{
	Use:   "update <type-id>",
	Short: "Update a relationship type's labels or presentation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "relationship type")
		if err != nil {
			return err
		}
		body, err := relationshipTypeUpdateBody(cmd)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIRelationshipType(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("relationship type response was empty")
		}
		etag := fmt.Sprintf(`"relationship-type-%d-r%d"`, id, current.JSON200.Revision)
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.PatchRelationshipTypeResp, error) {
				return api.PatchRelationshipTypeWithResponse(cmd.Context(),
					&generated.PatchRelationshipTypeRequestOptions{
						PathParams: &generated.PatchRelationshipTypePath{ID: id},
						Header:     &generated.PatchRelationshipTypeHeaders{IfMatch: etag},
						Body:       body,
					})
			})
		if err != nil {
			return err
		}
		return writeCLIRelationshipType(cmd, resp.JSON200)
	},
}

func relationshipTypeUpdateBody(cmd *cobra.Command) (*generated.PatchRelationshipTypeBody, error) {
	flags := cmd.Flags()
	body := &generated.PatchRelationshipTypeBody{}
	if flags.Changed("forward-label") {
		value := strings.TrimSpace(relationshipTypeUpdateForwardLabel)
		if value == "" {
			return nil, usageErr(cmd, errors.New("forward label must not be empty"))
		}
		body.ForwardLabel = &value
	}
	if flags.Changed("reverse-label") {
		value := strings.TrimSpace(relationshipTypeUpdateReverseLabel)
		if value == "" {
			return nil, usageErr(cmd, errors.New("reverse label must not be empty"))
		}
		body.ReverseLabel = &value
	}
	if flags.Changed("vcard-related-type") {
		value := strings.TrimSpace(relationshipTypeUpdateRelatedType)
		body.VcardRelatedType = &value
	}
	if flags.Changed("color") {
		value := strings.TrimSpace(relationshipTypeUpdateColor)
		body.Color = &value
	}
	if flags.Changed("icon") {
		value := strings.TrimSpace(relationshipTypeUpdateIcon)
		body.Icon = &value
	}
	if flags.Changed("description") {
		value := strings.TrimSpace(relationshipTypeUpdateDescription)
		body.Description = &value
	}
	if body.ForwardLabel == nil && body.ReverseLabel == nil && body.VcardRelatedType == nil &&
		body.Color == nil && body.Icon == nil && body.Description == nil {
		return nil, usageErr(cmd, errors.New("at least one update flag is required"))
	}
	return body, nil
}

var relationshipTypeDeleteCmd = &cobra.Command{
	Use:   "delete <type-id>",
	Short: "Delete an unused user-owned relationship type",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "relationship type")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIRelationshipType(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("relationship type response was empty")
		}
		etag := fmt.Sprintf(`"relationship-type-%d-r%d"`, id, current.JSON200.Revision)
		if _, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusNoContent},
			func(api *apiclient.Client) (*generated.DeleteRelationshipTypeResp, error) {
				return api.DeleteRelationshipTypeWithResponse(cmd.Context(),
					&generated.DeleteRelationshipTypeRequestOptions{
						PathParams: &generated.DeleteRelationshipTypePath{ID: id},
						Header:     &generated.DeleteRelationshipTypeHeaders{IfMatch: etag},
					})
			}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted relationship type %d\n", id)
		return nil
	},
}

var personRelationshipCmd = &cobra.Command{
	Use:   "relationship",
	Short: "Manage a person's typed relationships",
}

var personRelationshipListCmd = &cobra.Command{
	Use:   "list <person-id>",
	Short: "List one person's relationships from that person's side",
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
		query := &generated.ListPersonRelationshipsQuery{}
		if relationshipIncludeEnded {
			query.IncludeEnded = &relationshipIncludeEnded
		}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ListPersonRelationshipsResp, error) {
				return api.ListPersonRelationshipsWithResponse(cmd.Context(),
					&generated.ListPersonRelationshipsRequestOptions{
						PathParams: &generated.ListPersonRelationshipsPath{ID: personID}, Query: query,
					})
			})
		if err != nil {
			return err
		}
		if resp.JSON200 == nil {
			return errors.New("person relationships response was empty")
		}
		if personJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.Relationships)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tCOUNTERPART\tIS\tDIRECTION\tFROM\tUNTIL\tSTATUS")
		for _, view := range resp.JSON200.Relationships {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				view.Relationship.ID, formatCLIRelationshipCounterpart(view.CounterpartPersonID,
					view.CounterpartDisplayName, view.CounterpartVcardUID), view.CounterpartLabel,
				view.Direction, formatCLIPartialDate(view.Relationship.StartDate),
				formatCLIPartialDate(view.Relationship.EndDate), view.Relationship.Status)
		}
		return w.Flush()
	},
}

var personRelationshipAddCmd = &cobra.Command{
	Use:   "add <source-person-id> <type-slug> <target-person-id>",
	Short: "Declare a relationship between two persons",
	Long: "Declare a relationship between two persons.\n\n" +
		"Arguments read as a sentence: \"<source> is the <type> of <target>\". Exactly one row\n" +
		"is stored; the type's reverse label presents the same row from the other person's side.",
	Args: cobra.ExactArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		sourceID, err := positivePersonCLIArg(cmd, args[0], "source person")
		if err != nil {
			return err
		}
		typeSlug := strings.TrimSpace(args[1])
		if typeSlug == "" {
			return usageErr(cmd, errors.New("relationship type slug is required"))
		}
		targetID, err := positivePersonCLIArg(cmd, args[2], "target person")
		if err != nil {
			return err
		}
		if sourceID == targetID {
			return usageErr(cmd, errors.New("a person cannot have a relationship with itself"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.CreatePersonRelationshipBody{
			SourcePersonID: sourceID, TargetPersonID: targetID, RelationshipTypeSlug: typeSlug,
			StartDate: optionalCLIFlag(relationshipStartDate), EndDate: optionalCLIFlag(relationshipEndDate),
			Notes: optionalCLIFlag(relationshipNotes),
		}
		resp, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusCreated},
			func(api *apiclient.Client) (*generated.CreatePersonRelationshipResp, error) {
				return api.CreatePersonRelationshipWithResponse(cmd.Context(),
					&generated.CreatePersonRelationshipRequestOptions{Body: &body})
			})
		if err != nil {
			return err
		}
		return writeCLIPersonRelationship(cmd, resp.JSON201)
	},
}

var personRelationshipEndCmd = &cobra.Command{
	Use:   "end <relationship-id> <until-date>",
	Short: "Record that a relationship stopped being true",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "relationship")
		if err != nil {
			return err
		}
		until := strings.TrimSpace(args[1])
		if until == "" {
			return usageErr(cmd, errors.New("an end date is required"))
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIPersonRelationship(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("relationship response was empty")
		}
		etag := fmt.Sprintf(`"person-relationship-%d-r%d"`, id, current.JSON200.Revision)
		body := generated.PatchPersonRelationshipBody{EndDate: &until}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.PatchPersonRelationshipResp, error) {
				return api.PatchPersonRelationshipWithResponse(cmd.Context(),
					&generated.PatchPersonRelationshipRequestOptions{
						PathParams: &generated.PatchPersonRelationshipPath{ID: id},
						Header:     &generated.PatchPersonRelationshipHeaders{IfMatch: etag}, Body: &body,
					})
			})
		if err != nil {
			return err
		}
		return writeCLIPersonRelationship(cmd, resp.JSON200)
	},
}

var personRelationshipDeleteCmd = &cobra.Command{
	Use:   "delete <relationship-id>",
	Short: "Delete a relationship that should never have been recorded",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], "relationship")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIPersonRelationship(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("relationship response was empty")
		}
		etag := fmt.Sprintf(`"person-relationship-%d-r%d"`, id, current.JSON200.Revision)
		if _, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusNoContent},
			func(api *apiclient.Client) (*generated.DeletePersonRelationshipResp, error) {
				return api.DeletePersonRelationshipWithResponse(cmd.Context(),
					&generated.DeletePersonRelationshipRequestOptions{
						PathParams: &generated.DeletePersonRelationshipPath{ID: id},
						Header:     &generated.DeletePersonRelationshipHeaders{IfMatch: etag},
					})
			}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted relationship %d\n", id)
		return nil
	},
}

var personRelationshipReviewsCmd = &cobra.Command{
	Use:   "reviews",
	Short: "List imported vCard RELATED values awaiting review",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		status := strings.TrimSpace(relationshipReviewStatus)
		query := &generated.ListPersonRelationshipReviewsQuery{}
		if status != "" {
			switch status {
			case "pending", "accepted", "rejected":
				value := generated.ListPersonRelationshipReviewsQueryStatus(status)
				query.Status = &value
			default:
				return usageErr(cmd, errors.New("status must be pending, accepted, or rejected"))
			}
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ListPersonRelationshipReviewsResp, error) {
				return api.ListPersonRelationshipReviewsWithResponse(cmd.Context(),
					&generated.ListPersonRelationshipReviewsRequestOptions{Query: query})
			})
		if err != nil {
			return err
		}
		if resp.JSON200 == nil {
			return errors.New("person relationship reviews response was empty")
		}
		if personJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.Reviews)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tPERSON\tRELATED VALUE\tTYPE\tKIND\tMATCHED\tSTATUS")
		for _, review := range resp.JSON200.Reviews {
			matched := "-"
			if review.MatchedPersonID != nil {
				matched = strconv.FormatInt(*review.MatchedPersonID, 10)
			}
			_, _ = fmt.Fprintf(w, "%d\t%d\t%s\t%s\t%s\t%s\t%s\n", review.ID, review.PersonID,
				review.RawRelatedValue, dashIfEmpty(review.RawRelatedType), review.ValueKind, matched, review.Status)
		}
		return w.Flush()
	},
}

func getCLIRelationshipType(
	cmd *cobra.Command, client *daemonclient.Client, id int64,
) (*generated.GetRelationshipTypeResp, error) {
	return daemonclient.APIResponse(client,
		func(api *apiclient.Client) (*generated.GetRelationshipTypeResp, error) {
			return api.GetRelationshipTypeWithResponse(cmd.Context(),
				&generated.GetRelationshipTypeRequestOptions{PathParams: &generated.GetRelationshipTypePath{ID: id}})
		})
}

func getCLIPersonRelationship(
	cmd *cobra.Command, client *daemonclient.Client, id int64,
) (*generated.GetPersonRelationshipResp, error) {
	return daemonclient.APIResponse(client,
		func(api *apiclient.Client) (*generated.GetPersonRelationshipResp, error) {
			return api.GetPersonRelationshipWithResponse(cmd.Context(),
				&generated.GetPersonRelationshipRequestOptions{PathParams: &generated.GetPersonRelationshipPath{ID: id}})
		})
}

func writeCLIRelationshipType(cmd *cobra.Command, relationshipType *generated.RelationshipType) error {
	if relationshipType == nil {
		return errors.New("relationship type response was empty")
	}
	if personJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(relationshipType)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Relationship type: %d\nSlug: %s\nForward: %s\nReverse: %s\nSymmetric: %t\nRevision: %d\n",
		relationshipType.ID, relationshipType.Slug, relationshipType.ForwardLabel,
		relationshipType.ReverseLabel, relationshipType.IsSymmetric, relationshipType.Revision)
	return nil
}

func writeCLIPersonRelationship(cmd *cobra.Command, edge *generated.PersonRelationship) error {
	if edge == nil {
		return errors.New("relationship response was empty")
	}
	if personJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(edge)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Relationship: %d\nPerson %d is the %s of person %d\nFrom: %s\nUntil: %s\nStatus: %s\nRevision: %d\n",
		edge.ID, edge.SourcePersonID, edge.ForwardLabel, edge.TargetPersonID,
		formatCLIPartialDate(edge.StartDate), formatCLIPartialDate(edge.EndDate), edge.Status, edge.Revision)
	return nil
}

func optionalCLIFlag(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func optionalCLIString(value *string) string {
	if value == nil || *value == "" {
		return "-"
	}
	return *value
}

func formatCLIPartialDate(value *generated.PartialDate) string {
	if value == nil || value.Year == nil {
		return "-"
	}
	if value.Month == nil {
		return fmt.Sprintf("%04d", *value.Year)
	}
	if value.Day == nil {
		return fmt.Sprintf("%04d-%02d", *value.Year, *value.Month)
	}
	return fmt.Sprintf("%04d-%02d-%02d", *value.Year, *value.Month, *value.Day)
}

func formatCLIRelationshipCounterpart(id int64, displayName *string, vcardUID string) string {
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		return fmt.Sprintf("%d (%s)", id, *displayName)
	}
	if strings.TrimSpace(vcardUID) != "" {
		return fmt.Sprintf("%d (%s)", id, vcardUID)
	}
	return strconv.FormatInt(id, 10)
}

func dashIfEmpty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func init() {
	rootCmd.AddCommand(relationshipTypeCmd)
	relationshipTypeCmd.AddCommand(relationshipTypeListCmd, relationshipTypeCreateCmd,
		relationshipTypeUpdateCmd, relationshipTypeDeleteCmd)
	personCmd.AddCommand(personRelationshipCmd)
	personRelationshipCmd.AddCommand(personRelationshipListCmd, personRelationshipAddCmd,
		personRelationshipEndCmd, personRelationshipDeleteCmd, personRelationshipReviewsCmd)

	for _, command := range []*cobra.Command{
		relationshipTypeListCmd, relationshipTypeCreateCmd, relationshipTypeUpdateCmd,
		personRelationshipListCmd, personRelationshipAddCmd, personRelationshipEndCmd,
		personRelationshipReviewsCmd,
	} {
		command.Flags().BoolVar(&personJSON, flagJSON, false, "Output as JSON")
	}
	relationshipTypeCreateCmd.Flags().BoolVar(&relationshipTypeSymmetric, "symmetric", false,
		"The relationship reads the same from both sides; both labels must be identical")
	relationshipTypeCreateCmd.Flags().StringVar(&relationshipTypeColor, "color", "", "Presentation colour")
	relationshipTypeCreateCmd.Flags().StringVar(&relationshipTypeIcon, "icon", "", "Presentation icon")
	relationshipTypeUpdateCmd.Flags().StringVar(&relationshipTypeUpdateForwardLabel, "forward-label", "", "Forward label")
	relationshipTypeUpdateCmd.Flags().StringVar(&relationshipTypeUpdateReverseLabel, "reverse-label", "", "Reverse label")
	relationshipTypeUpdateCmd.Flags().StringVar(&relationshipTypeUpdateRelatedType, "vcard-related-type", "", "vCard RELATED TYPE mapping")
	relationshipTypeUpdateCmd.Flags().StringVar(&relationshipTypeUpdateColor, "color", "", "Presentation colour")
	relationshipTypeUpdateCmd.Flags().StringVar(&relationshipTypeUpdateIcon, "icon", "", "Presentation icon")
	relationshipTypeUpdateCmd.Flags().StringVar(&relationshipTypeUpdateDescription, "description", "", "Description")
	personRelationshipAddCmd.Flags().StringVar(&relationshipStartDate, "from", "", "Partial start date: YYYY, YYYY-MM, or YYYY-MM-DD")
	personRelationshipAddCmd.Flags().StringVar(&relationshipEndDate, "until", "", "Partial end date for a relationship that has already ended")
	personRelationshipAddCmd.Flags().StringVar(&relationshipNotes, "notes", "", "Free-text notes")
	personRelationshipListCmd.Flags().BoolVar(&relationshipIncludeEnded, "include-ended", false,
		"Include relationships that have ended")
	personRelationshipReviewsCmd.Flags().StringVar(&relationshipReviewStatus, "status", "",
		"Filter by review status: pending, accepted, or rejected")
}
