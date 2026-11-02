package api

import (
	"bytes"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMailKindRunThroughGeneratedClient(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, id, err := testutil.CreateIndexedSourceMessage(st, "fixture@example.test", "classify-1", "Purchase notice", "Your purchase has been paid.")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Mail.KindClassification.Enabled = true
	cfg.Mail.KindClassification.CredentialEnv = "MSGVAULT_TEST_MAIL_KIND_API_KEY"
	cfg.Mail.KindClassification.RetentionDeclaration = "synthetic provider policy"
	t.Setenv(cfg.Mail.KindClassification.CredentialEnv, "synthetic-key")
	disclosure, err := cfg.Mail.KindClassification.Disclosure()
	require.NoError(t, err)
	fingerprint, err := disclosure.Fingerprint()
	require.NoError(t, err)
	_, _, err = st.GrantMailKindConsentContext(t.Context(), disclosure, "fixture", nil)
	require.NoError(t, err)
	var requests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var request struct {
			State mailkind.Packet `json:"state"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "Your purchase has been paid.", request.State.Excerpt)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"transactional":{"type":"noul","noul":0.95},"kind":{"type":"choice","choice":"receipt","confidence":0.2,"probabilities":{"order":0,"shipment":0,"receipt":0.9,"invoice":0.1,"flight":0,"lodging":0,"reservation":0,"event_ticket":0,"subscription":0,"other":0}}}}`))
	}))
	defer provider.Close()
	srv := NewServer(cfg, st, nil, slog.New(slog.DiscardHandler))
	srv.mailKindEndpoint = provider.URL
	server := httptest.NewServer(srv.Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	defer func() { assert.NoError(t, client.Close()) }()
	limit := int64(1)
	_, err = client.ClassifyMailKind(t.Context(), "stale-fingerprint", &limit, []int64{id})
	require.Error(t, err)
	assert.Zero(t, requests.Load())
	// Empty or null explicit IDs must not turn into an automatic candidate batch.
	for _, messageIDs := range []any{[]int64{}, nil} {
		body, err := json.Marshal(map[string]any{"disclosure_fingerprint": fingerprint, "message_ids": messageIDs})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/api/v1/mail/kind/classify", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := server.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		assert.Equal(t, http.StatusBadRequest, response.StatusCode)
		assert.Zero(t, requests.Load())
	}
	result, err := client.ClassifyMailKind(t.Context(), fingerprint, &limit, []int64{id})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.Labeled)
	assert.EqualValues(t, 1, result.Requests)
	assert.Contains(t, result.Warning, "untrusted")
	assert.Nil(t, result.Blocker)
	assert.EqualValues(t, 1, requests.Load())
	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "Purchase notice")
	assert.NotContains(t, string(encoded), "synthetic-key")
	assert.NotContains(t, string(encoded), "message_id")
}
