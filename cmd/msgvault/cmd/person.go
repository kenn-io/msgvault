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
	personJSON             bool
	personClearDisplayName bool
)

const personValue = "person"

var personCmd = &cobra.Command{
	Use:   personValue,
	Short: "Manage durable person profiles",
}

var personPromoteCmd = &cobra.Command{
	Use:   "promote <participant-id>",
	Short: "Promote a participant's identity cluster to a durable person",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		participantID, err := positivePersonCLIArg(cmd, args[0], "participant")
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		body := generated.CreatePersonBody{ParticipantID: participantID}
		resp, err := daemonclient.APIResponseWithStatuses(client,
			[]int{http.StatusOK, http.StatusCreated},
			func(api *apiclient.Client) (*generated.CreatePersonResp, error) {
				return api.CreatePersonWithResponse(cmd.Context(),
					&generated.CreatePersonRequestOptions{Body: &body})
			})
		if err != nil {
			return err
		}
		// 201 carries a newly created person; 200 an idempotent re-promotion.
		person := resp.JSON201
		if person == nil {
			person = resp.JSON200
		}
		return writeCLIPerson(cmd, person)
	},
}

var personGetCmd = &cobra.Command{
	Use:   "get <person-id>",
	Short: "Get a durable person profile",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := getCLIPerson(cmd, client, id)
		if err != nil {
			return err
		}
		return writeCLIPerson(cmd, resp.JSON200)
	},
}

var personListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List durable person profiles",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.ListPeopleResp, error) {
				return api.ListPeopleWithResponse(cmd.Context())
			})
		if err != nil {
			return err
		}
		if personJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.People)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "ID\tDISPLAY NAME\tVCARD UID\tPARTICIPANTS\tREVISION")
		for _, person := range resp.JSON200.People {
			_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%d\t%d\n", person.ID,
				personDisplayName(person.DisplayName), person.VcardUID,
				len(person.ParticipantIds), person.Revision)
		}
		return w.Flush()
	},
}

