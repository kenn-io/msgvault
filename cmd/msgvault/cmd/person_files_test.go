package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type fakePersonFilesClient struct {
	metadataRequest daemonclient.PersonFileSearchOptions
	documentRequest store.DocumentSearchRequest
	visualRequest   daemonclient.VisualSearchOptions
	metadata        generated.PersonFileSearchHTTPResponse
	documents       store.DocumentSearchResponse
	visual          *visual.SearchResponse
	documentErr     error
	visualErr       error
}

func (f *fakePersonFilesClient) SearchPersonFiles(_ context.Context, request daemonclient.PersonFileSearchOptions) (generated.PersonFileSearchHTTPResponse, error) {
	f.metadataRequest = request
	return f.metadata, nil
}

func (f *fakePersonFilesClient) SearchDocuments(_ context.Context, request store.DocumentSearchRequest) (store.DocumentSearchResponse, error) {
	f.documentRequest = request
	return f.documents, f.documentErr
}

func (f *fakePersonFilesClient) SearchVisualAttachmentsFiltered(_ context.Context, request daemonclient.VisualSearchOptions) (*visual.SearchResponse, error) {
	f.visualRequest = request
	return f.visual, f.visualErr
}

func TestPersonFilesDefaultsToMetadataAndForwardsStableFilters(t *testing.T) {
	assert := assert.New(t)
	client := &fakePersonFilesClient{metadata: generated.PersonFileSearchHTTPResponse{Files: []generated.PersonFileSearchRow{}}}
	command := newPersonFilesCommand(personFilesCommandDeps{openClient: func(context.Context) (personFilesClient, func(), error) {
		return client, func() {}, nil
	}})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"40", "--direction", "from_person,group", "--after", "2026-08-01",
		"--before", "2026-08-20", "--filename", "inspection", "--mime-family", "image,pdf", "--limit", "25", "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Equal(int64(40), client.metadataRequest.PersonID)
	assert.Equal([]personscope.Direction{personscope.FromPerson, personscope.Group}, client.metadataRequest.Directions)
	assert.Equal([]query.FileMIMEFamily{query.FileMIMEImage, query.FileMIMEPDF}, client.metadataRequest.MIMEFamilies)
	assert.Equal("inspection", client.metadataRequest.Filename)
	assert.Equal(25, client.metadataRequest.Limit)
	assert.Contains(output.String(), `"metadata":{"available":true}`)
}

func TestPersonFilesAllReportsUnavailableOptionalLanes(t *testing.T) {
	assert := assert.New(t)
	client := &fakePersonFilesClient{
		metadata:    generated.PersonFileSearchHTTPResponse{Files: []generated.PersonFileSearchRow{}},
		documentErr: errors.New("document search is not configured"),
		visual:      &visual.SearchResponse{Results: []visual.AttachmentSearchResult{}},
	}
	command := newPersonFilesCommand(personFilesCommandDeps{openClient: func(context.Context) (personFilesClient, func(), error) {
		return client, func() {}, nil
	}})
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"40", "--lane", "all", "--query", "inspection diagram", "--json"})
	require.NoError(t, command.ExecuteContext(t.Context()))
	assert.Equal(int64(40), client.documentRequest.PersonID)
	assert.Equal(int64(40), client.visualRequest.PersonID)
	assert.Contains(output.String(), `"documents":{"available":false`)
	assert.Contains(output.String(), `"visual":{"available":true}`)
	assert.Contains(output.String(), `"metadata":{"available":true,"reason":"semantic query is not applied to authoritative metadata"}`)
}

func TestPersonFilesRejectsSemanticQueryForMetadataLane(t *testing.T) {
	command := newPersonFilesCommand(personFilesCommandDeps{openClient: func(context.Context) (personFilesClient, func(), error) {
		require.Fail(t, "lane validation must happen before opening the daemon")
		return nil, func() {}, nil
	}})
	command.SetArgs([]string{"40", "--query", "diagram"})

	err := command.ExecuteContext(t.Context())

	require.Error(t, err)
	assert.ErrorContains(t, err, "--query requires")
}

func TestPersonFilesRejectsSharedCursorForAllLanes(t *testing.T) {
	command := newPersonFilesCommand(personFilesCommandDeps{openClient: func(context.Context) (personFilesClient, func(), error) {
		require.Fail(t, "cursor validation must happen before opening the daemon")
		return nil, func() {}, nil
	}})
	command.SetArgs([]string{"40", "--lane", "all", "--query", "diagram", "--cursor", "opaque"})
	err := command.ExecuteContext(t.Context())
	require.Error(t, err)
	assert.ErrorContains(t, err, "cursor")
}

func TestPersonFilesTextOutputKeepsEveryLaneAligned(t *testing.T) {
	assert := assert.New(t)
	filename, mimeType := "inspection.png", "image/png"
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	provenance := &personscope.Provenance{
		ParticipantIDs: []int64{4}, Roles: []personscope.Role{personscope.RoleFrom},
		Directions: []personscope.Direction{personscope.FromPerson},
	}
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	record := personFilesOutput{
		Availability: map[string]personFilesLaneStatus{
			personFilesLaneMetadata:  {Available: true},
			personFilesLaneDocuments: {Available: true},
			personFilesLaneVisual:    {Available: true},
		},
		Metadata: &generated.PersonFileSearchHTTPResponse{Files: []generated.PersonFileSearchRow{{
			ID: 11, MessageID: 12, ConversationID: 13, SourceID: 14, EntryKey: "entry-11",
			OccurredAt: now, Filename: &filename, MimeType: &mimeType,
			PersonProvenance: generated.PersonFileProvenance{
				ParticipantIds: []int64{4}, Roles: []generated.PersonFileProvenanceRoles{generated.From},
				Directions: []generated.PersonFileProvenanceDirections{generated.FromPerson},
			},
		}}},
		Documents: &store.DocumentSearchResponse{Results: []store.DocumentSearchResult{{
			Rank: 1, AttachmentID: 21, MessageID: 22, ConversationID: 23, SourceID: 24,
			SourceMessageID: "source-22", OccurredAt: &now, Filename: "inspection.pdf",
			Excerpt: "matching", PersonProvenance: provenance,
		}}},
		Visual: &visual.SearchResponse{Results: []visual.AttachmentSearchResult{{
			Rank: 1, AttachmentID: 31, MessageID: 32, ConversationID: 33, SourceID: 34,
			SourceMessageID: "source-32", SentAt: now, Filename: "inspection.png",
			Score: 0.9, PersonProvenance: provenance,
		}}},
	}

	require.NoError(t, writePersonFilesOutput(command, record))
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	var headerFields int
	rows := 0
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "LANE" {
			headerFields = len(fields)
			continue
		}
		if fields[0] == personFilesLaneMetadata || fields[0] == personFilesLaneDocuments || fields[0] == personFilesLaneVisual {
			if len(fields) > 1 && (fields[1] == "available" || fields[1] == "unavailable:") {
				continue
			}
			assert.Len(fields, headerFields, "row: %s", line)
			rows++
		}
	}
	assert.Equal(14, headerFields)
	assert.Equal(3, rows)
}
