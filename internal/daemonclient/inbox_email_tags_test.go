package daemonclient_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// This peer proves the stable legacy-to-signed HTTP contract, not native Gmail
// acceptance; the daemon native test separately uses the real Gmail transport.
func TestInboxMessageTagsLegacyGmailPreservesReceipt(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

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
			require.NoError(json.NewDecoder(r.Body).Decode(&request))
			controls = append(controls, request)
			require.Equal(inboxcontrol.OpTags, request.Operation)
			require.Equal(target, *request.Target)
			after := state
			after.Tags = []string{"INBOX", "Label_next"}
			if request.DryRun {
				_ = json.NewEncoder(w).Encode(inboxcontrol.Result{Before: &state, Projected: &after, PreviewToken: "signed-preview"})
				return
			}
			require.Equal("signed-preview", request.PreviewToken)
			require.NotEmpty(request.IdempotencyKey)
			require.NotNil(request.Expected)
			_ = json.NewEncoder(w).Encode(inboxcontrol.Result{After: &after, Receipt: &inboxcontrol.Receipt{ID: "native-receipt", Status: inboxcontrol.StatusVerified}})
		default:
			assert.Fail("unexpected request", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true})
	require.NoError(err)
	defer client.Close()
	result, err := client.MessageTags(t.Context(), 7, &emailtags.MessageTagChange{Add: []string{"Label_next"}}, "")
	require.NoError(err)
	require.NotNil(result)
	assert.True(result.Verified)
	assert.Equal("native-receipt", result.ReceiptID)
	assert.Equal("verified", result.ReceiptStatus)
	assert.NotEmpty(result.IdempotencyKey)
	assert.Equal([]string{"INBOX", "Label_next"}, result.Tags)
	assert.Equal(1, legacyWrites)
	assert.Len(controls, 2)
}
