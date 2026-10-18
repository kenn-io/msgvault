package client_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestCreateLoopSuppressionReturnsDryRunPreview(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		dryRun      bool
		wantClosing int64
		wantMuted   bool
	}{
		{name: "dry run", status: http.StatusOK, body: `{"would_close":3,"suppression":null}`, dryRun: true, wantClosing: 3},
		{name: "created", status: http.StatusCreated, body: `{"would_close":0,"suppression":{"id":5,"target_kind":"person","created_at":"2026-10-17T12:00:00Z"}}`, wantMuted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertions.Equal(http.MethodPost, r.Method)
				assertions.Equal("/api/v1/loops/suppressions", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assertions.NoError(err)
			}))
			t.Cleanup(server.Close)

			apiClient, err := client.New(server.URL)
			requirements.NoError(err)
			personID := int64(7)
			dryRun := tc.dryRun
			result, err := apiClient.CreateLoopSuppression(t.Context(), &generated.CreateLoopSuppressionRequestOptions{
				Body: &generated.CreateLoopSuppressionBody{
					TargetKind: "person",
					PersonID:   &personID,
					DryRun:     &dryRun,
				},
			})
			requirements.NoError(err)
			requirements.NotNil(result)
			assertions.Equal(tc.wantClosing, result.WouldClose)
			assertions.Equal(tc.wantMuted, result.Suppression != nil)
		})
	}
}
