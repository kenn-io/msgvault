package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	personresolver "go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	personFilesLaneMetadata  = "metadata"
	personFilesLaneDocuments = "documents"
	personFilesLaneVisual    = "visual"
	personFilesLaneAll       = "all"
)

type personFilesClient interface {
	SearchPersonFiles(ctx context.Context, options daemonclient.PersonFileSearchOptions) (generated.PersonFileSearchHTTPResponse, error)
	SearchDocuments(ctx context.Context, request store.DocumentSearchRequest) (store.DocumentSearchResponse, error)
	SearchVisualAttachmentsFiltered(ctx context.Context, options daemonclient.VisualSearchOptions) (*visual.SearchResponse, error)
}

type personFilesCommandDeps struct {
	openClient func(context.Context) (personFilesClient, func(), error)
}

type personFilesLaneStatus struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

type personFilesOutput struct {
	PersonID      int64                                   `json:"person_id"`
	RequestedLane string                                  `json:"requested_lane"`
	Availability  map[string]personFilesLaneStatus        `json:"availability"`
	Metadata      *generated.PersonFileSearchHTTPResponse `json:"metadata,omitempty"`
	Documents     *store.DocumentSearchResponse           `json:"documents,omitempty"`
	Visual        *visual.SearchResponse                  `json:"visual,omitempty"`
}

func defaultPersonFilesCommandDeps() personFilesCommandDeps {
	return personFilesCommandDeps{openClient: func(ctx context.Context) (personFilesClient, func(), error) {
		client, _, err := OpenHTTPStore(ctx)
		if err != nil {
			return nil, func() {}, err
		}
		return client, func() { _ = client.Close() }, nil
	}}
}

