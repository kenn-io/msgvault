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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/loops/suppressions", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)

			apiClient, err := client.New(server.URL)
			require.NoError(t, err)
			personID := int64(7)
			dryRun := tc.dryRun
			result, err := apiClient.CreateLoopSuppression(t.Context(), &generated.CreateLoopSuppressionRequestOptions{
				Body: &generated.CreateLoopSuppressionBody{
					TargetKind: "person",
					PersonID:   &personID,
					DryRun:     &dryRun,
				},
			})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, tc.wantClosing, result.WouldClose)
			assert.Equal(t, tc.wantMuted, result.Suppression != nil)
		})
	}
}

func TestGeneratedCreateLoopSuppressionAcceptsBothSuccessStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "dry run", status: http.StatusOK, body: `{"would_close":3,"suppression":null}`},
		{name: "created", status: http.StatusCreated, body: `{"would_close":0,"suppression":{"id":5,"target_kind":"person","created_at":"2026-10-17T12:00:00Z"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, "/api/v1/loops/suppressions", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, err := w.Write([]byte(tc.body))
				assert.NoError(t, err)
			}))
			t.Cleanup(server.Close)

			apiClient, err := client.New(server.URL)
			require.NoError(t, err)
			personID := int64(7)
			dryRun := tc.status == http.StatusOK
			result, err := apiClient.Client.CreateLoopSuppression(t.Context(), &generated.CreateLoopSuppressionRequestOptions{
				Body: &generated.CreateLoopSuppressionBody{
					TargetKind: "person",
					PersonID:   &personID,
					DryRun:     &dryRun,
				},
			})
			require.NoError(t, err)
			require.NotNil(t, result)
			if tc.status == http.StatusOK {
				assert.Equal(t, int64(3), result.WouldClose)
				assert.Nil(t, result.Suppression)
			} else {
				assert.Equal(t, int64(0), result.WouldClose)
				require.NotNil(t, result.Suppression)
				assert.Equal(t, int64(5), result.Suppression.ID)
			}
		})
	}
}
