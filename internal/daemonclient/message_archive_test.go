package daemonclient_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
)

func TestArchiveMessageClientNeverReplaysUnknownOutcome(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/messages/7/archive", r.URL.Path)
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	archived := true
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived})
	var failure *emailtags.MessageTagError
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, "remote_unknown", failure.Code)
	assert.Equal(t, int64(1), calls.Load())
	_, err = client.ArchiveMessage(t.Context(), 7, emailtags.ArchiveChange{Archived: &archived, DryRun: true})
	require.Error(t, err)
	assert.False(t, errors.As(err, &failure), "preview failures must not claim a provider write happened")
	assert.Equal(t, int64(2), calls.Load())
}
