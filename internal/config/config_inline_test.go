package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
)

func TestInlineConfigDefaultsAndExplicitMediaOverride(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	cfg := NewDefaultConfig()
	assertions.Equal(cfg.Slack.MediaPolicy(""), cfg.Inline.MediaPolicy(""), "Inline uses Slack's shared chat-media defaults")
	assertions.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll,
		MaxParticipants: DefaultMediaMaxParticipants, MaxBytes: DefaultChatMaxMediaBytes}, cfg.Inline.MediaPolicy(""))
	path := filepath.Join(t.TempDir(), "config.toml")
	requires.NoError(os.WriteFile(path, []byte(`
[inline]
media_max_participants = 0
max_media_mb = 40
[inline.accounts_config."api.inline.chat:user:42"]
media = false
[[inline.accounts]]
identifier = "api.inline.chat:user:42"
chat_ids = ["123", "456"]
`), 0o600))
	loaded, err := Load(path, "")
	requires.NoError(err)
	account := loaded.GetInlineAccount("api.inline.chat:user:42")
	requires.NotNil(account)
	assertions.Equal("mcp", account.EffectiveTransport())
	assertions.Equal(DefaultInlineMCPEndpoint, account.EffectiveEndpoint())
	assertions.Equal(attachmentpolicy.Policy{Scope: attachmentpolicy.ScopeAll,
		MaxBytes: 40 << 20, DisabledReason: attachmentpolicy.SkipAccountPolicy}, loaded.Inline.MediaPolicy(account.Identifier))
	account.ChatIDs[0] = "999"
	assertions.Equal([]string{"123", "456"}, loaded.Inline.Accounts[0].ChatIDs, "returned account must not expose mutable selection")
}

func TestInlineAccountRejectsInvalidIdentitySelectionAndTransport(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*InlineAccount)
	}{
		{"bare principal", func(a *InlineAccount) { a.Identifier = "42" }},
		{"noncanonical principal", func(a *InlineAccount) { a.Identifier = "api.inline.chat:user:042" }},
		{"other origin", func(a *InlineAccount) { a.Identifier = "example.com:user:42" }},
		{"duplicate selection", func(a *InlineAccount) { a.ChatIDs = []string{"123", "123"} }},
		{"leading zero", func(a *InlineAccount) { a.ChatIDs = []string{"0123"} }},
		{"negative chat", func(a *InlineAccount) { a.ChatIDs = []string{"-1"} }},
		{"unsafe chat", func(a *InlineAccount) { a.ChatIDs = []string{"9007199254740992"} }},
		{"unsafe principal", func(a *InlineAccount) { a.Identifier = "api.inline.chat:user:9007199254740992" }},
		{"overflow chat", func(a *InlineAccount) { a.ChatIDs = []string{"9223372036854775808"} }},
		{"automatic transport", func(a *InlineAccount) { a.Transport = "auto" }},
		{"insecure endpoint", func(a *InlineAccount) { a.Endpoint = "http://mcp.inline.chat/mcp/v2" }},
		{"other server", func(a *InlineAccount) { a.Endpoint = "https://example.test/mcp/v2" }},
		{"endpoint credentials", func(a *InlineAccount) { a.Endpoint = "https://user:pass@mcp.inline.chat/mcp/v2" }},
		{"query token", func(a *InlineAccount) { a.Endpoint = "https://mcp.inline.chat/mcp/v2?token=example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := InlineAccount{Identifier: "api.inline.chat:user:42", ChatIDs: []string{"123"}}
			tc.edit(&account)
			require.Error(t, account.Validate())
		})
	}
}

func TestInlineConfigRejectsDuplicateAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
[[inline.accounts]]
identifier = "api.inline.chat:user:42"
chat_ids = ["123"]
[[inline.accounts]]
identifier = "api.inline.chat:user:42"
chat_ids = ["456"]
`), 0o600))
	_, err := Load(path, "")
	require.ErrorContains(t, err, "duplicate identifier")
}

func TestEditInlineAccountMergesSelectionAndPreservesConfig(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	original := `# Keep this comment.
[inline]
enabled = false # operator schedule setting
schedule = "*/15 * * * *"
media_max_participants = 0
[[inline.accounts]]
identifier = "api.inline.chat:user:42"
transport = "mcp" # transport comment
chat_ids = ["123", "456"]
extension = "retained"
[[inline.accounts]]
identifier = "api.inline.chat:user:99"
chat_ids = ["700"]
[web]
theme = "dark" # untouched
`
	requires.NoError(os.WriteFile(path, []byte(original), 0o600))
	snapshot, err := ReadConfigFile(path)
	requires.NoError(err)
	published, err := EditInlineAccount(path, snapshot.ETag, InlineAccount{
		Identifier: "api.inline.chat:user:42", Transport: "cli", CLIPath: "/opt/example/inline", ChatIDs: []string{"456", "789"},
	})
	requires.NoError(err)
	loaded, err := LoadConfigFile(published, "")
	requires.NoError(err)
	assertions.True(loaded.Inline.Enabled)
	assertions.Equal("*/15 * * * *", loaded.Inline.Schedule)
	assertions.Equal([]string{"123", "456", "789"}, loaded.Inline.Accounts[0].ChatIDs)
	assertions.Equal("cli", loaded.Inline.Accounts[0].Transport)
	assertions.Equal("/opt/example/inline", loaded.Inline.Accounts[0].CLIPath)
	assertions.Equal([]string{"700"}, loaded.Inline.Accounts[1].ChatIDs)
	assertions.Contains(string(published.Content), `extension = "retained"`)
	assertions.Contains(string(published.Content), `theme = "dark" # untouched`)
	assertions.Contains(string(published.Content), "# transport comment")
	assertions.Contains(string(published.Content), "# Keep this comment.")
	_, err = EditInlineAccount(path, snapshot.ETag, InlineAccount{Identifier: "api.inline.chat:user:42", ChatIDs: []string{"800"}})
	requires.ErrorIs(err, ErrConfigConflict)
	after, err := os.ReadFile(path)
	requires.NoError(err)
	assertions.Equal(published.Content, after)
}

