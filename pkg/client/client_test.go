package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestNewCreatesTypedClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":3}`))
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL)
	require.NoError(t, err, "New")

	stats, err := client.GetStats(context.Background())
	require.NoError(t, err, "GetStats")
	require.NotNil(t, stats)
	assert.Equal(t, int64(3), stats.TotalMessages)
}

func TestRunQueryDecodesScalarCells(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "method")
		assert.Equal(t, "/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n","s","b"],"rows":[[1,"x",true]],"row_count":1}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err, "New")
	got, err := c.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})
	require.NoError(t, err, "RunQuery")

	assert.Equal(t, []string{"n", "s", "b"}, got.Columns, "columns")
	require.Len(t, got.Rows, 1, "rows")
	numberCell, ok := got.Rows[0][0].(float64)
	require.True(t, ok, "number cell type")
	assert.InDelta(t, 1.0, numberCell, 0, "number cell")
	assert.Equal(t, "x", got.Rows[0][1], "string cell")
	assert.Equal(t, true, got.Rows[0][2], "bool cell")
}

func TestAddAccountAcceptsIdempotentOK(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "method")
		assert.Equal(t, "/api/v1/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","message":"account already exists"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err, "New")
	got, err := c.AddAccount(context.Background(), &generated.AddAccountRequestOptions{
		Body: &generated.AddAccountBody{
			Email:    "alice@example.com",
			Enabled:  true,
			Schedule: "0 2 * * *",
		},
	})
	require.NoError(t, err, "AddAccount")
	assert.Equal(t, "ok", got.Status, "status")
	assert.Equal(t, "account already exists", got.Message, "message")
}
