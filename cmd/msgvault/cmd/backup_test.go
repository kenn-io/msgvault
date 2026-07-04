package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
)

func TestResolveBackupRepoPrecedence(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	tests := []struct {
		name        string
		flagValue   string
		configRepo  string
		wantRepo    string
		wantErr     bool
		wantErrText string
	}{
		{
			name:       "flag wins over config",
			flagValue:  "/flag/repo",
			configRepo: "/config/repo",
			wantRepo:   "/flag/repo",
		},
		{
			name:       "config used when flag empty",
			flagValue:  "",
			configRepo: "/config/repo",
			wantRepo:   "/config/repo",
		},
		{
			name:        "error when neither is set",
			flagValue:   "",
			configRepo:  "",
			wantErr:     true,
			wantErrText: "backup: no repository configured; pass --repo or set [backup] repo in config.toml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			cfg = &config.Config{Backup: config.BackupConfig{Repo: tt.configRepo}}

			repo, err := resolveBackupRepo(tt.flagValue)

			if tt.wantErr {
				require.Error(err)
				assert.EqualError(err, tt.wantErrText)
				return
			}
			require.NoError(err)
			assert.Equal(tt.wantRepo, repo)
		})
	}
}

func TestResolveBackupRepoNilConfig(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = nil

	repo, err := resolveBackupRepo("/flag/repo")

	require.NoError(t, err)
	assert.Equal(t, "/flag/repo", repo)
}
