package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/importantdate"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type importantDatePreview struct {
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	Occurrence string `json:"occurrence,omitempty"`
	Days       *int   `json:"days,omitempty"`
}

func previewImportantDates(dates []store.PersonDate, now time.Time, location *time.Location, policy importantdate.LeapDayPolicy) []importantDatePreview {
	rows := make([]importantDatePreview, 0, 2)
	for _, kind := range []store.PersonDateKind{store.PersonDateBirthday, store.PersonDateAnniversary} {
		date, status := importantdate.SelectDeclaredDate(dates, kind, now)
		row := importantDatePreview{Kind: string(kind), Status: string(status)}
		if status == importantdate.DateEligible {
			if occurrence, ok := importantdate.NextOccurrence(date, now, location, policy); ok {
				row.Status = "date_valid"
				row.Occurrence = occurrence.Format(time.DateOnly)
				days := importantdate.DaysUntil(now, occurrence, location)
				row.Days = &days
			} else {
				row.Status = "no_occurrence"
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func newPersonImportantDatesCommand() *cobra.Command {
	var zone, leap string
	command := &cobra.Command{
		Use:   "important-dates <person-id>",
		Short: "Preview a person's declared annual dates",
		Long:  "Preview current declared dates for one person without writing to Kata. This preview does not evaluate eligibility, children, pets, consent, or delivery.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := positivePersonCLIArg(cmd, args[0], personValue)
			if err != nil {
				return err
			}
			location, err := time.LoadLocation(zone)
			if err != nil {
				return fmt.Errorf("invalid time zone: %w", err)
			}
			policy := importantdate.LeapDayPolicy(leap)
			if policy != importantdate.LeapFebruary28 && policy != importantdate.LeapMarch1 && policy != importantdate.LeapOnly {
				return errors.New("invalid leap-day policy")
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
			var profile struct {
				Dates []store.PersonDate `json:"dates"`
			}
			if err := json.Unmarshal(response.Body, &profile); err != nil {
				return fmt.Errorf("decode profile: %w", err)
			}
			return writeJSONOutput(cmd, previewImportantDates(profile.Dates, time.Now(), location, policy))
		},
	}
	command.Flags().StringVar(&zone, "timezone", "UTC", "IANA time zone")
	command.Flags().StringVar(&leap, "leap-day", string(importantdate.LeapFebruary28), "Leap-day policy: february_28, march_1, or leap_years_only")
	return command
}
