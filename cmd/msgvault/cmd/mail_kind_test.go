package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/mailkind"
	"go.kenn.io/msgvault/internal/testutil"
)

func runMailKindCLI(ctx context.Context, t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "msgvault", SilenceErrors: true, SilenceUsage: true}
	localFlag := rootCmd.PersistentFlags().Lookup("local")
	savedChanged := localFlag.Changed
	t.Cleanup(func() { localFlag.Changed = savedChanged })
	root.PersistentFlags().AddFlag(localFlag)
	root.AddCommand(newMailCommand())
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(io.Discard)
	root.SetArgs(append([]string{"mail", "kind"}, args...))
	err := root.ExecuteContext(ctx)
	return output.String(), err
}

func TestMailKindCLIThroughDaemon(t *testing.T) {
	st := testutil.NewTestStore(t)
	_, messageID, err := testutil.CreateIndexedSourceMessage(st, "fixture@example.test", "classify-1", "Purchase notice", "Your purchase has been paid.")
	require.NoError(t, err)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "fixture-owner-key"
	cfg.Mail.KindClassification.CredentialEnv = "MSGVAULT_TEST_MAIL_KIND_KEY"
	t.Setenv(cfg.Mail.KindClassification.CredentialEnv, "")
	var providerRequests atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerRequests.Add(1)
		var request struct {
			State mailkind.Packet `json:"state"`
		}
		if err := json.UnmarshalRead(r.Body, &request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		assert.Equal(t, "Your purchase has been paid.", request.State.Excerpt)
		_, _ = w.Write([]byte(`{"model":"jev-1.13.0","answers":{"transactional":{"type":"noul","noul":0.95},"kind":{"type":"choice","choice":"receipt","confidence":0.2,"probabilities":{"order":0,"shipment":0,"receipt":0.9,"invoice":0.1,"flight":0,"lodging":0,"reservation":0,"event_ticket":0,"subscription":0,"other":0}}}}`))
	}))
	t.Cleanup(provider.Close)
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: &storeAPIAdapter{store: st}, Logger: slog.New(slog.DiscardHandler), MailKindEndpoint: provider.URL,
	}).Router())
	t.Cleanup(server.Close)
	ctx := withStoreResolverConfig(t, &config.Config{Remote: config.RemoteConfig{
		URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true,
	}})
	status := func() api.MailKindStatus {
		out, err := runMailKindCLI(ctx, t, "status", "--json")
		require.NoError(t, err)
		var result api.MailKindStatus
		require.NoError(t, json.Unmarshal([]byte(out), &result))
		assert.NotContains(t, out, "fixture-owner-key")
		assert.NotContains(t, out, "synthetic-provider-key")
		return result
	}
	assert.Equal(t, "classification_disabled", status().Blocker)
	cfg.Mail.KindClassification.Enabled = true
	cfg.Mail.KindClassification.RetentionDeclaration = "fixture provider policy"
	missingKey := status()
	assert.Equal(t, "credential_unavailable", missingKey.Blocker)
	require.NotNil(t, missingKey.Disclosure)
	t.Setenv(cfg.Mail.KindClassification.CredentialEnv, "synthetic-provider-key")
	before := status()
	assert.Equal(t, "consent_required", before.Blocker)
	assert.False(t, before.Ready)
	out, err := runMailKindCLI(ctx, t, "consent", before.DisclosureFingerprint, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"changed":true`)
	assert.True(t, status().Ready)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: cfg.Server.APIKey, AllowInsecure: true, HTTPClient: server.Client()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, client.Close()) })
	limit := int64(1)
	result, err := client.ClassifyMailKind(t.Context(), before.DisclosureFingerprint, &limit, []int64{messageID})
	require.NoError(t, err)
	assert.EqualValues(t, 1, result.Labeled)
	assert.EqualValues(t, 1, result.Requests)
	assert.Nil(t, result.Blocker)
	assert.EqualValues(t, 1, providerRequests.Load())
	kind, err := st.MailKindLabelContext(t.Context(), messageID)
	require.NoError(t, err)
	assert.Equal(t, "receipt", kind)
	out, err = runMailKindCLI(ctx, t, "consent", before.DisclosureFingerprint, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"changed":false`)
	cfg.Mail.KindClassification.RetentionDeclaration = "fixture changed provider policy"
	after := status()
	assert.NotEqual(t, before.DisclosureFingerprint, after.DisclosureFingerprint)
	assert.False(t, after.Ready)
	_, err = runMailKindCLI(ctx, t, "consent", before.DisclosureFingerprint)
	require.Error(t, err)
	// Revocation works for an old disclosure even after configuration changes.
	out, err = runMailKindCLI(ctx, t, "revoke", before.DisclosureFingerprint, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"consent_active":false`)
	active, err := st.HasMailKindConsentContext(t.Context(), before.DisclosureFingerprint)
	require.NoError(t, err)
	assert.False(t, active)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/mail/kind/status", nil)
	require.NoError(t, err)
	response, err := server.Client().Do(request)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	assert.Equal(t, http.StatusUnauthorized, response.StatusCode)
}
