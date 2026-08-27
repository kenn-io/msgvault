package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDocumentAttachmentConfig(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := []byte(`
[vector]
enabled = true

[vector.embeddings]
endpoint = "http://127.0.0.1:8080/v1"
model = "test-embedding-model"
dimension = 768

[attachments.documents]
enabled = true
provider = "mistral"
region = "eu"
api_key_env = "PRIVATE_MISTRAL_KEY"
model = "mistral-ocr-4-0"
retention_posture = "zdr"
training_posture = "opted-out"
max_file_bytes = 1048576
max_pages_per_document = 20
max_response_bytes = 2097152
max_normalized_chars = 100000
max_spool_bytes = 8388608
min_free_space_bytes = 4194304
request_timeout = "2m"
max_retries = 2
max_pages_per_run = 100
max_estimated_cost_usd_per_run = 3.5
estimated_cost_usd_per_1000_units = 4.25
pricing_assumption_on = "2026-08-13"

[attachments.documents.scope]
message_types = ["EMAIL", "chat", "email"]

[attachments.documents.index]
lexical = true
store_chunk_text = true

[attachments.documents.index.embeddings]
enabled = true
profile = "vector.embeddings"
`)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	loaded, err := Load(path, "")
	require.NoError(t, err)
	documents := loaded.Attachments.Documents
	assert.True(documents.Enabled)
	assert.Equal("PRIVATE_MISTRAL_KEY", documents.APIKeyEnv)
	assert.Equal(2*time.Minute, documents.RequestTimeout)
	assert.Equal(int64(8388608), documents.MaxSpoolBytes)
	assert.Equal(int64(4194304), documents.MinFreeSpaceBytes)
	assert.InDelta(4.25, documents.EstimatedCostUSDPerKUnits, 0.0001)
	assert.Equal([]string{"chat", "email"}, documents.Scope.MessageTypes)
	assert.True(documents.LexicalEnabled())
	assert.True(documents.StoresChunkText())
	assert.True(documents.Index.Embeddings.Enabled)
	assert.Equal("vector.embeddings", documents.Index.Embeddings.Profile)
}

func TestLoadRejectsUnknownDocumentEmbeddingProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	content := []byte(`
[attachments.documents.index.embeddings]
enabled = true
profile = "other.embeddings"
`)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(t, err, "profile must be \"vector.embeddings\"")
}

func TestLoadRejectsDocumentEmbeddingsWithoutVectorLane(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[attachments.documents.index.embeddings]
enabled = true
`), 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(t, err, "requires [vector] enabled = true")
}

func TestLoadDefaultsOmittedEnabledDocumentEmbeddingProfile(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`
[vector]
enabled = true

[vector.embeddings]
endpoint = "http://127.0.0.1:8080/v1"
model = "test-embedding-model"
dimension = 768

[attachments.documents.index.embeddings]
enabled = true
`), 0o600))

	loaded, err := Load(path, "")
	requirements.NoError(err)
	assertions.True(loaded.Attachments.Documents.Index.Embeddings.Enabled)
	assertions.Equal("vector.embeddings", loaded.Attachments.Documents.Index.Embeddings.Profile)
}

func TestLoadRejectsInvalidDocumentAttachmentConfigWithoutResolvingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[attachments.documents]
enabled = false
region = "untrusted"
api_key_env = "MISSING_KEY_IS_NOT_READ"
`), 0o600))

	_, err := Load(path, "")
	assert.ErrorContains(t, err, "unknown region")
}

func TestLoadRejectsExplicitZeroDocumentLimits(t *testing.T) {
	for _, field := range []string{
		"max_file_bytes = 0",
		"max_pages_per_document = 0",
		"max_estimated_cost_usd_per_run = 0",
	} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			content := "[attachments.documents]\n" + field + "\n"
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

			_, err := Load(path, "")
			assert.ErrorContains(t, err, "must be")
		})
	}
}