var personSetDisplayNameCmd = &cobra.Command{
	Use:   "set-display-name <person-id> [display-name]",
	Short: "Set a durable person's display-name override",
	Args: func(cmd *cobra.Command, args []string) error {
		switch {
		case personClearDisplayName && len(args) != 1:
			return usageErr(cmd, errors.New("--clear cannot be used with a display name"))
		case !personClearDisplayName && len(args) != 2:
			return usageErr(cmd, errors.New("display name is required unless --clear is used"))
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		var displayName *string
		if !personClearDisplayName {
			value := strings.TrimSpace(args[1])
			if value == "" {
				return usageErr(cmd, errors.New("display name must not be empty"))
			}
			displayName = &value
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIPerson(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("person response was empty")
		}
		etag := fmt.Sprintf(`"person-%d-r%d"`, id, current.JSON200.Revision)
		body := generated.PatchPersonBody{DisplayName: displayName}
		resp, err := daemonclient.APIResponse(client,
			func(api *apiclient.Client) (*generated.PatchPersonResp, error) {
				return api.PatchPersonWithResponse(cmd.Context(), &generated.PatchPersonRequestOptions{
					PathParams: &generated.PatchPersonPath{ID: id},
					Header:     &generated.PatchPersonHeaders{IfMatch: etag},
					Body:       &body,
				})
			})
		if err != nil {
			return err
		}
		return writeCLIPerson(cmd, resp.JSON200)
	},
}

var personDeleteCmd = &cobra.Command{
	Use:   "delete <person-id>",
	Short: "Permanently delete a durable person profile",
	Long: "Permanently delete a durable person profile. The person's participant\n" +
		"bindings are removed and its vCard UID is retired forever; re-promoting\n" +
		"the same cluster afterwards creates a new person with a new UID.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		current, err := getCLIPerson(cmd, client, id)
		if err != nil {
			return err
		}
		if current.JSON200 == nil {
			return errors.New("person response was empty")
		}
		etag := fmt.Sprintf(`"person-%d-r%d"`, id, current.JSON200.Revision)
		if _, err := daemonclient.APIResponseWithStatuses(client,
			[]int{http.StatusNoContent},
			func(api *apiclient.Client) (*generated.DeletePersonResp, error) {
				return api.DeletePersonWithResponse(cmd.Context(), &generated.DeletePersonRequestOptions{
					PathParams: &generated.DeletePersonPath{ID: id},
					Header:     &generated.DeletePersonHeaders{IfMatch: etag},
				})
			}); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted person %d\n", id)
		return nil
	},
}

var (
	personMergeCmd          = newPersonMergeCommand()
	personSplitCmd          = newPersonSplitCommand()
	personMergeHistoryCmd   = newPersonMergeHistoryCommand()
	personMergeShowCmd      = newPersonMergeShowCommand()
	personMergeCandidateCmd = newPersonMergeCandidateCommand()
)

func newPersonMergeCommand() *cobra.Command {
	var survivorRevision, absorbedRevision int64
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "merge <survivor-id> <absorbed-id>",
		Short: "Merge one durable person profile into another",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			survivorID, err := positivePersonCLIArg(cmd, args[0], "survivor person")
			if err != nil {
				return err
			}
			absorbedID, err := positivePersonCLIArg(cmd, args[1], "absorbed person")
			if err != nil {
				return err
			}
			if survivorID == absorbedID {
				return usageErr(cmd, errors.New("survivor and absorbed person must differ"))
			}
			if err := positivePersonCLIRevision(cmd, survivorRevision, "survivor"); err != nil {
				return err
			}
			if err := positivePersonCLIRevision(cmd, absorbedRevision, "absorbed"); err != nil {
				return err
			}
			idempotencyKey, err = personCLIIdempotencyKey(cmd, idempotencyKey)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.MergePersonsBody{AbsorbedPersonID: absorbedID}
			resp, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.MergePersonsResp, error) {
					return api.MergePersonsWithResponse(cmd.Context(),
						&generated.MergePersonsRequestOptions{
							PathParams: &generated.MergePersonsPath{ID: survivorID},
							Header: &generated.MergePersonsHeaders{
								IfMatch: personMergeCLIIfMatch(
									survivorID, survivorRevision, absorbedID, absorbedRevision),
								IdempotencyKey: idempotencyKey,
							},
							Body: &body,
						})
				})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
			}
			writePersonMergeResult(cmd, resp.JSON200)
			return nil
		},
	}
	command.Flags().Int64Var(&survivorRevision, "survivor-revision", 0,
		"Expected survivor person revision")
	command.Flags().Int64Var(&absorbedRevision, "absorbed-revision", 0,
		"Expected absorbed person revision")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "",
		"Opaque retry key for this merge")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonSplitCommand() *cobra.Command {
	var mergeID, revision int64
	var participantIDs []int64
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "split <source-person-id>",
		Short: "Split selected merged participant lineage into a new person",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sourceID, err := positivePersonCLIArg(cmd, args[0], "source person")
			if err != nil {
				return err
			}
			if mergeID <= 0 {
				return usageErr(cmd, errors.New("merge ID must be a positive integer"))
			}
			if err := positivePersonCLIRevision(cmd, revision, "source"); err != nil {
				return err
			}
			if err := validatePersonCLIParticipants(cmd, participantIDs); err != nil {
				return err
			}
			idempotencyKey, err = personCLIIdempotencyKey(cmd, idempotencyKey)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.SplitPersonMergeBody{
				MergeID: mergeID, ParticipantIds: participantIDs,
			}
			resp, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.SplitPersonMergeResp, error) {
					return api.SplitPersonMergeWithResponse(cmd.Context(),
						&generated.SplitPersonMergeRequestOptions{
							PathParams: &generated.SplitPersonMergePath{ID: sourceID},
							Header: &generated.SplitPersonMergeHeaders{
								IfMatch: personCLIETag(sourceID, revision), IdempotencyKey: idempotencyKey,
							},
							Body: &body,
						})
				})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
			}
			writePersonSplitResult(cmd, resp.JSON200)
			return nil
		},
	}
	command.Flags().Int64Var(&mergeID, "merge-id", 0, "Merge record to split")
	command.Flags().Int64SliceVar(&participantIDs, "participant", nil,
		"Participant lineage to move; repeat for multiple participants")
	command.Flags().Int64Var(&revision, "revision", 0, "Expected source person revision")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "",
		"Opaque retry key for this split")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonMergeHistoryCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "merge-history <person-id>",
		Short: "List merge and split history for a durable person",
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
			resp, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.ListPersonMergesResp, error) {
					return api.ListPersonMergesWithResponse(cmd.Context(),
						&generated.ListPersonMergesRequestOptions{
							PathParams: &generated.ListPersonMergesPath{ID: personID},
						})
				})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200.Merges)
			}
			return writePersonMergeHistory(cmd, resp.JSON200.Merges)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonMergeShowCommand() *cobra.Command {
	var snapshot, jsonOutput bool
	command := &cobra.Command{
		Use:   "merge-show <merge-id>",
		Short: "Inspect a durable person merge",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mergeID, err := positivePersonCLIArg(cmd, args[0], "merge")
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			if snapshot {
				resp, loadErr := daemonclient.APIResponse(client,
					func(api *apiclient.Client) (*generated.GetPersonMergeSnapshotResp, error) {
						return api.GetPersonMergeSnapshotWithResponse(cmd.Context(),
							&generated.GetPersonMergeSnapshotRequestOptions{
								PathParams: &generated.GetPersonMergeSnapshotPath{MergeID: mergeID},
							})
					})
				if loadErr != nil {
					return loadErr
				}
				if jsonOutput {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(),
					"Merge snapshot: %d\nVersion: %d\nSHA-256: %s\nSnapshot: %s\n",
					mergeID, resp.JSON200.Version, resp.JSON200.Sha256, resp.JSON200.Snapshot)
				return nil
			}
			resp, loadErr := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.GetPersonMergeResp, error) {
					return api.GetPersonMergeWithResponse(cmd.Context(),
						&generated.GetPersonMergeRequestOptions{
							PathParams: &generated.GetPersonMergePath{MergeID: mergeID},
						})
				})
			if loadErr != nil {
				return loadErr
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
			}
			writePersonMergeDetail(cmd, resp.JSON200)
			return nil
		},
	}
	command.Flags().BoolVar(&snapshot, "snapshot", false, "Show the verified merge snapshot")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func newPersonMergeCandidateCommand() *cobra.Command {
	var personID, revision int64
	var decision string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "merge-candidate <candidate-id>",
		Short: "Accept or reject a person merge review candidate",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			candidateID, err := positivePersonCLIArg(cmd, args[0], "candidate")
			if err != nil {
				return err
			}
			if personID <= 0 {
				return usageErr(cmd, errors.New("person ID must be a positive integer"))
			}
			if err := positivePersonCLIRevision(cmd, revision, "person"); err != nil {
				return err
			}
			mappedDecision, err := personCLICandidateDecision(cmd, decision)
			if err != nil {
				return err
			}
			client, _, err := OpenHTTPStore(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			body := generated.DecidePersonMergeCandidateBody{
				PersonID: personID, Decision: mappedDecision,
			}
			resp, err := daemonclient.APIResponse(client,
				func(api *apiclient.Client) (*generated.DecidePersonMergeCandidateResp, error) {
					return api.DecidePersonMergeCandidateWithResponse(cmd.Context(),
						&generated.DecidePersonMergeCandidateRequestOptions{
							PathParams: &generated.DecidePersonMergeCandidatePath{CandidateID: candidateID},
							Header: &generated.DecidePersonMergeCandidateHeaders{
								IfMatch: personCLIETag(personID, revision),
							},
							Body: &body,
						})
				})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.JSON200)
			}
			personETag := ""
			if resp.Headers200 != nil {
				personETag = resp.Headers200.ETag
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(),
				"Candidate: %d\nMerge: %d\nPerson: %d\nState: %s\nPerson ETag: %s\n",
				resp.JSON200.ID, resp.JSON200.MergeID, resp.JSON200.PersonID,
				resp.JSON200.State, personETag)
			return nil
		},
	}
	command.Flags().Int64Var(&personID, "person-id", 0,
		"Person profile that owns the candidate")
	command.Flags().StringVar(&decision, "decision", "",
		"Decision: accepted or rejected")
	command.Flags().Int64Var(&revision, "revision", 0, "Expected person revision")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output as JSON")
	return command
}

