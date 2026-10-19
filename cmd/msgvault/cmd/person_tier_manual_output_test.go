package cmd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cmd "go.kenn.io/msgvault/cmd/msgvault/cmd"
)

func TestPersonManualTierOutputsShowGrant(t *testing.T) {
	for _, tc := range []struct {
		action, body string
		want         []string
	}{
		{"track", `{"person_id":7,"tracked":true,"provenance":"manual","actor":"synthetic-actor","tracked_at":"2026-10-17T12:00:00Z"}`, []string{"tracked", "provenance", "manual", "synthetic-actor"}},
		{"track", `{"person_id":7,"tracked":true,"tracked_at":"2026-10-17T12:00:00Z"}`, []string{"manual", "unknown (legacy row)"}},
		{"enroll", `{"person_id":7,"enrolled":true,"provenance":"manual","actor":"synthetic-actor","enabled_at":"2026-10-17T12:00:00Z"}`, []string{"enrolled", "provenance", "manual", "synthetic-actor"}},
	} {
		t.Run(tc.action+tc.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.24.0"}`))
					return
				}
				assert.Equal(t, http.MethodPut, r.Method)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			command := cmd.PersonManualGrantCommandForTest(t.Context(), server.URL, tc.action)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"7"})
			require.NoError(t, command.Execute())
			for _, value := range tc.want {
				assert.Contains(t, output.String(), value)
			}
		})
	}
}

func TestPersonLegacyManualRemovalDoesNotClaimNewOptOutSideEffect(t *testing.T) {
	for _, tc := range []struct{ action, body string }{
		{"untrack", `{"person_id":7,"tracked":false,"tracked_at":null}`},
		{"unenroll", `{"person_id":7,"enrolled":false,"enabled_at":null,"actor":""}`},
	} {
		t.Run(tc.action, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/health" {
					_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.23.0"}`))
					return
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			command := cmd.PersonManualGrantCommandForTest(t.Context(), server.URL, tc.action)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"7"})
			require.NoError(t, command.Execute())
			assert.NotContains(t, output.String(), "recorded opt-out")
			assert.NotContains(t, output.String(), "person tiers opt-in")
		})
	}
}
