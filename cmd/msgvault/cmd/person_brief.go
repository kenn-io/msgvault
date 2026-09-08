package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/textutil"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	maxPersonBriefHistoryLimit = 200
	personBriefDateFormat      = "2006-01-02"
)

// personBriefGenerateLong warns that a manual generation is not free: the
// worker pays for the brief call and, when the person's cursors are already
// caught up, one bounded extraction page as well.
const personBriefGenerateLong = "Generate a person's brief now.\n\n" +
	"The daemon runs one manual sweep attempt for this person with the brief\n" +
	"forced, so it bypasses the minimum interval and the new-activity check but\n" +
	"still requires enrollment, a consented provider profile that allows\n" +
	"sensitive content, and available budget. A manual generation may also run\n" +
	"one bounded extraction page for the person, and it spends provider budget\n" +
	"for every call it makes.\n\n" +
	"A request that could never produce a brief is refused rather than reported\n" +
	"as a run that stored nothing: an unenrolled person, a brief lane turned off\n" +
	"with [people.sweep.brief] enabled = false, and a provider profile with\n" +
	"allow_sensitive = false each fail with the daemon's reason."

func newPersonBriefCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "brief",
		Short: "Read, generate, and enroll people in the \"last time we talked\" brief",
	}
	command.AddCommand(
		newPersonBriefShowCommand(),
		newPersonBriefHistoryCommand(),
		newPersonBriefGenerateCommand(),
		newPersonBriefRejectCommand(),
		newPersonBriefEnrollmentCommand("enroll", true),
		newPersonBriefEnrollmentCommand("unenroll", false),
	)
	return command
}

func newPersonBriefShowCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "show <person-id>",
		Short: "Show a person's current brief version",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, client, cleanup, err := openPersonBriefClient(cmd, args[0])
			if err != nil {
				return err
			}
			defer cleanup()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.GetPersonBriefResp, error) {
					return api.GetPersonBriefWithResponse(cmd.Context(),
						&generated.GetPersonBriefRequestOptions{
							PathParams: &generated.GetPersonBriefPath{ID: personID},
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person brief response was empty")
			}
			if jsonOutput {
				return writePersonBriefRawJSON(cmd, response.Body)
			}
			return writePersonBrief(cmd.OutOrStdout(), personID, *response.JSON200)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonBriefHistoryCommand() *cobra.Command {
	var jsonOutput bool
	var limit int
	command := &cobra.Command{
		Use:   "history <person-id>",
		Short: "List a person's brief version history",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, client, cleanup, err := openPersonBriefClient(cmd, args[0])
			if err != nil {
				return err
			}
			defer cleanup()
			query := &generated.ListPersonBriefVersionsQuery{}
			if cmd.Flags().Changed("limit") {
				if limit < 1 || limit > maxPersonBriefHistoryLimit {
					return usageErr(cmd, fmt.Errorf("--limit must be between 1 and %d",
						maxPersonBriefHistoryLimit))
				}
				bounded := int64(limit)
				query.Limit = &bounded
			}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonBriefVersionsResp, error) {
					return api.ListPersonBriefVersionsWithResponse(cmd.Context(),
						&generated.ListPersonBriefVersionsRequestOptions{
							PathParams: &generated.ListPersonBriefVersionsPath{ID: personID},
							Query:      query,
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person brief history response was empty")
			}
			if jsonOutput {
				return writePersonBriefRawJSON(cmd, response.Body)
			}
			return writePersonBriefHistory(cmd.OutOrStdout(), response.JSON200.Versions)
		},
	}
	command.Flags().IntVar(&limit, "limit", 20, "Maximum versions to list")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonBriefGenerateCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "generate <person-id>",
		Short: "Generate a person's brief now",
		Long:  personBriefGenerateLong,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, client, cleanup, err := openPersonBriefClient(cmd, args[0])
			if err != nil {
				return err
			}
			defer cleanup()
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.GeneratePersonBriefResp, error) {
					return api.GeneratePersonBriefWithResponse(cmd.Context(),
						&generated.GeneratePersonBriefRequestOptions{
							PathParams: &generated.GeneratePersonBriefPath{ID: personID},
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person brief generation response was empty")
			}
			if jsonOutput {
				return writePersonBriefRawJSON(cmd, response.Body)
			}
			return writePersonBriefRun(cmd.OutOrStdout(), personID, *response.JSON200)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonBriefRejectCommand() *cobra.Command {
	var jsonOutput bool
	var reason string
	command := &cobra.Command{
		Use:   "reject <person-id>",
		Short: "Reject a person's current brief version",
		Long: "Reject a person's current brief version. The version stays readable in\n" +
			"history; the next generation produces the next version.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, client, cleanup, err := openPersonBriefClient(cmd, args[0])
			if err != nil {
				return err
			}
			defer cleanup()
			// The route accepts an absent reason; the CLI always sends the
			// field so the request body says exactly what the owner typed.
			body := generated.RejectPersonBriefBody{Reason: &reason}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.RejectPersonBriefResp, error) {
					return api.RejectPersonBriefWithResponse(cmd.Context(),
						&generated.RejectPersonBriefRequestOptions{
							PathParams: &generated.RejectPersonBriefPath{ID: personID},
							Body:       &body,
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person brief rejection response was empty")
			}
			if jsonOutput {
				return writePersonBriefRawJSON(cmd, response.Body)
			}
			brief := *response.JSON200
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Person %d brief version %d: %s\nReason: %s\n",
				personID, brief.Version, brief.Status,
				personBriefReasonOrDash(brief.RejectedReason))
			if err != nil {
				return fmt.Errorf("write person brief rejection: %w", err)
			}
			return nil
		},
	}
	command.Flags().StringVar(&reason, "reason", "", "Why this version was rejected")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonBriefEnrollmentCommand(action string, enrolled bool) *cobra.Command {
	var jsonOutput, track bool
	short := "Enroll a durable person in generated briefs"
	if !enrolled {
		short = "Remove a durable person from generated briefs"
	}
	command := &cobra.Command{
		Use:   action + " <person-id>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			personID, client, cleanup, err := openPersonBriefClient(cmd, args[0])
			if err != nil {
				return err
			}
			defer cleanup()
			body := generated.SetPersonBriefEnrollmentBody{Enrolled: enrolled, Track: &track}
			response, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.SetPersonBriefEnrollmentResp, error) {
					return api.SetPersonBriefEnrollmentWithResponse(cmd.Context(),
						&generated.SetPersonBriefEnrollmentRequestOptions{
							PathParams: &generated.SetPersonBriefEnrollmentPath{ID: personID},
							Body:       &body,
						})
				})
			if err != nil {
				return err
			}
			if response.JSON200 == nil {
				return errors.New("person brief enrollment response was empty")
			}
			if jsonOutput {
				return writePersonBriefRawJSON(cmd, response.Body)
			}
			state := "not enrolled"
			if response.JSON200.Enrolled {
				state = "enrolled"
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Person %d brief: %s\n",
				personID, state); err != nil {
				return fmt.Errorf("write person brief enrollment: %w", err)
			}
			return nil
		},
	}
	if enrolled {
		command.Flags().BoolVar(&track, "track", false,
			"Track the person first when it is not tracked yet")
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

// openPersonBriefClient validates the person ID before any network call and
// opens the daemon client every brief command talks to.
func openPersonBriefClient(
	cmd *cobra.Command, rawPersonID string,
) (int64, *daemonclient.Client, func(), error) {
	personID, err := positivePersonCLIArg(cmd, rawPersonID, personValue)
	if err != nil {
		return 0, nil, nil, err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return 0, nil, nil, err
	}
	return personID, client, func() { _ = client.Close() }, nil
}

// writePersonBriefRawJSON forwards the daemon's exact response body, so --json
// never reshapes the contract a script depends on.
func writePersonBriefRawJSON(cmd *cobra.Command, body []byte) error {
	if _, err := cmd.OutOrStdout().Write(body); err != nil {
		return fmt.Errorf("write person brief JSON: %w", err)
	}
	if bytes.HasSuffix(body, []byte("\n")) {
		return nil
	}
	if _, err := cmd.OutOrStdout().Write([]byte("\n")); err != nil {
		return fmt.Errorf("write person brief JSON: %w", err)
	}
	return nil
}

func writePersonBrief(w io.Writer, personID int64, brief generated.PersonBrief) error {
	if _, err := fmt.Fprintf(w, "Person %d brief version %d (%s, generated %s)\n",
		personID, brief.Version, brief.Status,
		brief.GeneratedAt.UTC().Format(personBriefDateFormat)); err != nil {
		return fmt.Errorf("write person brief: %w", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n",
		textutil.SanitizeTerminal(brief.RenderedText)); err != nil {
		return fmt.Errorf("write person brief: %w", err)
	}
	if brief.RejectedAt != nil {
		if _, err := fmt.Fprintf(w, "Rejected %s: %s\n",
			brief.RejectedAt.UTC().Format(personBriefDateFormat),
			personBriefReasonOrDash(brief.RejectedReason)); err != nil {
			return fmt.Errorf("write person brief: %w", err)
		}
	}
	if brief.DroppedItemCount > 0 {
		if _, err := fmt.Fprintf(w, "Dropped items: %d\n", brief.DroppedItemCount); err != nil {
			return fmt.Errorf("write person brief: %w", err)
		}
	}
	if len(brief.Evidence) == 0 {
		if _, err := fmt.Fprintln(w, "Evidence: none"); err != nil {
			return fmt.Errorf("write person brief: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(w, "Evidence:"); err != nil {
		return fmt.Errorf("write person brief: %w", err)
	}
	for _, item := range brief.Evidence {
		support := ""
		if !item.EvidenceSupported {
			support = " (unsupported)"
		}
		if _, err := fmt.Fprintf(w, "- %s %s%s\n",
			item.EventTime.UTC().Format(personBriefDateFormat),
			textutil.SanitizeTerminal(item.Directness), support); err != nil {
			return fmt.Errorf("write person brief: %w", err)
		}
	}
	return nil
}

func writePersonBriefHistory(w io.Writer, versions []generated.PersonBrief) error {
	table := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "VERSION\tSTATUS\tGENERATED\tDROPPED\tEVIDENCE")
	for _, version := range versions {
		_, _ = fmt.Fprintf(table, "%d\t%s\t%s\t%d\t%d\n", version.Version,
			textutil.SanitizeTerminal(version.Status),
			version.GeneratedAt.UTC().Format(personBriefDateFormat),
			version.DroppedItemCount, len(version.Evidence))
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write person brief history: %w", err)
	}
	return nil
}

func writePersonBriefRun(w io.Writer, personID int64, run generated.PersonBriefRun) error {
	attempt := run.AttemptID
	if attempt == "" {
		attempt = "-"
	}
	outcome := "no new version"
	if run.BriefVersion > 0 {
		outcome = "version " + strconv.FormatInt(run.BriefVersion, 10)
	}
	if run.BriefFailureClass != "" {
		outcome += " (" + textutil.SanitizeTerminal(run.BriefFailureClass) + ")"
	}
	if _, err := fmt.Fprintf(w, "Person %d brief run %s: attempt=%s %s\n", personID,
		textutil.SanitizeTerminal(run.RunID), textutil.SanitizeTerminal(attempt),
		outcome); err != nil {
		return fmt.Errorf("write person brief run: %w", err)
	}
	return nil
}

func personBriefReasonOrDash(reason string) string {
	if reason == "" {
		return "-"
	}
	return textutil.SanitizeTerminal(reason)
}
