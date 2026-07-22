package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedSavedViewStateRoundTripsCanonicalDefinition(t *testing.T) {
	want := `{
		"query":"invoice",
		"search_mode":"full_text",
		"filters":[{"field":"source_id","operator":"eq","values":["9007199254740993"]}],
		"grouping":["sender"],
		"presentation":"table",
		"sort":[{"field":"sent_at","direction":"desc"}],
		"columns":["sender","subject"],
		"inspector_pinned":true
	}`

	var state generated.SavedViewStateEnvelope
	require.NoError(t, json.Unmarshal([]byte(want), &state))
	got, err := json.Marshal(state)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(got))
}

func TestGeneratedEnumNamesPreserveSavedViewCompatibilityAndQualifyExploration(t *testing.T) {
	assertions := assert.New(t)
	assertions.Equal(generated.Asc, generated.SavedViewSortDirection("asc"))
	assertions.Equal(generated.Desc, generated.SavedViewSortDirection("desc"))
	assertions.Equal(generated.IdentitySearchSortDirectionAsc, generated.IdentitySearchSortDirection("asc"))
	assertions.Equal(generated.IdentitySearchSortDirectionDesc, generated.IdentitySearchSortDirection("desc"))
	assertions.Equal(generated.Files, generated.SavedViewStateEnvelopePresentation("files"))
	assertions.Equal(generated.Table, generated.SavedViewStateEnvelopePresentation("table"))
	assertions.Equal(generated.Timeline, generated.SavedViewStateEnvelopePresentation("timeline"))
	assertions.Equal(generated.ExploreFilterDimensionAfter, generated.ExploreFilterDimension("after"))
	assertions.Equal(generated.ExploreGroupSortDirectionAsc, generated.ExploreGroupSortDirection("asc"))
	assertions.Equal(generated.ExploreGroupDimensionSource, generated.ExploreGroupDimension("source"))
	assertions.Equal(generated.ExploreGroupsHTTPRequestSearchModeFullText, generated.ExploreGroupsHTTPRequestSearchMode("full_text"))
}

func TestGeneratedExploreGroupingValidatesExactlyOneDimension(t *testing.T) {
	requirements := require.New(t)
	valid := generated.ExploreGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource,
	}}
	requirements.NoError(valid.Validate())

	requirements.Error((generated.ExploreGroupsHTTPRequest{}).Validate(), "empty grouping")
	requirements.Error((generated.ExploreGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource, generated.ExploreGroupDimensionMonth,
	}}).Validate(), "multiple grouping dimensions")

	fileValid := generated.FileGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource,
	}}
	requirements.NoError(fileValid.Validate())
	requirements.Error((generated.FileGroupsHTTPRequest{}).Validate(), "empty file grouping")
	requirements.Error((generated.FileGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource, generated.ExploreGroupDimensionMonth,
	}}).Validate(), "multiple file grouping dimensions")
}

func TestGeneratedFileMetadataRequiresPresenceButAcceptsEmptyLegacyStrings(t *testing.T) {
	t.Run("metadata response", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.FileMetadataResponse
		requirements.NoError(json.Unmarshal([]byte(
			`{"content_state":"metadata_only","entry_key":"source:1:message:m1","filename":"","mime_type":""}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})

	t.Run("search row", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.FileSearchRow
		requirements.NoError(json.Unmarshal([]byte(
			`{"containing_title":"item","content_state":"metadata_only","entry_key":"message:1","filename":"","key":"file:1","mime_family":"other","mime_type":"","occurred_at":"2026-07-19T12:00:00Z","source_identifier":"archive@example.com","source_type":"synthetic"}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})
}

func TestGeneratedGetAttachmentContentReturnsBinaryBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte{0x00, 0xff, 0x7b, 0x22, 0x6e, 0x6f, 0x74, 0x2d, 0x6a, 0x73, 0x6f, 0x6e}
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method, "method")
		assert.Equal("/api/v1/attachments/"+hash+"/content", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	c, err := generated.NewDefaultClient(server.URL, runtime.WithHTTPClient(httpClientDoer{client: http.DefaultClient}))
	require.NoError(err, "NewDefaultClient")

	got, err := c.GetAttachmentContent(context.Background(), &generated.GetAttachmentContentRequestOptions{
		PathParams: &generated.GetAttachmentContentPath{Hash: hash},
	})
	require.NoError(err, "GetAttachmentContent")
	require.NotNil(got, "response")
	assert.Equal(content, *got, "content")
}

func TestGetAttachmentContentVerifiesRequestedHash(t *testing.T) {
	require := require.New(t)
	want := []byte("public client attachment")
	corrupt := bytes.Clone(want)
	corrupt[0] ^= 0xff
	hash := fmt.Sprintf("%x", sha256.Sum256(want))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(corrupt)
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err)
	options := &generated.GetAttachmentContentRequestOptions{
		PathParams: &generated.GetAttachmentContentPath{Hash: hash},
	}
	_, err = c.GetAttachmentContent(context.Background(), options)
	require.ErrorIs(err, contentverify.ErrMismatch)
	response, err := c.GetAttachmentContentWithResponse(context.Background(), options)
	require.ErrorIs(err, contentverify.ErrMismatch)
	require.NotNil(response)
	assert.Equal(t, corrupt, response.Body)
}

