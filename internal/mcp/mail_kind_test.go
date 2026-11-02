package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func mailKindMCPFixture(t *testing.T, ready bool, calls *atomic.Int32) *daemonclient.Client {
	t.Helper()
	cfg := mailkind.Config{Enabled: true, RetentionDeclaration: "synthetic retention"}
	cfg.ApplyDefaults()
	disclosure, err := cfg.Disclosure()
	require.NoError(t, err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/mail/kind/status" {
			data, err := json.Marshal(map[string]any{"enabled": true, "ready": ready, "batch_size": 20,
				"data_fields": mailkind.DataFields, "disclosure": disclosure, "disclosure_fingerprint": fingerprint})
			if !assert.NoError(t, err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(data)
			return
		}
		assert.Equal(t, "/api/v1/mail/kind/classify", r.URL.Path)
		calls.Add(1)
		var request struct {
			DisclosureFingerprint string  `json:"disclosure_fingerprint"`
			Limit                 int64   `json:"limit"`
			MessageIDs            []int64 `json:"message_ids"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, fingerprint, request.DisclosureFingerprint)
		assert.EqualValues(t, 1, request.Limit)
		assert.Equal(t, []int64{7}, request.MessageIDs)
		_, _ = w.Write([]byte(`{"classified":1,"labeled":1,"undecided":0,"errors":0,"requests":1,"warning":"Provider output is advisory and untrusted."}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	return client
}

func TestMailKindMCPConfirmationAndGates(t *testing.T) {
	var calls atomic.Int32
	backend := mailKindMCPFixture(t, true, &calls)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MailKind: backend}
	readOnly := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(t, readOnly, ToolGetMailKindStatus)
	assert.NotContains(t, readOnly, ToolClassifyMailKind)
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), ToolClassifyMailKind)
	opts.AllowMailKind = true
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, false)), ToolClassifyMailKind)
	listed := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(t, listed, ToolClassifyMailKind)
	annotations, ok := listed[ToolClassifyMailKind]["annotations"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, annotations["destructiveHint"])
	assert.Equal(t, true, annotations["openWorldHint"])
	args := map[string]any{"limit": float64(1), "message_ids": []any{float64(7)}}
	declined := confirmedCallTool(t, opts, ToolClassifyMailKind, args, false)
	assert.Equal(t, true, declined["isError"])
	assert.Zero(t, calls.Load())
	accepted := confirmedCallTool(t, opts, ToolClassifyMailKind, args, true, func(message string) {
		assert.Contains(t, message, mailkind.Endpoint)
		assert.Contains(t, message, mailkind.ModelID)
		assert.Contains(t, message, mailkind.DataFields)
		assert.Contains(t, message, "synthetic retention")
		assert.Contains(t, message, "Disclosure fingerprint:")
	})
	assert.NotEqual(t, true, accepted["isError"])
	assert.EqualValues(t, 1, calls.Load())
	assert.NotContains(t, fmt.Sprint(accepted), "synthetic-key")
	opts.DelegatedOnly = true
	assert.NotContains(t, toolsByName(t, rawListTools(t, opts, true)), ToolClassifyMailKind)
}

func TestMailKindMCPBlockedBeforeConfirmation(t *testing.T) {
	var calls atomic.Int32
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MailKind: mailKindMCPFixture(t, false, &calls), AllowMailKind: true}
	result := rawCallTool(t, opts, ToolClassifyMailKind, map[string]any{"limit": float64(1)})
	assert.Equal(t, true, result["isError"])
	assert.Empty(t, result["inputRequests"])
	assert.Zero(t, calls.Load())
}
