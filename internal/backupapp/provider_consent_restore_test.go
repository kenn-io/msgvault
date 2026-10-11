package backupapp_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/backup"
	"go.kenn.io/msgvault/internal/backupapp"
	"go.kenn.io/msgvault/internal/store"
)

// TestProviderConsentSurvivesRestore guards the restore contract that only
// document-vector authority is invalidated; people consent stays granted.
func TestProviderConsentSurvivesRestore(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	ctx := t.Context()
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "msgvault.db")
	attachmentsDir := filepath.Join(dataDir, "attachments")
	require.NoError(os.MkdirAll(attachmentsDir, 0o700))

	st, err := store.OpenForTest(dbPath)
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	fingerprint := strings.Repeat("d", 64)
	for _, statement := range []string{
		`INSERT INTO person_inference_profiles
			(fingerprint, provider_kind, endpoint, model, api_key_env,
			 retention_posture, training_posture, allowed_sources, source_since, policy_json)
		 VALUES (?, 'openai_compatible', 'https://api.example.test/v1', 'gpt-test', 'TEST_KEY',
			 'zero_retention', 'no_training', '["conversation_text"]', '2025-01-01', '{}')`,
		`INSERT INTO person_enrichment_profiles
			(fingerprint, provider_name, provider_kind, provider_namespace, endpoint, api_key_env, policy_json)
		 VALUES (?, 'exa-primary', 'exa', 'https://api.example.test', 'https://api.example.test/search',
			 'PROVIDER_API_KEY', '{}')`,
		`INSERT INTO person_semantic_embedding_profiles
			(fingerprint, purpose, destination, api_format, model, api_key_env,
			 retention_posture, training_posture, renderer_policy,
			 disclosed_field_classes, corpus_scope, policy_json)
		 VALUES (?, 'semantic_person_embeddings', 'https://embedding.example.test/v1/embeddings',
			 'openai', 'synthetic-model', 'TEST_KEY', 'zero_data_retention', 'no_training',
			 'person-semantic-v1', '["person_display_name"]', 'all_durable_people', '{}')`,
	} {
		_, err := st.DB().ExecContext(ctx, statement, fingerprint)
		require.NoError(err)
	}
	inference, _, err := st.GrantPersonInferenceConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	enrichment, _, err := st.GrantPersonEnrichmentConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	semantic, _, err := st.GrantPersonSemanticEmbeddingConsent(ctx, fingerprint, "cli")
	require.NoError(err)
	require.NoError(st.Close())

	repository, err := backup.Init(filepath.Join(t.TempDir(), "repository"))
	require.NoError(err)
	app := backupapp.New("test")
	_, err = backup.Create(ctx, repository, app, backup.CreateOptions{
		DBPath: dbPath, ContentDir: attachmentsDir, DataDir: dataDir,
	})
	require.NoError(err)
	restoreResult, err := backup.Restore(ctx, repository, app, backup.RestoreOptions{
		TargetDir:         filepath.Join(t.TempDir(), "restored"),
		AuxiliaryTarget:   backupapp.NewDocumentAuxiliaryTarget(),
		BeforePublication: backupapp.InvalidateRestoredDocumentVectors,
	})
	require.NoError(err)
	restored, err := store.OpenForTest(restoreResult.DBPath)
	require.NoError(err)
	t.Cleanup(func() { _ = restored.Close() })

	inferenceStatus, err := restored.GetPersonInferenceConsentStatus(ctx, fingerprint)
	require.NoError(err)
	enrichmentStatus, err := restored.PersonEnrichmentConsentStatus(ctx, fingerprint)
	require.NoError(err)
	semanticStatus, err := restored.GetPersonSemanticEmbeddingConsentStatus(ctx, fingerprint)
	require.NoError(err)
	for _, check := range []struct {
		name   string
		want   *store.ProviderConsent
		status *store.ProviderConsentStatus
	}{
		{"inference", inference, inferenceStatus},
		{"enrichment", enrichment, enrichmentStatus},
		{"semantic", semantic, semanticStatus},
	} {
		assert.True(check.status.Active, check.name)
		require.NotNil(check.status.Consent, check.name)
		assert.Equal(check.want.ID, check.status.Consent.ID, check.name)
	}
}