func writePersonMergeResult(cmd *cobra.Command, result *generated.PersonMergeResult) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Merge: %d\nSurvivor: %d\nSurvivor UID: %s\nAbsorbed: %d\nAbsorbed UID: %s\n"+
			"Survivor revision: %d -> %d\nAbsorbed revision: %d\n"+
			"Absorbed UID alias: %s -> %s\nReview candidates: %d\n"+
			"Identity revision: %d\nCache state: %s\n",
		result.Merge.ID, result.Person.ID, result.Person.VcardUID,
		result.Merge.AbsorbedPersonID, result.Merge.AbsorbedVcardUID,
		result.Merge.SurvivorRevisionBefore, result.Merge.SurvivorRevisionAfter,
		result.Merge.AbsorbedRevisionBefore, result.Merge.AbsorbedVcardUID,
		result.Person.VcardUID, len(result.ReviewCandidates),
		result.IdentityRevision, result.CacheState)
}

func writePersonSplitResult(cmd *cobra.Command, result *generated.PersonSplitResult) {
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Split: %d\nMerge: %d\nSource person: %d\nSource UID: %s\n"+
			"New person: %d\nNew UID: %s\nSource revision: %d -> %d\n"+
			"Exact reversal: %t\nUID alias disposition: %s\nAmbiguous rows: %d\n"+
			"Identity revision: %d\nCache state: %s\n",
		result.Split.ID, result.Split.MergeID, result.SourcePerson.ID,
		result.SourcePerson.VcardUID, result.NewPerson.ID, result.NewPerson.VcardUID,
		result.Split.SourceRevisionBefore, result.Split.SourceRevisionAfter,
		result.ExactReversal, result.UIDAliasDisposition, len(result.AmbiguousRows),
		result.IdentityRevision, result.CacheState)
}

