package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/mistraltest"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func documentJSONFixture(t *testing.T) (context.Context, string, documentsCommandDeps, *store.Store) {
	t.Helper()
	markDaemonCLISubprocessForTest(t)
	settings := config.NewDefaultConfig()
	settings.Data.DataDir = t.TempDir()
	settings.Attachments.Documents.Enabled = true
	settings.Attachments.Documents.RetentionPosture = documentindex.RetentionStandard
	settings.Attachments.Documents.TrainingPosture = documentindex.TrainingOptedOut
	st := testutil.NewTestStore(t)
	deps := documentsCommandDeps{openStore: func(context.Context) (*store.Store, func(), error) { return st, func() {}, nil }}
	return testInvocationContext(t.Context(), settings, invocationOptions{}), writeCommandCapabilityManifest(t, settings.Attachments.Documents.MaxPagesPerDocument), deps, st
}

func TestDocumentJSONPolicyReadAndFingerprintGuardPrecedeMutation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx, manifest, deps, st := documentJSONFixture(t)
	policy := newDocumentsCmd(deps)
	var output bytes.Buffer
	policy.SetOut(&output)
	policy.SetErr(&bytes.Buffer{})
	policy.SetArgs([]string{"policy", "--capabilities=" + manifest, "--json"})
	requirements.NoError(policy.ExecuteContext(ctx))
	var preview map[string]any
	requirements.NoError(json.Unmarshal(output.Bytes(), &preview))
	fingerprint, ok := preview["fingerprint"].(string)
	requirements.True(ok)
	assertions.Len(fingerprint, 64)
	assertions.NotContains(output.String(), manifest)
	assertions.NotContains(output.String(), "api_key_env")
	profileID, ok := preview["profile_id"].(string)
	requirements.True(ok)
	status, err := st.GetDocumentIndexStatus(ctx, profileID)
	requirements.NoError(err)
	assertions.False(status.ProfileExists)
	for _, command := range []string{"consent-mistral", "build", "resume", "retry"} {
		t.Run(command, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			cmd := newDocumentsCmd(deps)
			var receipt bytes.Buffer
			cmd.SetOut(&receipt)
			cmd.SetErr(&bytes.Buffer{})
			args := []string{command, "--capabilities=" + manifest, "--json", "--if-fingerprint=" + strings.Repeat("0", 64)}
			if command == "retry" {
				args = append(args, "--hash="+strings.Repeat("a", 64))
			} else {
				args = append(args, "--yes")
			}
			cmd.SetArgs(args)
			requirements.Error(cmd.ExecuteContext(ctx))
			var failure map[string]any
			requirements.NoError(json.Unmarshal(receipt.Bytes(), &failure))
			assertions.Equal("document_policy_changed", failure["error"])
			status, err := st.GetDocumentIndexStatus(ctx, profileID)
			requirements.NoError(err)
			assertions.False(status.ProfileExists)
		})
	}
	consent := newDocumentsCmd(deps)
	output.Reset()
	consent.SetOut(&output)
	consent.SetErr(&bytes.Buffer{})
	consent.SetArgs([]string{"consent-mistral", "--capabilities=" + manifest, "--json", "--if-fingerprint=" + fingerprint, "--yes"})
	requirements.NoError(consent.ExecuteContext(ctx))
	var receipt map[string]any
	requirements.NoError(json.Unmarshal(output.Bytes(), &receipt))
	assertions.Equal(fingerprint, receipt["fingerprint"])
	assertions.Equal(true, receipt["exact_consent"])
	assertions.NotEmpty(receipt["consented_at"])
}

func TestConfiguredDocumentPolicySnapshotOwnsMutableValues(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	ctx, manifest, _, _ := documentJSONFixture(t)
	state := invocationFromContext(ctx)
	state.cfg.Attachments.Documents.Scope.MessageTypes = []string{"email"}
	snapshot, _, _, profile, err := configuredDocumentProfile(manifest, state)
	requirements.NoError(err)
	state.cfg.Attachments.Documents.Scope.MessageTypes[0] = "chat"
	*state.cfg.Attachments.Documents.Index.Lexical = false
	state.cfg.Attachments.Documents.Model = "changed-after-policy-resolution"
	assertions.Equal([]string{"email"}, snapshot.Scope.MessageTypes)
	assertions.True(*snapshot.Index.Lexical)
	assertions.Equal(profile.Model, snapshot.Model)
}

func TestDocumentChangedHostPolicyOrManifestRefusesBeforeStoreOpen(t *testing.T) {
	for _, change := range []string{"config", "manifest"} {
		t.Run(change, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			ctx, manifest, deps, _ := documentJSONFixture(t)
			state := invocationFromContext(ctx)
			state.cfg.Attachments.Documents.MaxPagesPerDocument = 100
			before, err := configuredDocumentProfileOnly(manifest, state)
			requirements.NoError(err)
			if change == "config" {
				state.cfg.Attachments.Documents.MaxFileBytes--
			} else {
				data, err := os.ReadFile(writeCommandCapabilityManifest(t, 101))
				requirements.NoError(err)
				requirements.NoError(os.WriteFile(manifest, data, 0600))
			}
			after, err := configuredDocumentProfileOnly(manifest, state)
			requirements.NoError(err)
			requirements.NotEqual(before.Fingerprint, after.Fingerprint)
			storeOpened := false
			openStore := deps.openStore
			deps.openStore = func(ctx context.Context) (*store.Store, func(), error) { storeOpened = true; return openStore(ctx) }
			command := newDocumentsCmd(deps)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&bytes.Buffer{})
			command.SetArgs([]string{"build", "--capabilities=" + manifest, "--json", "--if-fingerprint=" + before.Fingerprint, "--yes"})
			requirements.Error(command.ExecuteContext(ctx))
			var failure map[string]any
			requirements.NoError(json.Unmarshal(output.Bytes(), &failure))
			assertions.Equal("document_policy_changed", failure["error"])
			assertions.False(storeOpened)
		})
	}
}

func FuzzDocumentSnapshotPreservesProductionPolicyFingerprint(f *testing.F) {
	for _, scope := range []string{"email", "chat", "", "email\x00", "--yes", "\xff"} {
		f.Add(scope)
	}
	baseline := documentindex.DefaultDocumentsConfig()
	baseline.RetentionPosture = documentindex.RetentionStandard
	baseline.TrainingPosture = documentindex.TrainingOptedOut
	policy, err := baseline.MistralPolicy()
	require.NoError(f, err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	require.NoError(f, err)
	input, err := documentindex.ResolveInputPolicy(&baseline, manifest)
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, scope string) {
		requirements := require.New(t)
		assertions := assert.New(t)
		original := cloneDocumentConfig(&baseline)
		original.Scope.MessageTypes = []string{scope}
		before, originalErr := original.ProfileFingerprint(manifest, input.AllowedMediaTypes)
		snapshot := cloneDocumentConfig(original)
		original.Scope.MessageTypes[0] = "changed-after-resolution"
		*original.Index.Lexical = false
		*original.Index.StoreChunkText = false
		original.Model = "changed-after-resolution"
		after, snapshotErr := snapshot.ProfileFingerprint(manifest, input.AllowedMediaTypes)
		if originalErr != nil {
			assertions.Error(snapshotErr)
			return
		}
		requirements.NoError(snapshotErr)
		assertions.Equal(before, after)
	})
}