func TestEditInlineAccountAppendsAndRejectsInlineArrayWithoutDiscarding(t *testing.T) {
	for _, tc := range []struct {
		name, original string
		wantError      bool
	}{
		{"new", "# fresh config\n", false},
		{"no trailing newline", "[web]\ntheme = \"dark\"", false},
		{"inline array", "[inline]\naccounts = [{identifier = \"api.inline.chat:user:99\", chat_ids = [\"700\"]}]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requires := require.New(t)

			path := filepath.Join(t.TempDir(), "config.toml")
			requires.NoError(os.WriteFile(path, []byte(tc.original), 0o600))
			snapshot, err := ReadConfigFile(path)
			requires.NoError(err)
			published, err := EditInlineAccount(path, snapshot.ETag, InlineAccount{Identifier: "api.inline.chat:user:42", ChatIDs: []string{"123"}})
			if tc.wantError {
				requires.ErrorIs(err, ErrAmbiguousConfigTarget)
				after, readErr := os.ReadFile(path)
				requires.NoError(readErr)
				assertions.Equal(tc.original, string(after))
				return
			}
			requires.NoError(err)
			loaded, err := LoadConfigFile(published, "")
			requires.NoError(err)
			assertions.Equal([]string{"123"}, loaded.Inline.Accounts[0].ChatIDs)
			assertions.Equal(uint32(0o600), uint32(published.Mode.Perm()))
		})
	}
}

func TestEditInlineAccountCreatesMissingConfig(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	snapshot, err := ReadConfigFile(path)
	requires.NoError(err)
	requires.False(snapshot.Exists)
	published, err := EditInlineAccount(path, snapshot.ETag, InlineAccount{Identifier: "api.inline.chat:user:42", ChatIDs: []string{"123"}})
	requires.NoError(err)
	loaded, err := LoadConfigFile(published, "")
	requires.NoError(err)
	assertions.True(loaded.Inline.Enabled)
	assertions.Equal("api.inline.chat:user:42", loaded.Inline.Accounts[0].Identifier)
	assertions.Equal([]string{"123"}, loaded.Inline.Accounts[0].ChatIDs)
}

func TestInlineAccountEmptyChatFilterArchivesAllAndReAddCanRestoreIt(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	cfg := NewDefaultConfig()
	principal := "api.inline.chat:user:42"
	requires.NoError(cfg.UpsertInlineAccount(InlineAccount{Identifier: principal}))
	assertions.Empty(cfg.Inline.Accounts[0].ChatIDs)
	requires.NoError(cfg.UpsertInlineAccount(InlineAccount{Identifier: principal, ChatIDs: []string{"123"}}))
	assertions.Equal([]string{"123"}, cfg.Inline.Accounts[0].ChatIDs)
	requires.NoError(cfg.UpsertInlineAccount(InlineAccount{Identifier: principal, ChatIDs: []string{"456"}}))
	assertions.Equal([]string{"123", "456"}, cfg.Inline.Accounts[0].ChatIDs)
	requires.NoError(cfg.UpsertInlineAccount(InlineAccount{Identifier: principal}))
	assertions.Empty(cfg.Inline.Accounts[0].ChatIDs)
}

func TestEditInlineAccountCanRemoveChatFilter(t *testing.T) {
	assertions := assert.New(t)
	requires := require.New(t)

	path := filepath.Join(t.TempDir(), "config.toml")
	requires.NoError(os.WriteFile(path, []byte(`[[inline.accounts]]
identifier = "api.inline.chat:user:42"
chat_ids = ["123"]
`), 0o600))
	snapshot, err := ReadConfigFile(path)
	requires.NoError(err)
	published, err := EditInlineAccount(path, snapshot.ETag, InlineAccount{Identifier: "api.inline.chat:user:42"})
	requires.NoError(err)
	loaded, err := LoadConfigFile(published, "")
	requires.NoError(err)
	assertions.Empty(loaded.Inline.Accounts[0].ChatIDs)
	assertions.True(loaded.Inline.Enabled)
}
