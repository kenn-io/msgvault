package discord

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBotID1 = "113456789012345678"
	testBotID2 = "223456789012345678"
)

func TestTokenRecordJSONDoesNotExposeAccessToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	record := TokenRecord{
		BotUserID:   testBotID1,
		BotUsername: "archive-bot",
		AccessToken: "secret.discord.token",
		Binding:     "work",
	}

	data, err := json.Marshal(record)
	require.NoError(err)
	assert.NotContains(string(data), record.AccessToken)
	assert.NotContains(string(data), "access_token")
	assert.Contains(string(data), testBotID1)
	assert.NotContains(record.String(), record.AccessToken)
}

func TestTokenManagerSaveUsesSecureNamedFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	manager := NewTokenManager(dir)
	record := TokenRecord{
		BotUserID:   testBotID1,
		BotUsername: "archive-bot",
		AccessToken: "secret.discord.token",
		Binding:     "work",
	}

	require.NoError(manager.Save(record))
	path := filepath.Join(dir, "discord_"+testBotID1+".json")
	assert.Equal(path, manager.TokenPath(testBotID1))

	data, err := os.ReadFile(path)
	require.NoError(err)
	assert.Contains(string(data), `"access_token": "secret.discord.token"`)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(err)
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
		dirInfo, err := os.Stat(dir)
		require.NoError(err)
		assert.Equal(os.FileMode(0700), dirInfo.Mode().Perm())
	}
}

func TestTokenManagerResolveNamedBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	work := TokenRecord{BotUserID: testBotID1, BotUsername: "work-bot", AccessToken: "token-one", Binding: "work"}
	personal := TokenRecord{BotUserID: testBotID2, BotUsername: "personal-bot", AccessToken: "token-two", Binding: "personal"}
	require.NoError(manager.Save(work))
	require.NoError(manager.Save(personal))

	got, err := manager.Resolve("personal")
	require.NoError(err)
	assert.Equal(personal, got)

	_, err = manager.Resolve("missing")
	require.Error(err)
	assert.ErrorIs(err, ErrTokenNotFound)
}

func TestTokenManagerResolveSoleTokenWithoutBinding(t *testing.T) {
	manager := NewTokenManager(t.TempDir())
	want := TokenRecord{BotUserID: testBotID1, BotUsername: "archive-bot", AccessToken: "token-one"}
	require.NoError(t, manager.Save(want))

	got, err := manager.Resolve("")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestTokenManagerPromotesUnnamedToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	unnamed := TokenRecord{BotUserID: testBotID1, BotUsername: "archive-bot", AccessToken: "token-one"}
	require.NoError(manager.Save(unnamed))

	promoted, err := manager.Promote(testBotID1, "work")
	require.NoError(err)
	assert.Equal("work", promoted.Binding)
	assert.Equal(unnamed.AccessToken, promoted.AccessToken)

	got, err := manager.Resolve("work")
	require.NoError(err)
	assert.Equal(promoted, got)

	second := TokenRecord{BotUserID: testBotID2, BotUsername: "second-bot", AccessToken: "token-two", Binding: "personal"}
	assert.NoError(manager.Save(second), "a named second bot is safe after promotion")
}

func TestTokenManagerResolveUnnamedRejectsAmbiguity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	require.NoError(manager.Save(TokenRecord{
		BotUserID: testBotID1, BotUsername: "work-bot", AccessToken: "token-one", Binding: "work",
	}))
	require.NoError(manager.Save(TokenRecord{
		BotUserID: testBotID2, BotUsername: "personal-bot", AccessToken: "token-two", Binding: "personal",
	}))

	_, err := manager.Resolve("")
	require.Error(err)
	require.ErrorIs(err, ErrAmbiguousBinding)
	assert.NotContains(err.Error(), "token-one")
	assert.NotContains(err.Error(), "token-two")
}

func TestTokenManagerRejectsDuplicateBinding(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	require.NoError(manager.Save(TokenRecord{
		BotUserID: testBotID1, BotUsername: "first-bot", AccessToken: "token-one", Binding: "work",
	}))

	err := manager.Save(TokenRecord{
		BotUserID: testBotID2, BotUsername: "second-bot", AccessToken: "token-two", Binding: "work",
	})
	require.Error(err)
	require.ErrorIs(err, ErrDuplicateBinding)
	assert.NotContains(err.Error(), "token-one")
	assert.NotContains(err.Error(), "token-two")
	_, statErr := os.Stat(manager.TokenPath(testBotID2))
	assert.True(os.IsNotExist(statErr))
}

func TestTokenManagerRejectsSecondBotWhileDefaultIsUnnamed(t *testing.T) {
	tests := []struct {
		name    string
		binding string
	}{
		{name: "second bot is unnamed"},
		{name: "second bot is named", binding: "personal"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewTokenManager(t.TempDir())
			require.NoError(t, manager.Save(TokenRecord{
				BotUserID: testBotID1, BotUsername: "first-bot", AccessToken: "token-one",
			}))

			err := manager.Save(TokenRecord{
				BotUserID: testBotID2, BotUsername: "second-bot", AccessToken: "token-two", Binding: tt.binding,
			})
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrAmbiguousBinding)
		})
	}
}

func TestTokenManagerListValidatesStoredRecords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	require.NoError(manager.Save(TokenRecord{
		BotUserID: testBotID1, BotUsername: "archive-bot", AccessToken: "token-one", Binding: "work",
	}))

	badPath := manager.TokenPath(testBotID2)
	require.NoError(os.WriteFile(badPath, []byte(`{"bot_user_id":"not-the-filename","bot_username":"bad","access_token":"do-not-leak"}`), 0600))

	_, err := manager.List()
	require.Error(err)
	assert.NotContains(err.Error(), "do-not-leak")
	assert.Contains(err.Error(), filepath.Base(badPath))
}

func TestTokenManagerValidatesRecordsBeforeWriting(t *testing.T) {
	tests := []struct {
		name   string
		record TokenRecord
	}{
		{name: "missing bot id", record: TokenRecord{BotUsername: "bot", AccessToken: "secret"}},
		{name: "malformed bot id", record: TokenRecord{BotUserID: "../escape", BotUsername: "bot", AccessToken: "secret"}},
		{name: "missing username", record: TokenRecord{BotUserID: testBotID1, AccessToken: "secret"}},
		{name: "missing access token", record: TokenRecord{BotUserID: testBotID1, BotUsername: "bot"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			manager := NewTokenManager(t.TempDir())
			err := manager.Save(tt.record)
			require.Error(err)
			require.NotErrorIs(err, ErrDuplicateBinding)
			if tt.record.AccessToken != "" {
				assert.NotContains(err.Error(), tt.record.AccessToken)
			}
		})
	}
}
