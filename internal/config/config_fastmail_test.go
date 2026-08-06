package config

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityops"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func loadConfigText(t *testing.T, content string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	cfg, err := Load(path, "")
	require.NoError(t, err)
	return cfg
}

func loadConfigTextError(t *testing.T, content string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	_, err := Load(path, "")
	return err
}

func TestFastmailConfigAcceptsAccountOrSourceID(t *testing.T) {
	assert := assert.New(t)
	cfg := loadConfigText(t, `
[[fastmail]]
account = "primary@example.test"
api_token = "fm_test_account"

[[fastmail]]
source_id = 14
api_token = "fm_test_id"
auto_confirm_identities = true
`)
	require.Len(t, cfg.Fastmail, 2)
	assert.Equal("primary@example.test", cfg.Fastmail[0].Account)
	assert.Equal(int64(14), cfg.Fastmail[1].SourceID)
	assert.True(cfg.Fastmail[1].AutoConfirmIdentities)
}

func TestFastmailConfigRejectsBothSelectors(t *testing.T) {
	err := loadConfigTextError(t, `[[fastmail]]
account="primary@example.test"
source_id=14
api_token="fm_test"
`)
	require.ErrorContains(t, err, "account and source_id are mutually exclusive")
}

func TestFastmailConfigRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "missing selector",
			content: `[[fastmail]]
api_token = "fm_test_missing_selector"
`,
			wantErr: "account or source_id is required",
		},
		{
			name: "zero source ID",
			content: `[[fastmail]]
source_id = 0
api_token = "fm_test_zero_id"
`,
			wantErr: "source_id must be positive",
		},
		{
			name: "negative source ID",
			content: `[[fastmail]]
source_id = -1
api_token = "fm_test_negative_id"
`,
			wantErr: "source_id must be positive",
		},
		{
			name: "empty token",
			content: `[[fastmail]]
account = "primary@example.test"
api_token = ""
`,
			wantErr: "api_token is required",
		},
		{
			name: "duplicate account selector",
			content: `[[fastmail]]
account = " primary@example.test "
api_token = "fm_test_first"

[[fastmail]]
account = "PRIMARY@example.test"
api_token = "fm_test_second"
`,
			wantErr: "duplicate account selector",
		},
		{
			name: "Unicode fold-equivalent account selectors",
			content: `[[fastmail]]
account = "s@example.test"
api_token = "fm_test_first"

[[fastmail]]
account = "ſ@example.test"
api_token = "fm_test_second"
`,
			wantErr: "duplicate account selector",
		},
		{
			name: "duplicate source ID selector",
			content: `[[fastmail]]
source_id = 14
api_token = "fm_test_first"

[[fastmail]]
source_id = 14
api_token = "fm_test_second"
`,
			wantErr: "duplicate source_id selector",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := loadConfigTextError(t, tt.content)
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestFastmailSourceSelector(t *testing.T) {
	tests := []struct {
		name    string
		source  FastmailSource
		want    identityops.SourceSelector
		wantErr string
	}{
		{
			name:   "account",
			source: FastmailSource{Account: " primary@example.test "},
			want:   identityops.SourceSelector{Account: "primary@example.test"},
		},
		{
			name:   "source ID",
			source: FastmailSource{SourceID: 14},
			want:   identityops.SourceSelector{SourceID: 14},
		},
		{
			name:    "missing selector",
			source:  FastmailSource{},
			wantErr: "account or source_id is required",
		},
		{
			name:    "both selectors",
			source:  FastmailSource{Account: "primary@example.test", SourceID: 14},
			wantErr: "account and source_id are mutually exclusive",
		},
		{
			name:    "negative source ID",
			source:  FastmailSource{SourceID: -1},
			wantErr: "source_id must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			got, err := tt.source.Selector()
			if tt.wantErr != "" {
				require.ErrorContains(err, tt.wantErr)
				return
			}
			require.NoError(err)
			assert.Equal(tt.want, got)
		})
	}
}

func TestFastmailSourceForMatchesIDBeforeAccountAndReturnsCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "primary@example.test")
	require.NoError(err)
	cfg := &Config{Fastmail: []FastmailSource{
		{Account: "primary@example.test", APIToken: "fm_test_account"},
		{SourceID: source.ID, APIToken: "fm_test_id"},
	}}

	got, err := cfg.FastmailSourceFor(st, source.ID)
	require.NoError(err)
	require.NotNil(got)
	assert.Equal(source.ID, got.SourceID)

	got.APIToken = "modified"
	assert.Equal("fm_test_id", cfg.Fastmail[1].APIToken)
}

func TestFastmailSourceForMatchesIdentifierOrDisplayName(t *testing.T) {
	tests := []struct {
		name   string
		source *store.Source
	}{
		{
			name:   "identifier",
			source: &store.Source{ID: 14, SourceType: "imap", Identifier: "PRIMARY@example.test"},
		},
		{
			name: "display name",
			source: &store.Source{
				ID:          14,
				SourceType:  "imap",
				Identifier:  "imap://primary",
				DisplayName: sql.NullString{String: "PRIMARY@example.test", Valid: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource(tt.source.SourceType, tt.source.Identifier)
			require.NoError(err)
			if tt.source.DisplayName.Valid {
				require.NoError(st.UpdateSourceDisplayName(source.ID, tt.source.DisplayName.String))
			}
			cfg := loadConfigText(t, `
[[fastmail]]
account = " primary@example.test "
api_token = "fm_test_account"
`)

			got, err := cfg.FastmailSourceFor(st, source.ID)
			require.NoError(err)
			require.NotNil(got)
			assert.Equal(t, "primary@example.test", got.Account)
		})
	}
}

func TestFastmailSourceForRejectsAmbiguousAccountMatchesWithoutToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "PRIMARY@example.test")
	require.NoError(err)
	require.NoError(st.UpdateSourceDisplayName(source.ID, "primary mailbox"))
	cfg := &Config{Fastmail: []FastmailSource{
		{Account: "primary@example.test", APIToken: "fm_test_identifier"},
		{Account: "Primary Mailbox", APIToken: "fm_test_display"},
	}}
	got, err := cfg.FastmailSourceFor(st, source.ID)
	require.ErrorContains(err, "ambiguous Fastmail configuration")
	assert.Nil(got)
	assert.NotContains(err.Error(), "fm_test_identifier")
	assert.NotContains(err.Error(), "fm_test_display")
}

