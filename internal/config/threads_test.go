package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsSendConfigDefaultsAndSelectors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	assert.Empty(NewDefaultConfig().Threads.Sends)
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{"", true}, {"[[threads.sends]]\nsource_id=42\n", true}, {"[[threads.sends]]\nsource_id=42\n[[threads.sends]]\nsource_id=43\n", true},
		{"[[threads.sends]]\n", false}, {"[[threads.sends]]\nsource_id=0\n", false}, {"[[threads.sends]]\nsource_id=-1\n", false},
		{"[[threads.sends]]\nsource_id=42\n[[threads.sends]]\nsource_id=42\n", false}, {"[[threads.sends]]\naccount='threads:100:200'\n", false},
		{"[[threads.sends]]\nsource_id=42\nenabled=true\n", false}, {"[threads]\napp_secret='synthetic-secret'\n", false},
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(os.WriteFile(path, []byte(tc.body), 0o600))
		cfg, err := Load(path, "")
		if tc.valid {
			require.NoError(err)
			if tc.body != "" {
				assert.Equal(int64(42), cfg.Threads.Sends[0].SourceID)
			}
		} else {
			require.Error(err)
			assert.Contains(err.Error(), "threads")
		}
	}
}
