package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeeperDocbankConfig(t *testing.T) {
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"
api_key_env = "DOCBANK_TEST_KEY"
all_sources_upload_consent = true
asr_profile = "voice-asr"
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	cfg, err := Load(path, "")
	require.NoError(t, err)
	assert.Equal(t, DocbankIntegrationConfig{
		Enabled: true, URL: "http://127.0.0.1:8080",
		APIKeyEnv: "DOCBANK_TEST_KEY", AllSourcesUploadConsent: true, ASRProfile: "voice-asr",
	}, cfg.Integrations.Docbank)
}

func TestBeeperDocbankConfigRejectsReservedSuppliedTranscriptProfile(t *testing.T) {
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"
api_key_env = "DOCBANK_TEST_KEY"
all_sources_upload_consent = true
asr_profile = " supplied-transcript "
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	_, err := Load(path, "")
	require.ErrorContains(t, err, "reserved for supplied transcript input")
}

func TestBeeperDocbankConfigDefaultsDisabled(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	t.Setenv("MSGVAULT_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(os.WriteFile(path, []byte(""), 0o600))

	cfg, err := Load(path, "")
	require.NoError(err)
	assert.False(cfg.Integrations.Docbank.Enabled)
	assert.False(cfg.Integrations.Docbank.AllSourcesUploadConsent)
}

func TestDocbankAttachmentMirrorConfigValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		config     DocbankIntegrationConfig
		collection string
		wantError  string
	}{
		{name: "defaults", collection: "/msgvault"},
		{name: "root collection", config: DocbankIntegrationConfig{AttachmentCollection: "/"}, collection: "/"},
		{name: "documented settings", config: DocbankIntegrationConfig{
			Enabled: true, AttachmentMirror: true, AttachmentUploadConsent: true,
			AttachmentCollection: "/msgvault/files", AttachmentMaxBytes: 104857600,
			AttachmentAfter: "2026-01-01", AttachmentSourceIDs: []int64{1, 2},
			AttachmentMIMEClasses: []string{"application", "image"},
		}, collection: "/msgvault/files"},
		{name: "relative collection", config: DocbankIntegrationConfig{AttachmentCollection: "msgvault"}, wantError: "attachment_collection"},
		{name: "unclean collection", config: DocbankIntegrationConfig{AttachmentCollection: "/msgvault/../files"}, wantError: "attachment_collection"},
		{name: "trailing separator", config: DocbankIntegrationConfig{AttachmentCollection: "/msgvault/"}, wantError: "attachment_collection"},
		{name: "negative size", config: DocbankIntegrationConfig{AttachmentMaxBytes: -1}, wantError: "attachment_max_bytes"},
		{name: "malformed date", config: DocbankIntegrationConfig{AttachmentAfter: "2026-1-1"}, wantError: "attachment_after"},
		{name: "impossible date", config: DocbankIntegrationConfig{AttachmentAfter: "2026-02-30"}, wantError: "attachment_after"},
		{name: "zero source", config: DocbankIntegrationConfig{AttachmentSourceIDs: []int64{0}}, wantError: "attachment_source_ids"},
		{name: "negative source", config: DocbankIntegrationConfig{AttachmentSourceIDs: []int64{-1}}, wantError: "attachment_source_ids"},
		{name: "empty MIME class", config: DocbankIntegrationConfig{AttachmentMIMEClasses: []string{""}}, wantError: "attachment_mime_classes"},
		{name: "uppercase MIME class", config: DocbankIntegrationConfig{AttachmentMIMEClasses: []string{"Image"}}, wantError: "attachment_mime_classes"},
		{name: "full MIME type", config: DocbankIntegrationConfig{AttachmentMIMEClasses: []string{"image/png"}}, wantError: "attachment_mime_classes"},
		{name: "MIME whitespace", config: DocbankIntegrationConfig{AttachmentMIMEClasses: []string{" image"}}, wantError: "attachment_mime_classes"},
		{name: "MIME punctuation", config: DocbankIntegrationConfig{AttachmentMIMEClasses: []string{"im_age"}}, wantError: "attachment_mime_classes"},
		{name: "MIME numeric prefix", config: DocbankIntegrationConfig{AttachmentMIMEClasses: []string{"1image"}}, wantError: "attachment_mime_classes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			err := tc.config.validate()
			if tc.wantError != "" {
				require.ErrorContains(err, tc.wantError)
				return
			}
			require.NoError(err)
			assert.Equal(t, tc.collection, tc.config.MirrorCollection())
		})
	}
}
