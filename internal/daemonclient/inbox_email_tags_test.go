package daemonclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This peer proves the stable legacy-to-signed HTTP contract, not native Gmail
// acceptance; the daemon native test separately uses the real Gmail transport.
func TestInboxMessageTagsLegacyGmailPreservesReceipt(t *testing.T) {
	assertions := assert.New(t)

	requirements := require.New(t)

	target := inboxcontrol.Target{SourceID: 2, SourceType: "gmail", SourceIdentifier: "owner@example.test", AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: 7, ProviderID: "native-7"}
	state := inboxcontrol.State{Target: target, Tags: []string{"INBOX"}, ObservedAt: time.Now().UTC()}
	controls := []inboxcontrol.Request{}
	legacyWrites := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/health":
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.15.0"}`))
		case "/api/v1/mcp/capabilities":
			_, _ = w.Write([]byte(syntheticInboxDescriptor))
		case "/api/v1/messages/7/tags":
			if r.Method == http.MethodPost {
				legacyWrites++
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"inbox_preview_required","message":"Use signed control"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"message_id": 7, "source_id": 2, "provider": "gmail", "tags": state.Tags, "before": state.Tags, "available_tags": []emailtags.MessageTag{{ID: "Label_next", Name: "Next"}}, "verified": true, "target": target})
		case "/api/v1/inbox/control":
			var request inboxcontrol.Request
			if !assertions.NoError(json.NewDecoder(r.Body).Decode(&request)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			controls = append(controls, request)
			assertions.Equal(inboxcontrol.OpTags, request.Operation)
			if !assertions.NotNil(request.Target) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			assertions.Equal(target, *request.Target)
			after := state
			after.Tags = []string{"INBOX", "Label_next"}
			if request.DryRun {
				_ = json.NewEncoder(w).Encode(inboxcontrol.Result{Before: &state, Projected: &after, PreviewToken: "signed-preview"})
				return
			}
			assertions.Equal("signed-preview", request.PreviewToken)
			assertions.NotEmpty(request.IdempotencyKey)
			assertions.NotNil(request.Expected)
			_ = json.NewEncoder(w).Encode(inboxcontrol.Result{After: &after, Receipt: &inboxcontrol.Receipt{ID: "native-receipt", Status: inboxcontrol.StatusVerified}})
		default:
			assertions.Fail("unexpected request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	defer func() { assertions.NoError(client.Close()) }()
	result, err := client.MessageTags(t.Context(), 7, &emailtags.MessageTagChange{Add: []string{"Label_next"}}, "")
	requirements.NoError(err)
	requirements.NotNil(result)
	assertions.True(result.Verified)
	assertions.Equal("native-receipt", result.ReceiptID)
	assertions.Equal("verified", result.ReceiptStatus)
	assertions.NotEmpty(result.IdempotencyKey)
	assertions.Equal([]string{"INBOX", "Label_next"}, result.Tags)
	assertions.Equal(1, legacyWrites)
	assertions.Len(controls, 2)
}