func newPersonFilesCommand(deps personFilesCommandDeps) *cobra.Command {
	var lane, queryText, filename, afterValue, beforeValue, cursor string
	var rawDirections, rawMIMEFamilies []string
	var limit int
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "files <person-id>",
		Short: "Retrieve files related to one durable person",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			personID, err := positivePersonCLIArg(command, args[0], personValue)
			if err != nil {
				return err
			}
			lane = strings.ToLower(strings.TrimSpace(lane))
			switch lane {
			case personFilesLaneMetadata, personFilesLaneDocuments, personFilesLaneVisual, personFilesLaneAll:
			default:
				return usageErr(command, errors.New("--lane must be metadata, documents, visual, or all"))
			}
			if limit < 1 || limit > 100 {
				return usageErr(command, errors.New("--limit must be between 1 and 100"))
			}
			queryText = strings.TrimSpace(queryText)
			filename = strings.TrimSpace(filename)
			if lane != personFilesLaneMetadata && queryText == "" {
				return usageErr(command, errors.New("--query is required for documents, visual, and all lanes"))
			}
			if lane == personFilesLaneMetadata && queryText != "" {
				return usageErr(command, errors.New("--query requires the documents, visual, or all lane"))
			}
			if lane == personFilesLaneAll && cursor != "" {
				return usageErr(command, errors.New("--cursor requires one lane because lane cursors are independent"))
			}
			directions := make([]personscope.Direction, len(rawDirections))
			for i, raw := range rawDirections {
				directions[i] = personscope.Direction(raw)
			}
			if len(directions) > 0 {
				directions, _, err = personresolver.NormalizeDirections(directions)
				if err != nil {
					return usageErr(command, err)
				}
			}
			mimeFamilies, err := parsePersonFileMIMEFamilies(rawMIMEFamilies)
			if err != nil {
				return usageErr(command, err)
			}
			after, err := parseDocumentSearchDate(afterValue)
			if err != nil {
				return usageErr(command, fmt.Errorf("invalid --after: %w", err))
			}
			before, err := parseDocumentSearchDate(beforeValue)
			if err != nil {
				return usageErr(command, fmt.Errorf("invalid --before: %w", err))
			}
			if after != nil && before != nil && !after.Before(*before) {
				return usageErr(command, errors.New("--after must be before --before"))
			}

			client, cleanup, err := deps.openClient(command.Context())
			if err != nil {
				return err
			}
			defer cleanup()
			output := personFilesOutput{
				PersonID: personID, RequestedLane: lane,
				Availability: make(map[string]personFilesLaneStatus, 3),
			}
			metadataRequest := daemonclient.PersonFileSearchOptions{
				PersonID: personID, Directions: directions, After: after, Before: before,
				Filename: filename, MIMEFamilies: mimeFamilies,
				Limit: limit, Cursor: cursor,
			}
			if lane == personFilesLaneMetadata || lane == personFilesLaneAll {
				metadata, metadataErr := client.SearchPersonFiles(command.Context(), metadataRequest)
				if metadataErr != nil {
					return metadataErr
				}
				output.Metadata = &metadata
				status := personFilesLaneStatus{Available: true}
				if lane == personFilesLaneAll {
					status.Reason = "semantic query is not applied to authoritative metadata"
				}
				output.Availability[personFilesLaneMetadata] = status
			}

			documentUnsupported := ""
			if filename != "" || len(mimeFamilies) > 0 {
				documentUnsupported = "document lane cannot apply exact filename or MIME-family filters"
			}
			if lane == personFilesLaneDocuments || lane == personFilesLaneAll {
				if documentUnsupported != "" {
					if lane == personFilesLaneDocuments {
						return usageErr(command, errors.New(documentUnsupported))
					}
					output.Availability[personFilesLaneDocuments] = personFilesLaneStatus{Reason: documentUnsupported}
				} else {
					documents, documentErr := client.SearchDocuments(command.Context(), store.DocumentSearchRequest{
						Query: queryText, PersonID: personID, Directions: directions,
						After: after, Before: before, PageSize: limit, Cursor: cursor,
					})
					if documentErr != nil {
						if lane == personFilesLaneDocuments {
							return documentErr
						}
						output.Availability[personFilesLaneDocuments] = personFilesLaneStatus{Reason: documentErr.Error()}
					} else {
						output.Documents = &documents
						output.Availability[personFilesLaneDocuments] = personFilesLaneStatus{Available: true}
					}
				}
			}

			if lane == personFilesLaneVisual || lane == personFilesLaneAll {
				mimePrefix, mimeErr := personFilesVisualMIMEPrefix(mimeFamilies)
				if mimeErr != nil {
					if lane == personFilesLaneVisual {
						return usageErr(command, mimeErr)
					}
					output.Availability[personFilesLaneVisual] = personFilesLaneStatus{Reason: mimeErr.Error()}
				} else {
					visualResult, visualErr := client.SearchVisualAttachmentsFiltered(command.Context(), daemonclient.VisualSearchOptions{
						Text: queryText, Limit: limit, Cursor: cursor, PersonID: personID,
						Directions: directions, Filename: filename, MIMEPrefix: mimePrefix,
						After: after, Before: before,
					})
					if visualErr != nil {
						if lane == personFilesLaneVisual {
							return visualErr
						}
						output.Availability[personFilesLaneVisual] = personFilesLaneStatus{Reason: visualErr.Error()}
					} else {
						output.Visual = visualResult
						output.Availability[personFilesLaneVisual] = personFilesLaneStatus{Available: true}
					}
				}
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(output)
			}
			return writePersonFilesOutput(command, output)
		},
	}
	command.Flags().StringSliceVar(&rawDirections, "direction", nil, "Person relation: from_person, to_person, or group")
	command.Flags().StringVar(&afterValue, "after", "", "Only messages on or after YYYY-MM-DD or RFC3339")
	command.Flags().StringVar(&beforeValue, "before", "", "Only messages before YYYY-MM-DD or RFC3339")
	command.Flags().StringSliceVar(&rawMIMEFamilies, "mime-family", nil, "Stable MIME family (repeatable)")
	command.Flags().StringVar(&filename, "filename", "", "Case-insensitive filename substring")
	command.Flags().StringVar(&lane, "lane", personFilesLaneMetadata, "Retrieval lane: metadata, documents, visual, or all")
	command.Flags().StringVar(&queryText, "query", "", "Semantic query required by documents, visual, and all lanes")
	command.Flags().IntVarP(&limit, "limit", "n", 100, "Maximum results per lane")
	command.Flags().StringVar(&cursor, "cursor", "", "Opaque cursor for one selected lane")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured grouped JSON")
	return command
}