func TestFastmailSourceForRejectsAccountMatchingMultipleArchiveSources(t *testing.T) {
	tests := []struct {
		name       string
		identifier string
		display    string
	}{
		{name: "identifier", identifier: "primary@example.test"},
		{name: "display name", identifier: "imap://primary", display: "primary@example.test"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			first, err := st.GetOrCreateSource("imap", tt.identifier)
			require.NoError(err)
			second, err := st.GetOrCreateSource("gmail", tt.identifier)
			require.NoError(err)
			if tt.display != "" {
				require.NoError(st.UpdateSourceDisplayName(first.ID, tt.display))
				require.NoError(st.UpdateSourceDisplayName(second.ID, tt.display))
			}

			cfg := &Config{Fastmail: []FastmailSource{{
				Account:  "PRIMARY@example.test",
				APIToken: "fm_test_must_not_leak",
			}}}
			for _, sourceID := range []int64{first.ID, second.ID} {
				got, lookupErr := cfg.FastmailSourceFor(st, sourceID)
				require.ErrorContains(lookupErr, "matches multiple sources")
				assert.Nil(got)
				require.ErrorContains(lookupErr, "source_id")
				assert.NotContains(lookupErr.Error(), "fm_test_must_not_leak")
			}
		})
	}
}

// TestFastmailSourceForWrapsStoreFailure confirms that infrastructure
// failures while listing archive sources are distinguishable from
// configuration/selector errors, so callers can classify them as internal
// server errors instead of user-input errors.
func TestFastmailSourceForWrapsStoreFailure(t *testing.T) {
	t.Run("store failure is wrapped", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		cfg := &Config{Fastmail: []FastmailSource{{Account: "primary@example.test", APIToken: "fm_test"}}}
		st := failingFastmailSourceStore{err: errors.New("db unavailable")}

		got, err := cfg.FastmailSourceFor(st, 14)

		require.Error(err)
		assert.Nil(got)
		assert.ErrorIs(err, ErrFastmailSourceLookup)
	})

	t.Run("misconfiguration is not wrapped", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		st := testutil.NewTestStore(t)
		source, err := st.GetOrCreateSource("imap", "PRIMARY@example.test")
		require.NoError(err)
		require.NoError(st.UpdateSourceDisplayName(source.ID, "primary mailbox"))
		cfg := &Config{Fastmail: []FastmailSource{
			{Account: "primary@example.test", APIToken: "fm_test_identifier"},
			{Account: "Primary Mailbox", APIToken: "fm_test_display"},
		}}

		got, lookupErr := cfg.FastmailSourceFor(st, source.ID)

		require.Error(lookupErr)
		assert.Nil(got)
		assert.NotErrorIs(lookupErr, ErrFastmailSourceLookup)
	})
}

type failingFastmailSourceStore struct {
	err error
}

func (f failingFastmailSourceStore) ListSources(string) ([]*store.Source, error) {
	return nil, f.err
}

func TestFastmailSourceForReturnsNilWhenSourceIsNotConfigured(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "other@example.test")
	require.NoError(t, err)
	cfg := &Config{Fastmail: []FastmailSource{{
		Account:  "primary@example.test",
		APIToken: "fm_test_account",
	}}}

	got, err := cfg.FastmailSourceFor(st, source.ID)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestFastmailConfigSurvivesSaveAndLoad(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Fastmail = []FastmailSource{
		{
			Account:               "primary@example.test",
			APIToken:              "fm_test_account",
			AutoConfirmIdentities: true,
		},
		{
			SourceID:              14,
			APIToken:              "fm_test_source_id",
			AutoConfirmIdentities: true,
		},
	}

	require.NoError(cfg.Save())
	content, err := os.ReadFile(cfg.ConfigFilePath())
	require.NoError(err)
	assert.NotContains(string(content), "source_id = 0")

	loaded, err := Load(cfg.ConfigFilePath(), "")
	require.NoError(err)
	require.Len(loaded.Fastmail, 2)
	assert.Equal("primary@example.test", loaded.Fastmail[0].Account)
	assert.Equal("fm_test_account", loaded.Fastmail[0].APIToken)
	assert.True(loaded.Fastmail[0].AutoConfirmIdentities)
	assert.False(loaded.Fastmail[0].sourceIDConfigured)
	assert.Equal(int64(14), loaded.Fastmail[1].SourceID)
	assert.Equal("fm_test_source_id", loaded.Fastmail[1].APIToken)
	assert.True(loaded.Fastmail[1].AutoConfirmIdentities)
	assert.True(loaded.Fastmail[1].sourceIDConfigured)
}
