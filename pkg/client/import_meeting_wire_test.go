package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/pkg/client/generated"
)

// Native daemon/SDK tests exercise import persistence separately. This local
// HTTP endpoint proves the generated public client's actual wire contract:
// regeneration must not silently discard opt-in or the returned logical ID.
func TestMeetingImportClientCarriesOptInAndLogicalRunOnWire(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(map[bool]string{false: "default-off", true: "explicit-opt-in"}[notify], func(t *testing.T) {
			require := Require.New(t)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Assert.Equal(t, http.MethodPost, r.Method)
				Assert.Equal(t, "/api/v1/import/meeting", r.URL.Path)
				var request map[string]any
				if !Assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
					http.Error(w, "invalid synthetic request", http.StatusBadRequest)
					return
				}
				if notify {
					Assert.Equal(t, true, request["notify_imports"])
				} else {
					value, present := request["notify_imports"]
					Assert.True(t, !present || value == false)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"message_id":1,"source_id":1,"source_message_id":"meeting:synthetic","status":"created","import_run_id":"import_synthetic-wire"}`))
			}))
			defer server.Close()
			apiClient, err := New(server.URL)
			require.NoError(err)
			input := `{"source":{"identifier":"synthetic","account_email":"owner@example.net"},"meeting":{"external_id":"synthetic","started_at":"2001-01-01T12:00:00Z","summary_text":"Synthetic meeting"}}`
			if notify {
				input = `{"notify_imports":true,` + input[1:]
			}
			var body generated.ImportMeetingBody
			require.NoError(json.Unmarshal([]byte(input), &body))
			response, err := apiClient.ImportMeeting(t.Context(), &generated.ImportMeetingRequestOptions{Body: &body})
			require.NoError(err)
			require.NotNil(response)
			wire, err := json.Marshal(response)
			require.NoError(err)
			var decoded map[string]any
			require.NoError(json.Unmarshal(wire, &decoded))
			Assert.Equal(t, "import_synthetic-wire", decoded["import_run_id"])
		})
	}
}
