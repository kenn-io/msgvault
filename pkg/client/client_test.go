package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

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
