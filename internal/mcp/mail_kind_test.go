package mcp

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/query/querytest"
)

func mailKindMCPFixture(t *testing.T, ready bool, calls *atomic.Int32) *daemonclient.Client {
	assert := Assert.New(t)
	require := Require.New(t)
	t.Helper()
	cfg := mailkind.Config{Enabled: true, RetentionDeclaration: "synthetic retention"}
	cfg.ApplyDefaults()
	disclosure, err := cfg.Disclosure()
	require.NoError(err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/mail/kind/status" {
			data, err := json.Marshal(map[string]any{"enabled": true, "ready": ready, "batch_size": 20,
				"data_fields": mailkind.DataFields, "disclosure": disclosure, "disclosure_fingerprint": fingerprint})
			if !assert.NoError(err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(data)
			return
		}
		assert.Equal("/api/v1/mail/kind/classify", r.URL.Path)
		calls.Add(1)
		var request struct {
			DisclosureFingerprint string  `json:"disclosure_fingerprint"`
			Limit                 int64   `json:"limit"`
			MessageIDs            []int64 `json:"message_ids"`
		}
		if !assert.NoError(json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(fingerprint, request.DisclosureFingerprint)
		assert.EqualValues(1, request.Limit)
		assert.Equal([]int64{7}, request.MessageIDs)
		_, _ = w.Write([]byte(`{"classified":1,"labeled":1,"undecided":0,"errors":0,"requests":1,"warning":"Provider output is advisory and untrusted."}`))
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(err)
	t.Cleanup(func() { assert.NoError(client.Close()) })
	return client
}

func TestMailKindMCPConfirmationAndGates(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	var calls atomic.Int32
	backend := mailKindMCPFixture(t, true, &calls)
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MailKind: backend}
	readOnly := toolsByName(t, rawListTools(t, opts, false))
	assert.Contains(readOnly, ToolGetMailKindStatus)
	assert.NotContains(readOnly, ToolClassifyMailKind)
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), ToolClassifyMailKind)
	opts.AllowMailKind = true
	assert.NotContains(toolsByName(t, rawListTools(t, opts, false)), ToolClassifyMailKind)
	listed := toolsByName(t, rawListTools(t, opts, true))
	require.Contains(listed, ToolClassifyMailKind)
	annotations, ok := listed[ToolClassifyMailKind]["annotations"].(map[string]any)
	require.True(ok)
	assert.Equal(true, annotations["destructiveHint"])
	assert.Equal(true, annotations["openWorldHint"])
	args := map[string]any{"limit": float64(1), "message_ids": []any{float64(7)}}
	declined := confirmedCallTool(t, opts, ToolClassifyMailKind, args, false)
	assert.Equal(true, declined["isError"])
	assert.Zero(calls.Load())
	accepted := confirmedCallTool(t, opts, ToolClassifyMailKind, args, true, func(message string) {
		assert.Contains(message, mailkind.Endpoint)
		assert.Contains(message, mailkind.ModelID)
		assert.Contains(message, mailkind.DataFields)
		assert.Contains(message, "synthetic retention")
		assert.Contains(message, "Disclosure fingerprint:")
	})
	assert.NotEqual(true, accepted["isError"])
	assert.EqualValues(1, calls.Load())
	assert.NotContains(fmt.Sprint(accepted), "synthetic-key")
	opts.DelegatedOnly = true
	assert.NotContains(toolsByName(t, rawListTools(t, opts, true)), ToolClassifyMailKind)
}

func TestMailKindMCPBlockedBeforeConfirmation(t *testing.T) {
	assert := Assert.New(t)
	var calls atomic.Int32
	opts := ServeOptions{Engine: &querytest.MockEngine{}, MailKind: mailKindMCPFixture(t, false, &calls), AllowMailKind: true}
	result := rawCallTool(t, opts, ToolClassifyMailKind, map[string]any{"limit": float64(1)})
	assert.Equal(true, result["isError"])
	assert.Empty(result["inputRequests"])
	assert.Zero(calls.Load())
}
