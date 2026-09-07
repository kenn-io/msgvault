package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteImageArchivingRequiresExplicitTrue(t *testing.T) {
	assert.False(t, NewDefaultConfig().Sync.ArchiveRemoteImages)
	for _, tc := range []struct {
		name, content string
		want          bool
	}{
		{"absent", "[sync]\nrate_limit_qps = 5\n", false},
		{"disabled", "[sync]\narchive_remote_images = false\n", false},
		{"enabled", "[sync]\narchive_remote_images = true\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0600))
			cfg, err := Load(path, "")
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.Sync.ArchiveRemoteImages)
		})
	}
}