func TestNewCreatesTypedClient(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":3}`))
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL)
	require.NoError(
		err, "New")

	stats, err := client.GetStats(context.Background())
	require.NoError(
		err, "GetStats")

	require.NotNil(stats)
	assert.Equal(t, int64(3), stats.TotalMessages)
}

func TestRunQueryDecodesScalarCells(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n","s","b"],"rows":[[1,"x",true]],"row_count":1}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	got, err := c.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})
	require.NoError(
		err, "RunQuery")

	assert.Equal([]string{"n", "s", "b"}, got.Columns, "columns")
	require.Len(got.Rows, 1, "rows")
	numberCell, ok := got.Rows[0][0].(float64)
	require.True(ok, "number cell type")
	assert.InDelta(1.0, numberCell, 0, "number cell")
	assert.Equal("x", got.Rows[0][1], "string cell")
	assert.Equal(true, got.Rows[0][2], "bool cell")
}

func TestGetMessageRendersLargeIDInPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/24489626", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":24489626,"subject":"Large ID"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	resp, err := c.GetMessageWithResponse(context.Background(), &generated.GetMessageRequestOptions{
		PathParams: &generated.GetMessagePath{ID: 24489626},
	})
	require.NoError(err, "GetMessageWithResponse")
	require.NotNil(resp.JSON200, "JSON200")
	assert.Equal(int64(24489626), resp.JSON200.ID, "id")
}

func TestListMessagesRendersLargeQueryValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages", r.URL.Path, "path")
		assert.Equal("12345678", r.URL.Query().Get("page"), "page query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"page":12345678,"page_size":20,"total":0}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	page := int64(12345678)
	resp, err := c.ListMessagesWithResponse(context.Background(), &generated.ListMessagesRequestOptions{
		Query: &generated.ListMessagesQuery{Page: &page},
	})
	require.NoError(err, "ListMessagesWithResponse")
	require.NotNil(resp.JSON200, "JSON200")
	assert.Equal(int64(12345678), resp.JSON200.Page, "page")
}

func TestAddAccountAcceptsIdempotentOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","message":"account already exists"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	got, err := c.AddAccount(context.Background(), &generated.AddAccountRequestOptions{
		Body: &generated.AddAccountBody{
			Email:    "alice@example.com",
			Enabled:  true,
			Schedule: "0 2 * * *",
		},
	})
	require.NoError(
		err, "AddAccount")

	assert.Equal("ok", got.Status, "status")
	assert.Equal("account already exists", got.Message, "message")
}

func TestStageDeletionAcceptsDryRunOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/deletions", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		// Dry runs return 200, not 201.
		_, _ = w.Write([]byte(`{"dry_run":true,"message_count":3,"sample_gmail_ids":["gm-1","gm-2","gm-3"]}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	sender := "alice@example.com"
	dryRun := true
	got, err := c.StageDeletion(context.Background(), &generated.StageDeletionRequestOptions{
		Body: &generated.StageDeletionBody{
			Filter: &generated.StageDeletionFilter{Sender: &sender},
			DryRun: &dryRun,
		},
	})
	require.NoError(
		err, "StageDeletion dry run")

	assert.True(got.DryRun, "dry_run")
	assert.Equal(int64(3), got.MessageCount, "message_count")
	assert.Len(got.SampleGmailIds, 3, "sample ids")
}
