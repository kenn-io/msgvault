package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeeperDraftConfigRoundTrip(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	requirements.NoError(os.WriteFile(path, []byte("[[beeper.drafts]]\nsource_id = 42\n"), 0o600))
	cfg, err := Load(path, "")
	requirements.NoError(err)
	requirements.Len(cfg.Beeper.Drafts, 1)
	assertions.Equal(int64(42), cfg.Beeper.Drafts[0].SourceID)

	data, err := os.ReadFile(path)
	requirements.NoError(err)
	assertions.Contains(string(data), "source_id = 42")
}

func TestBeeperDraftConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "missing source", body: "[[beeper.drafts]]\n"},
		{name: "zero source", body: "[[beeper.drafts]]\nsource_id = 0\n"},
		{name: "duplicate source", body: "[[beeper.drafts]]\nsource_id = 4\n[[beeper.drafts]]\nsource_id = 4\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tc.body), 0o600))
			_, err := Load(path, "")
			assertions.Error(err)
		})
	}
}
