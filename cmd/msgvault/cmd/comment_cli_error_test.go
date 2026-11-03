package cmd

import (
	"encoding/json/v2"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meta"
)

func TestCommentProviderDiagnosticPreservesGuidance(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure meta.ActionError
	}{
		{"abusive_traffic", meta.ActionError{Code: "rate_limited", Message: "Comment action rejected", AbuseWarning: true}},
		{"ordinary_throttle", meta.ActionError{Code: "rate_limited", Message: "Comment action rejected"}},
		{"permissions", meta.ActionError{Code: "permission_missing", Message: "Comment action rejected", RequiredPermissions: []string{"pages_messaging"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, asJSON := range []bool{false, true} {
				name := "human"
				if asJSON {
					name = "json"
				}
				t.Run(name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					calls := 0
					client := newMCPDaemonClient(t, func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/api/v1/health" {
							assert.NoError(json.MarshalWrite(w, map[string]any{"status": "ok", "api_schema_version": commentActionsMinAPISchemaVersion}))
							return
						}
						assert.Equal(http.MethodPost, r.Method)
						assert.Equal("/api/v1/messages/7/comment-actions", r.URL.Path)
						calls++
						w.WriteHeader(http.StatusTooManyRequests)
						assert.NoError(json.MarshalWrite(w, &tc.failure))
					})
					args := []string{"reply", "7", "--visibility", "private", "--body", "Example reply"}
					if asJSON {
						args = append(args, "--json")
					}
					out, stderr, err := groupedCommentCLI(t, client, args...)
					require.Error(err)
					assert.Empty(out)
					assert.Equal(1, calls)
					if asJSON {
						var diagnostic struct {
							Error struct {
								Code                string   `json:"code"`
								Exit                int      `json:"exit_code"`
								AbuseWarning        bool     `json:"abuse_warning"`
								RequiredPermissions []string `json:"required_permissions"`
							} `json:"error"`
						}
						require.NoError(json.Unmarshal([]byte(stderr), &diagnostic))
						assert.Equal(tc.failure.Code, diagnostic.Error.Code)
						assert.Equal(7, diagnostic.Error.Exit)
						assert.Equal(tc.failure.AbuseWarning, diagnostic.Error.AbuseWarning)
						assert.Equal(tc.failure.RequiredPermissions, diagnostic.Error.RequiredPermissions)
					} else {
						if tc.failure.AbuseWarning {
							assert.Contains(stderr, "Stop sending")
						} else {
							assert.NotContains(stderr, "Stop sending")
						}
						for _, permission := range tc.failure.RequiredPermissions {
							assert.Contains(stderr, permission)
						}
					}
				})
			}
		})
	}
}