func writePersonMergeHistory(cmd *cobra.Command, history []generated.PersonMergeSummary) error {
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "MERGE\tSURVIVOR\tABSORBED\tCURRENT\tSPLITS\tPENDING\tROWS")
	for _, summary := range history {
		current := "-"
		if summary.Merge.CurrentPersonID != nil {
			current = strconv.FormatInt(*summary.Merge.CurrentPersonID, 10)
		}
		_, _ = fmt.Fprintf(w, "%d\t%d\t%d\t%s\t%d\t%d\t%d\n",
			summary.Merge.ID, summary.Merge.SurvivorPersonID,
			summary.Merge.AbsorbedPersonID, current, summary.SplitCount,
			summary.PendingCandidateCount, summary.RowCount)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush person merge history: %w", err)
	}
	return nil
}

func writePersonMergeDetail(cmd *cobra.Command, detail *generated.PersonMergeDetail) {
	current := "-"
	if detail.Merge.CurrentPersonID != nil {
		current = strconv.FormatInt(*detail.Merge.CurrentPersonID, 10)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Merge: %d\nSurvivor: %d (%s)\nAbsorbed: %d (%s)\nCurrent person: %s\n"+
			"Participants: %d\nRows: %d\nSplits: %d\nReview candidates: %d\nSnapshot SHA-256: %s\n",
		detail.Merge.ID, detail.Merge.SurvivorPersonID, detail.Merge.SurvivorVcardUID,
		detail.Merge.AbsorbedPersonID, detail.Merge.AbsorbedVcardUID, current,
		len(detail.Participants), len(detail.Rows), len(detail.Splits),
		len(detail.ReviewCandidates), detail.Merge.SnapshotSha256)
}

