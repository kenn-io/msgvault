package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestBlandConfig(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	requirements.NoError(os.WriteFile(path, []byte(`[[bland]]
account_email = "Owner@example.com"
api_key = "test-key"
encrypted_key = "test-byot"
enabled = true
schedule = "0 */6 * * *"
max_media_mb = 12
fetch_corrected_transcript = true
`), 0600))
	cfg, err := Load(path, "")
	requirements.NoError(err)
	s := cfg.GetBlandSource("default")
	requirements.NotNil(s)
	assertions.Equal("owner@example.com", s.AccountEmail)
	assertions.Equal(int64(12<<20), s.MediaPolicy().MaxBytes)
	assertions.Len(cfg.ScheduledBlandSources(), 1)
	assertions.True(s.FetchCorrectedTranscript)
	assertions.False((BlandSource{}).FetchCorrectedTranscript)
	s.MediaScope = "none"
	assertions.Equal(attachmentpolicy.ScopeNone, s.MediaPolicy().Scope)
}
