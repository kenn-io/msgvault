package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMailKindConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		valid         bool
	}{
		{"default", "", true},
		{"enabled", `enabled=true
retention_declaration="operator reviewed provider policy"
credential_env="FIXTURE_MAIL_KIND_KEY"`, true},
		{"missing retention", "enabled=true", false},
		{"inline key", `api_key="not-a-real-secret"`, false},
		{"endpoint override", `endpoint="https://unreviewed.example"`, false},
		{"unknown provider", `provider="unknown"`, false},
		{"zero batch", "batch_size=0", false},
		{"zero choice", "minimum_choice_probability=0.0", false},
		{"low threshold", "minimum_transactional=0.8", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[mail.kind_classification]\n"+tc.content), 0o600))
			cfg, err := Load(path, t.TempDir())
			if !tc.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "jev", cfg.Mail.KindClassification.Provider)
			assert.Equal(t, 20, cfg.Mail.KindClassification.BatchSize)
			assert.InDelta(t, 0.9, cfg.Mail.KindClassification.MinimumTransactional, 1e-12)
		})
	}
}
