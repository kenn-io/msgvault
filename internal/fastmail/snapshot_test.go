package fastmail

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotAtScaleRequestsOnlyMetadata(t *testing.T) {
	states := []string{"enabled", "disabled", "deleted", "pending"}
	srv := newTestServer(t, func(baseURL string) sessionResponse {
		return sessionResponse{APIURL: baseURL + "/jmap", Capabilities: capabilitySet(CoreCapability, MaskedEmailCapability), Accounts: map[string]sessionAccount{"account": {AccountCapabilities: capabilitySet(MaskedEmailCapability)}}, PrimaryAccounts: map[string]string{MaskedEmailCapability: "account"}}
	}, func(w http.ResponseWriter, r *http.Request) {
		var req jmapRequest
		if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&req)) {
			return
		}
		if !assert.Len(t, req.MethodCalls, 1) {
			return
		}
		var args struct {
			Properties []string `json:"properties"`
		}
		if !assert.NoError(t, json.Unmarshal(req.MethodCalls[0][1], &args)) {
			return
		}
		assert.ElementsMatch(t, []string{"id", "email", "state", "forDomain", "description", "createdAt", "lastMessageAt"}, args.Properties)
		list := make([]any, 3000)
		for i := range list {
			list[i] = map[string]any{"id": strconv.Itoa(i), "email": fmt.Sprintf("mask-%04d@example.test", i), "state": states[i%4], "forDomain": "https://example.com", "description": "Synthetic mask", "createdAt": "2026-01-01T00:00:00Z", "lastMessageAt": nil}
		}
		assert.NoError(t, writeJSON(w, map[string]any{"methodResponses": []any{[]any{"MaskedEmail/get", map[string]any{"accountId": "account", "state": "revision-1", "list": list}, "masked"}}}))
	})
	assert := assert.New(t)
	require := require.New(t)
	client := newClient(testToken, srv.Client(), srv.URL+"/session")
	snapshot, err := client.ListIdentitySnapshot(t.Context())
	require.NoError(err)
	require.Len(snapshot.Records, 3000)
	assert.NotEmpty(snapshot.State)
	assert.Equal("0", snapshot.Records[0].ID)
	assert.Equal("https://example.com", snapshot.Records[0].ForDomain)
	assert.Equal("Synthetic mask", snapshot.Records[0].Description)
	assert.Equal("2026-01-01T00:00:00Z", snapshot.Records[0].CreatedAt)
	assert.Empty(snapshot.Records[0].LastMessageAt)
}

func TestSnapshotRejectsOversizedListsWithoutServerError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capability string
		method     string
		callID     string
		advertised int64
		wantLimit  int64
	}{
		{name: "masked/advertised", capability: MaskedEmailCapability, method: "MaskedEmail/get", callID: "masked", advertised: 2, wantLimit: 2},
		{name: "masked/fallback", capability: MaskedEmailCapability, method: "MaskedEmail/get", callID: "masked", wantLimit: 4096},
		{name: "identity/advertised", capability: SubmissionCapability, method: "Identity/get", callID: "identity", advertised: 2, wantLimit: 2},
		{name: "identity/fallback", capability: SubmissionCapability, method: "Identity/get", callID: "identity", wantLimit: 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, func(baseURL string) sessionResponse {
				capabilities := capabilitySet(CoreCapability, MaskedEmailCapability, tc.capability)
				if tc.advertised != 0 {
					capabilities[CoreCapability] = []byte(fmt.Sprintf(`{"maxObjectsInGet":%d}`, tc.advertised))
				}
				return sessionResponse{APIURL: baseURL + "/jmap", Capabilities: capabilities, Accounts: map[string]sessionAccount{"account": {AccountCapabilities: capabilitySet(MaskedEmailCapability, tc.capability)}}, PrimaryAccounts: map[string]string{MaskedEmailCapability: "account", tc.capability: "account"}}
			}, func(w http.ResponseWriter, _ *http.Request) {
				list := make([]any, tc.wantLimit+1)
				for i := range list {
					list[i] = map[string]any{"id": strconv.Itoa(i), "email": fmt.Sprintf("mask-%04d@example.test", i), "state": "enabled"}
				}
				// Serve a successful /get response rather than requestTooLarge.
				responses := []any{[]any{tc.method, map[string]any{"accountId": "account", "state": "one", "list": list}, tc.callID}}
				if tc.capability == SubmissionCapability {
					responses = append(responses, []any{"MaskedEmail/get", map[string]any{"accountId": "account", "state": "one", "list": []any{}}, "masked"})
				}
				assert.NoError(t, writeJSON(w, map[string]any{"methodResponses": responses}))
			})
			client := newClient(testToken, srv.Client(), srv.URL+"/session")
			_, err := client.ListIdentitySnapshot(t.Context())
			var limitErr *ObjectLimitError
			require.ErrorAs(t, err, &limitErr)
			assert.Equal(t, tc.wantLimit, limitErr.MaxObjectsInGet)
			assert.Equal(t, tc.method, limitErr.Method)
		})
	}
}
