package cmd

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personobs"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func observationCLIReference(cmd *cobra.Command, raw string) (int64, error) {
	raw = strings.TrimPrefix(raw, "observation:")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, usageErr(cmd, errors.New("observation must be a positive ID or observation:<id>"))
	}
	return id, nil
}
func observationCLITime(cmd *cobra.Command, name, raw string) (*time.Time, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // Omitted optional timestamps are represented as nil.
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, usageErr(cmd, fmt.Errorf("--%s must be an RFC3339 timestamp", name))
	}
	return &at, nil
}
func observationCLIDate(cmd *cobra.Command, raw string) (*generated.PartialDateValue, error) {
	if raw == "" {
		return nil, nil //nolint:nilnil // Omitted optional dates are represented as nil.
	}
	parts := strings.Split(raw, "-")
	if len(parts) > 3 || len(parts[0]) != 4 {
		return nil, usageErr(cmd, errors.New("--occurs-on must be YYYY, YYYY-MM or YYYY-MM-DD"))
	}
	values := []int64{}
	for i, p := range parts {
		if i > 0 && len(p) != 2 {
			return nil, usageErr(cmd, errors.New("--occurs-on must be YYYY, YYYY-MM or YYYY-MM-DD"))
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			return nil, usageErr(cmd, err)
		}
		values = append(values, n)
	}
	year := values[0]
	month, day := int64(1), int64(1)
	if len(values) > 1 {
		month = values[1]
	}
	if len(values) > 2 {
		day = values[2]
	}
	date := time.Date(int(year), time.Month(month), int(day), 0, 0, 0, 0, time.UTC)
	if year < 1 || year > 9999 || int64(date.Year()) != year || int64(date.Month()) != month || int64(date.Day()) != day {
		return nil, usageErr(cmd, errors.New("--occurs-on is not a valid date"))
	}
	result := &generated.PartialDateValue{Year: year}
	if len(values) > 1 {
		result.Month = &month
	}
	if len(values) > 2 {
		result.Day = &day
	}
	return result, nil
}
func observationCLIShow(cmd *cobra.Command, id int64, include bool) (*generated.GetPersonObservationResp, error) {
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.GetPersonObservationResp, error) {
		return api.GetPersonObservationWithResponse(cmd.Context(), &generated.GetPersonObservationRequestOptions{PathParams: &generated.GetPersonObservationPath{ID: id}, Query: &generated.GetPersonObservationQuery{IncludeSensitive: &include}})
	})
}
func newPersonObservationsCommand() *cobra.Command {
	var jsonOutput bool
	root := &cobra.Command{Use: "observations", Short: "Read and curate person observations"}
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Output JSON")
	var level, status, kind, asOf string
	var include bool
	var limit, offset int64
	list := &cobra.Command{Use: "list <person-id>", Short: "List a person's observations", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		if limit < 1 || limit > 500 || offset < 0 {
			return usageErr(cmd, errors.New("--limit must be 1–500 and --offset must not be negative"))
		}
		if _, err := observationCLITime(cmd, "as-of", asOf); err != nil {
			return err
		}
		for _, filter := range []struct {
			name, value string
			allowed     []string
		}{{"level", level, []string{"explicit", "deductive", "inductive", "contradiction"}}, {"status", status, []string{string(personobs.StatusActive), "unsupported", "superseded", "rejected"}}, {"kind", kind, []string{"state", "event", "plan", "preference", "relationship", "pattern", "other"}}} {
			if filter.value != "" {
				for value := range strings.SplitSeq(filter.value, ",") {
					found := false
					for _, allowed := range filter.allowed {
						if value == allowed {
							found = true
						}
					}
					if !found {
						return usageErr(cmd, fmt.Errorf("invalid --%s: %s", filter.name, value))
					}
				}
			}
		}
		q := &generated.ListPersonObservationsQuery{IncludeSensitive: &include, Limit: &limit, Offset: &offset}
		if level != "" {
			q.Level = &level
		}
		if status != "" {
			q.Status = &status
		}
		if kind != "" {
			q.Kind = &kind
		}
		if asOf != "" {
			q.AsOf = &asOf
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListPersonObservationsResp, error) {
			return api.ListPersonObservationsWithResponse(cmd.Context(), &generated.ListPersonObservationsRequestOptions{PathParams: &generated.ListPersonObservationsPath{ID: id}, Query: q})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("observations response was empty")
		}
		if jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "REFERENCE\tLEVEL\tSTATUS\tCONTENT")
		for _, o := range response.JSON200.Observations {
			_, _ = fmt.Fprintf(writer, "observation:%d\t%s\t%s\t%s\n", o.ID, o.Level, o.Status, o.Content)
		}
		return writer.Flush()
	}}
	list.Flags().StringVar(&level, "level", "", "Filter by comma-separated levels")
	list.Flags().StringVar(&status, "status", "", "Filter by comma-separated statuses")
	list.Flags().StringVar(&kind, "kind", "", "Filter by comma-separated kinds")
	list.Flags().StringVar(&asOf, "as-of", "", "Validity at an RFC3339 timestamp")
	list.Flags().BoolVar(&include, "include-sensitive", false, "Include sensitive observations")
	list.Flags().Int64Var(&limit, "limit", 100, "Maximum rows, up to 500")
	list.Flags().Int64Var(&offset, "offset", 0, "Zero-based offset")
	var showSensitive bool
	show := &cobra.Command{Use: "show <observation:id>", Short: "Show support, premises and history", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := observationCLIReference(cmd, args[0])
		if err != nil {
			return err
		}
		response, err := observationCLIShow(cmd, id, showSensitive)
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("observation response was empty")
		}
		if jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		o := response.JSON200
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "observation:%d\n%s\nLevel: %s\nStatus: %s\nKind: %s\nUpdated: %s\nSupport: %d citation(s), %d premise(s), %d dependent(s)\n", o.ID, o.Content, o.Level, o.Status, o.Kind, o.UpdatedAt.Format(time.RFC3339Nano), len(o.Evidence), len(o.Premises), len(o.Dependents))
		if err != nil {
			return fmt.Errorf("write observation details: %w", err)
		}
		return nil
	}}
	show.Flags().BoolVar(&showSensitive, "include-sensitive", false, "Include sensitive observations")
	var addKind, stability, subject, from, until, occurs string
	var categories []string
	add := &cobra.Command{Use: "add <person-id> <content>", Short: "Record an owner-authored observation", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		k := generated.AddOwnerObservationRequestKind(addKind)
		if err := k.Validate(); err != nil {
			return usageErr(cmd, err)
		}
		body := &generated.AddOwnerObservationBody{Content: args[1], Kind: &k, Categories: categories}
		if subject != "" {
			body.SubjectRef = &subject
		}
		if stability != "" {
			value := generated.AddOwnerObservationRequestStability(stability)
			if err := value.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			body.Stability = &value
		}
		for _, category := range categories {
			if !personobs.Category(category).Valid() {
				return usageErr(cmd, fmt.Errorf("invalid category: %s", category))
			}
		}
		body.ValidFrom, err = observationCLITime(cmd, "valid-from", from)
		if err != nil {
			return err
		}
		body.ValidUntil, err = observationCLITime(cmd, "valid-until", until)
		if err != nil {
			return err
		}
		body.OccursOn, err = observationCLIDate(cmd, occurs)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponseWithStatuses(client, []int{http.StatusCreated}, func(api *apiclient.Client) (*generated.AddOwnerObservationResp, error) {
			return api.AddOwnerObservationWithResponse(cmd.Context(), &generated.AddOwnerObservationRequestOptions{PathParams: &generated.AddOwnerObservationPath{ID: id}, Body: body})
		})
		if err != nil {
			return err
		}
		if response.JSON201 == nil {
			return errors.New("observation response was empty")
		}
		if jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "observation:%d\n", response.JSON201.ID)
		if err != nil {
			return fmt.Errorf("write added observation ID: %w", err)
		}
		return nil
	}}
	add.Flags().StringVar(&addKind, "kind", "state", "Observation kind")
	add.Flags().StringVar(&stability, "stability", "", "fixed, durable, volatile or episodic")
	add.Flags().StringVar(&subject, "subject-ref", "", "Optional subject reference")
	add.Flags().StringSliceVar(&categories, "category", nil, "Sensitivity category (repeatable)")
	add.Flags().StringVar(&from, "valid-from", "", "RFC3339 start")
	add.Flags().StringVar(&until, "valid-until", "", "RFC3339 end")
	add.Flags().StringVar(&occurs, "occurs-on", "", "YYYY, YYYY-MM or YYYY-MM-DD")
	var expected, endAt, reason string
	decide := &cobra.Command{Use: "decide <observation:id> <action>", Short: "Confirm, mark unsure, clear, reject, pin, unpin, end or forget", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := observationCLIReference(cmd, args[0])
		if err != nil {
			return err
		}
		action := generated.DecidePersonObservationRequestAction(args[1])
		if err := action.Validate(); err != nil {
			return usageErr(cmd, err)
		}
		body := &generated.DecidePersonObservationBody{Action: action}
		if reason != "" {
			value := generated.DecidePersonObservationRequestReason(reason)
			if err := value.Validate(); err != nil {
				return usageErr(cmd, err)
			}
			body.Reason = &value
		}
		body.EndAt, err = observationCLITime(cmd, "end-at", endAt)
		if err != nil {
			return err
		}
		at, err := observationCLITime(cmd, "expected-updated-at", expected)
		if err != nil {
			return err
		}
		if at == nil {
			response, err := observationCLIShow(cmd, id, true)
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("observation response was empty")
			}
			body.ExpectedUpdatedAt = response.JSON200.UpdatedAt
		} else {
			body.ExpectedUpdatedAt = *at
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.DecidePersonObservationResp, error) {
			return api.DecidePersonObservationWithResponse(cmd.Context(), &generated.DecidePersonObservationRequestOptions{PathParams: &generated.DecidePersonObservationPath{ID: id}, Body: body})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("observation response was empty")
		}
		if jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "observation:%d: %s\n", id, action)
		if err != nil {
			return fmt.Errorf("write observation decision: %w", err)
		}
		return nil
	}}
	decide.Flags().StringVar(&expected, "expected-updated-at", "", "Require this RFC3339 revision; defaults to the current revision")
	decide.Flags().StringVar(&endAt, "end-at", "", "RFC3339 end; defaults to now for end")
	decide.Flags().StringVar(&reason, "reason", "", "wrong, not_useful or outdated")
	suppressions := &cobra.Command{Use: "suppressions <person-id>", Short: "List active hash-only suppressions", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		response, err := daemonclient.APIResponse(client, func(api *apiclient.Client) (*generated.ListObservationSuppressionsResp, error) {
			return api.ListObservationSuppressionsWithResponse(cmd.Context(), &generated.ListObservationSuppressionsRequestOptions{PathParams: &generated.ListObservationSuppressionsPath{ID: id}})
		})
		if err != nil {
			return err
		}
		if response.JSON200 == nil {
			return errors.New("suppression response was empty")
		}
		if jsonOutput {
			return writePersonFactCLIRawJSON(cmd, response.Body)
		}
		writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(writer, "CONTENT KEY\tREASON\tREJECTED")
		for _, s := range response.JSON200.Suppressions {
			_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\n", s.ContentKey, s.Reason, s.RejectedAt.Format(time.RFC3339))
		}
		return writer.Flush()
	}}
	clearCmd := &cobra.Command{Use: "clear <person-id> <content-key>", Short: "Allow a suppressed statement to be proposed again", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		_, err = daemonclient.APIResponseWithStatuses(client, []int{http.StatusNoContent}, func(api *apiclient.Client) (*generated.ClearObservationSuppressionResp, error) {
			return api.ClearObservationSuppressionWithResponse(cmd.Context(), &generated.ClearObservationSuppressionRequestOptions{PathParams: &generated.ClearObservationSuppressionPath{ID: id, Key: args[1]}})
		})
		return err
	}}
	suppressions.AddCommand(clearCmd)
	root.AddCommand(list, show, add, decide, suppressions)
	return root
}
