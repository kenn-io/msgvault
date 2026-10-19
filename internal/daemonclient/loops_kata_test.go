package daemonclient_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/loops"
)

func TestTrackOpenLoopKeepsConflictReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/loops/12/kata", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"loop_already_tracked","message":"already tracked","kata":{"issue_uid":"01SYNTHETIC","qualified_ref":"example#abcd","status":"open","title":"Reply to Example Person"}}`))
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, Context: t.Context()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	_, err = client.TrackOpenLoop(t.Context(), 12, loops.TrackRequest{})
	var conflict *daemonclient.LoopAlreadyTrackedError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, "example#abcd", conflict.Kata.QualifiedRef)
	assert.Contains(t, err.Error(), "example#abcd")
}
