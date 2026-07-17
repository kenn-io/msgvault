package daemonclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestBackupFreezeBeginReturnsTokenAndSetsAuthHeader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/backup/freeze/begin", r.URL.Path, "path")
		assert.Equal("secret-key", r.Header.Get("X-Api-Key"), "api key")
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(json.NewEncoder(w).Encode(generated.BackupFreezeBeginResponse{Token: "01hz-fake-token"}), "encode response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", AllowInsecure: true})
	require.NoError(err, "New")

	token, err := c.BackupFreezeBegin(context.Background())
	require.NoError(err, "BackupFreezeBegin")
	assert.Equal("01hz-fake-token", token, "token")
}

func TestBackupFreezeBeginReturnsErrorOnNonOKStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, err := w.Write([]byte(`{"error":"backup_freeze_active","message":"a backup freeze is already active"}`))
		assert.NoError(err, "write response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", AllowInsecure: true})
	require.NoError(err, "New")

	token, err := c.BackupFreezeBegin(context.Background())
	assert.Empty(token, "token")
	require.Error(err, "BackupFreezeBegin should fail on 409")
	assert.Contains(err.Error(), "already active", "error message")
}

func TestBackupFreezeEndSendsTokenAndSetsAuthHeader(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	var gotBody generated.BackupFreezeEndRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/backup/freeze/end", r.URL.Path, "path")
		assert.Equal("secret-key", r.Header.Get("X-Api-Key"), "api key")
		assert.NoError(json.NewDecoder(r.Body).Decode(&gotBody), "decode request body")
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{}`))
		assert.NoError(err, "write response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", AllowInsecure: true})
	require.NoError(err, "New")

	err = c.BackupFreezeEnd(context.Background(), "01hz-fake-token")
	require.NoError(err, "BackupFreezeEnd")
	assert.Equal("01hz-fake-token", gotBody.Token, "request token")
}

func TestBackupFreezeEndReturnsErrorOnNonOKStatus(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"error":"backup_freeze_not_active","message":"no active backup freeze with that token"}`))
		assert.NoError(err, "write response")
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{URL: srv.URL, APIKey: "secret-key", AllowInsecure: true})
	require.NoError(err, "New")

	err = c.BackupFreezeEnd(context.Background(), "bogus-token")
	require.Error(err, "BackupFreezeEnd should fail on 400")
	assert.Contains(err.Error(), "no active backup freeze", "error message")
}
