package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
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
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Server.APIKey = "fixture-owner-key"
	cfg.Mail.KindClassification.CredentialEnv = "MSGVAULT_TEST_MAIL_KIND_KEY"
	t.Setenv(cfg.Mail.KindClassification.CredentialEnv, "")
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: cfg, Store: st, Logger: slog.New(slog.DiscardHandler),
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