func parsePersonFileMIMEFamilies(raw []string) ([]query.FileMIMEFamily, error) {
	allowed := map[query.FileMIMEFamily]bool{
		query.FileMIMEImage: true, query.FileMIMEPDF: true, query.FileMIMEAudio: true,
		query.FileMIMEVideo: true, query.FileMIMEText: true, query.FileMIMEDocument: true,
		query.FileMIMEArchive: true, query.FileMIMEOther: true,
	}
	result := make([]query.FileMIMEFamily, 0, len(raw))
	seen := make(map[query.FileMIMEFamily]bool, len(raw))
	for _, value := range raw {
		family := query.FileMIMEFamily(strings.ToLower(strings.TrimSpace(value)))
		if !allowed[family] {
			return nil, fmt.Errorf("unknown MIME family %q", value)
		}
		if !seen[family] {
			seen[family] = true
			result = append(result, family)
		}
	}
	return result, nil
}

func personFilesVisualMIMEPrefix(families []query.FileMIMEFamily) (string, error) {
	if len(families) == 0 {
		return "", nil
	}
	if len(families) > 1 {
		return "", errors.New("visual lane accepts at most one exactly mappable MIME family")
	}
	switch families[0] {
	case query.FileMIMEImage:
		return "image/", nil
	case query.FileMIMEVideo:
		return "video/", nil
	default:
		return "", fmt.Errorf("visual lane cannot exactly map MIME family %q", families[0])
	}
}

func writePersonFilesOutput(command *cobra.Command, output personFilesOutput) error {
	w := tabwriter.NewWriter(command.OutOrStdout(), 0, 0, 2, ' ', 0)
	for _, lane := range []string{personFilesLaneMetadata, personFilesLaneDocuments, personFilesLaneVisual} {
		status, selected := output.Availability[lane]
		if !selected {
			continue
		}
		state := "available"
		if !status.Available {
			state = "unavailable: " + status.Reason
		} else if status.Reason != "" {
			state += ": " + status.Reason
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\n", strings.ToUpper(lane), state)
	}
	_, _ = fmt.Fprintln(w, "LANE\tRANK\tATTACHMENT\tMESSAGE\tCONVERSATION\tSOURCE\tENTRY\tSOURCE-MESSAGE\tDATE\tFILE\tPARTICIPANTS\tROLES\tDIRECTIONS\tMATCH")
	if output.Metadata != nil {
		for i, result := range output.Metadata.Files {
			_, _ = fmt.Fprintf(w, "metadata\t%d\t%d\t%d\t%d\t%d\t%s\t-\t%s\t%s\t%v\t%v\t%v\t-\n",
				i+1, result.ID, result.MessageID, result.ConversationID, result.SourceID, result.EntryKey,
				personFilesTimestamp(&result.OccurredAt), pointerString(result.Filename),
				result.PersonProvenance.ParticipantIds, result.PersonProvenance.Roles,
				result.PersonProvenance.Directions)
		}
	}
	if output.Documents != nil {
		for _, result := range output.Documents.Results {
			participants, roles, directions := personProvenanceColumns(result.PersonProvenance)
			_, _ = fmt.Fprintf(w, "documents\t%d\t%d\t%d\t%d\t%d\t-\t%s\t%s\t%s\t%v\t%v\t%v\t%s\n",
				result.Rank, result.AttachmentID, result.MessageID, result.ConversationID, result.SourceID,
				result.SourceMessageID, personFilesTimestamp(result.OccurredAt), result.Filename,
				participants, roles, directions, strings.Join(strings.Fields(result.Excerpt), " "))
		}
	}
	if output.Visual != nil {
		for _, result := range output.Visual.Results {
			participants, roles, directions := personProvenanceColumns(result.PersonProvenance)
			_, _ = fmt.Fprintf(w, "visual\t%d\t%d\t%d\t%d\t%d\t-\t%s\t%s\t%s\t%v\t%v\t%v\t%.4f\n",
				result.Rank, result.AttachmentID, result.MessageID, result.ConversationID, result.SourceID,
				result.SourceMessageID, personFilesTimestamp(&result.SentAt), result.Filename,
				participants, roles, directions, result.Score)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("flush person files output: %w", err)
	}
	return nil
}

func personFilesTimestamp(value *time.Time) string {
	if value == nil || value.IsZero() {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func personProvenanceColumns(provenance *personscope.Provenance) ([]int64, []personscope.Role, []personscope.Direction) {
	if provenance == nil {
		return nil, nil, nil
	}
	return provenance.ParticipantIDs, provenance.Roles, provenance.Directions
}

func pointerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
