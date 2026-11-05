package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/vector/visual"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type fakePersonFilesClient struct {
	metadataRequest daemonclient.PersonFileSearchOptions
	visualRequest   daemonclient.VisualSearchOptions
	metadata        generated.PersonFileSearchHTTPResponse
	visual          *visual.SearchResponse
	visualErr       error
}

func (f *fakePersonFilesClient) SearchPersonFiles(_ context.Context, request daemonclient.PersonFileSearchOptions) (generated.PersonFileSearchHTTPResponse, error) {
	f.metadataRequest = request
	return f.metadata, nil
}

func (f *fakePersonFilesClient) SuggestPersonFiles(context.Context, daemonclient.PersonFileSuggestionOptions) (generated.PersonFileSuggestionHTTPResponse, error) {
	return generated.PersonFileSuggestionHTTPResponse{}, errors.New("unexpected filename suggestion call in ordinary file search")
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