func positivePersonCLIRevision(cmd *cobra.Command, revision int64, kind string) error {
	if revision <= 0 {
		return usageErr(cmd, fmt.Errorf("%s revision must be a positive integer", kind))
	}
	return nil
}

func personCLIIdempotencyKey(cmd *cobra.Command, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", usageErr(cmd, errors.New("idempotency key is required"))
	}
	if len(value) > 128 {
		return "", usageErr(cmd, errors.New("idempotency key must be at most 128 bytes"))
	}
	return value, nil
}

func validatePersonCLIParticipants(cmd *cobra.Command, participantIDs []int64) error {
	if len(participantIDs) == 0 {
		return usageErr(cmd, errors.New("at least one participant ID is required"))
	}
	seen := make(map[int64]struct{}, len(participantIDs))
	for _, participantID := range participantIDs {
		if participantID <= 0 {
			return usageErr(cmd, errors.New("participant IDs must be positive integers"))
		}
		if _, duplicate := seen[participantID]; duplicate {
			return usageErr(cmd, fmt.Errorf("duplicate participant ID %d", participantID))
		}
		seen[participantID] = struct{}{}
	}
	return nil
}

func personCLICandidateDecision(
	cmd *cobra.Command, value string,
) (generated.DecidePersonMergeCandidateRequestDecision, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "accepted", "accept":
		return generated.Accept, nil
	case "rejected", "reject":
		return generated.Reject, nil
	default:
		return "", usageErr(cmd, errors.New("decision must be accepted or rejected"))
	}
}

func personCLIETag(personID, revision int64) string {
	return fmt.Sprintf(`"person-%d-r%d"`, personID, revision)
}

func personMergeCLIIfMatch(
	survivorID, survivorRevision, absorbedID, absorbedRevision int64,
) string {
	return personCLIETag(survivorID, survivorRevision) + ", " +
		personCLIETag(absorbedID, absorbedRevision)
}

func getCLIPerson(
	cmd *cobra.Command, client *daemonclient.Client, id int64,
) (*generated.GetPersonProfileResp, error) {
	return daemonclient.APIResponse(client,
		func(api *apiclient.Client) (*generated.GetPersonProfileResp, error) {
			return api.GetPersonProfileWithResponse(cmd.Context(),
				&generated.GetPersonProfileRequestOptions{
					PathParams: &generated.GetPersonProfilePath{ID: id},
				})
		})
}

func writeCLIPerson(cmd *cobra.Command, person *generated.Person) error {
	if person == nil {
		return errors.New("person response was empty")
	}
	if personJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(person)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Person: %d\nDisplay name: %s\nvCard UID: %s\nParticipants: %v\nRevision: %d\n",
		person.ID, personDisplayName(person.DisplayName), person.VcardUID,
		person.ParticipantIds, person.Revision)
	return nil
}

func personDisplayName(name *string) string {
	if name == nil {
		return "-"
	}
	return *name
}

func positivePersonCLIArg(cmd *cobra.Command, raw, kind string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, usageErr(cmd, fmt.Errorf("%s ID must be a positive integer", kind))
	}
	return id, nil
}

func init() {
	rootCmd.AddCommand(personCmd)
	personCmd.AddCommand(newPersonProviderCommand(defaultPersonProviderCommandDeps()))
	personCmd.AddCommand(personPromoteCmd, personGetCmd, personListCmd,
		personSetDisplayNameCmd, personDeleteCmd, personTrackCmd, personUntrackCmd,
		personMergeCmd, personSplitCmd, personMergeHistoryCmd, personMergeShowCmd,
		personMergeCandidateCmd, newPersonFilesCommand(defaultPersonFilesCommandDeps()),
		personSearchCmd)
	for _, command := range []*cobra.Command{
		personPromoteCmd, personGetCmd, personListCmd, personSetDisplayNameCmd,
		personTrackCmd, personUntrackCmd,
	} {
		command.Flags().BoolVar(&personJSON, flagJSON, false, "Output as JSON")
	}
	personSetDisplayNameCmd.Flags().BoolVar(
		&personClearDisplayName, "clear", false, "Clear the display-name override",
	)
}
