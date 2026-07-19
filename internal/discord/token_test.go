package discord

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testBotID1 = "113456789012345678"
	testBotID2 = "223456789012345678"
)

func requireCredentialSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		require.FailNow(t, "credential operation unexpectedly failed")
	}
}

func requireCredentialFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		require.FailNow(t, "credential operation unexpectedly succeeded")
	}
}

func requireCredentialErrorIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		require.FailNow(t, "credential error did not match expected category")
	}
}

func assertNoCredentialLeak(t *testing.T, text, secret string) {
	t.Helper()
	if strings.Contains(text, secret) {
		assert.Fail(t, "public output exposed a credential")
	}
}

func assertTokenRecord(t *testing.T, want, got TokenRecord) {
	t.Helper()
	assert.Equal(t, want.BotUserID, got.BotUserID)
	assert.Equal(t, want.BotUsername, got.BotUsername)
	assert.Equal(t, want.Binding, got.Binding)
	assert.Equal(t, sha256.Sum256([]byte(want.AccessToken)), sha256.Sum256([]byte(got.AccessToken)))
}

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
	var public map[string]any
	require.NoError(json.Unmarshal(data, &public))
	_, hasAccessToken := public["access_token"]
	assert.False(hasAccessToken, "public JSON exposed the access_token field")
	assert.Equal(testBotID1, public["bot_user_id"])
	assertNoCredentialLeak(t, record.String(), record.AccessToken)
}

func TestTokenRecordFormattingDoesNotExposeAccessToken(t *testing.T) {
	record := TokenRecord{
		BotUserID:   testBotID1,
		BotUsername: "archive-bot",
		AccessToken: "format-only-secret-token",
		Binding:     "work",
	}

	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
		t.Run(format, func(t *testing.T) {
			assertNoCredentialLeak(t, fmt.Sprintf(format, record), record.AccessToken)
			assertNoCredentialLeak(t, fmt.Sprintf(format, &record), record.AccessToken)
		})
	}
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

	requireCredentialSuccess(t, manager.Save(record))
	path := filepath.Join(dir, "discord_"+testBotID1+".json")
	assert.Equal(path, manager.TokenPath(testBotID1))

	data, err := os.ReadFile(path)
	require.NoError(err)
	var stored map[string]any
	require.NoError(json.Unmarshal(data, &stored))
	storedToken, ok := stored["access_token"].(string)
	require.True(ok, "stored credential JSON access_token is not a string")
	assert.Equal(sha256.Sum256([]byte(record.AccessToken)), sha256.Sum256([]byte(storedToken)))

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
	manager := NewTokenManager(t.TempDir())
	work := TokenRecord{BotUserID: testBotID1, BotUsername: "work-bot", AccessToken: "token-one", Binding: "work"}
	personal := TokenRecord{BotUserID: testBotID2, BotUsername: "personal-bot", AccessToken: "token-two", Binding: "personal"}
	requireCredentialSuccess(t, manager.Save(work))
	requireCredentialSuccess(t, manager.Save(personal))

	got, err := manager.Resolve("personal")
	requireCredentialSuccess(t, err)
	assertTokenRecord(t, personal, got)

	_, err = manager.Resolve("missing")
	requireCredentialFailure(t, err)
	requireCredentialErrorIs(t, err, ErrTokenNotFound)
}

func TestTokenManagerResolveSoleTokenWithoutBinding(t *testing.T) {
	manager := NewTokenManager(t.TempDir())
	want := TokenRecord{BotUserID: testBotID1, BotUsername: "archive-bot", AccessToken: "token-one"}
	requireCredentialSuccess(t, manager.Save(want))

	got, err := manager.Resolve("")
	requireCredentialSuccess(t, err)
	assertTokenRecord(t, want, got)
}

func TestTokenManagerPromotesUnnamedToken(t *testing.T) {
	assert := assert.New(t)
	manager := NewTokenManager(t.TempDir())
	unnamed := TokenRecord{BotUserID: testBotID1, BotUsername: "archive-bot", AccessToken: "token-one"}
	requireCredentialSuccess(t, manager.Save(unnamed))

	promoted, err := manager.Promote(testBotID1, "work")
	requireCredentialSuccess(t, err)
	assert.Equal("work", promoted.Binding)
	assert.Equal(sha256.Sum256([]byte(unnamed.AccessToken)), sha256.Sum256([]byte(promoted.AccessToken)))

	got, err := manager.Resolve("work")
	requireCredentialSuccess(t, err)
	assertTokenRecord(t, promoted, got)

	second := TokenRecord{BotUserID: testBotID2, BotUsername: "second-bot", AccessToken: "token-two", Binding: "personal"}
	requireCredentialSuccess(t, manager.Save(second))
}

func TestTokenManagerResolveUnnamedRejectsAmbiguity(t *testing.T) {
	manager := NewTokenManager(t.TempDir())
	requireCredentialSuccess(t, manager.Save(TokenRecord{
		BotUserID: testBotID1, BotUsername: "work-bot", AccessToken: "token-one", Binding: "work",
	}))
	requireCredentialSuccess(t, manager.Save(TokenRecord{
		BotUserID: testBotID2, BotUsername: "personal-bot", AccessToken: "token-two", Binding: "personal",
	}))

	_, err := manager.Resolve("")
	requireCredentialFailure(t, err)
	requireCredentialErrorIs(t, err, ErrAmbiguousBinding)
	assertNoCredentialLeak(t, err.Error(), "token-one")
	assertNoCredentialLeak(t, err.Error(), "token-two")
}

func TestTokenManagerRejectsDuplicateBinding(t *testing.T) {
	assert := assert.New(t)
	manager := NewTokenManager(t.TempDir())
	requireCredentialSuccess(t, manager.Save(TokenRecord{
		BotUserID: testBotID1, BotUsername: "first-bot", AccessToken: "token-one", Binding: "work",
	}))

	err := manager.Save(TokenRecord{
		BotUserID: testBotID2, BotUsername: "second-bot", AccessToken: "token-two", Binding: "work",
	})
	requireCredentialFailure(t, err)
	requireCredentialErrorIs(t, err, ErrDuplicateBinding)
	assertNoCredentialLeak(t, err.Error(), "token-one")
	assertNoCredentialLeak(t, err.Error(), "token-two")
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
			requireCredentialSuccess(t, manager.Save(TokenRecord{
				BotUserID: testBotID1, BotUsername: "first-bot", AccessToken: "token-one",
			}))

			err := manager.Save(TokenRecord{
				BotUserID: testBotID2, BotUsername: "second-bot", AccessToken: "token-two", Binding: tt.binding,
			})
			requireCredentialFailure(t, err)
			requireCredentialErrorIs(t, err, ErrAmbiguousBinding)
		})
	}
}

func TestTokenManagerConcurrentSavePreservesBindingInvariants(t *testing.T) {
	tests := []struct {
		name    string
		binding string
	}{
		{name: "unnamed bots"},
		{name: "duplicate named binding", binding: "shared"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const workers = 32
			dir := t.TempDir()
			start := make(chan struct{})
			results := make(chan error, workers)
			var ready sync.WaitGroup
			var done sync.WaitGroup
			ready.Add(workers)
			done.Add(workers)

			for i := range workers {
				go func() {
					defer done.Done()
					manager := NewTokenManager(dir)
					record := TokenRecord{
						BotUserID:   strconv.FormatUint(333456789012345678+uint64(i), 10),
						BotUsername: "concurrent-bot",
						AccessToken: "concurrent-secret-" + strconv.Itoa(i),
						Binding:     tt.binding,
					}
					ready.Done()
					<-start
					results <- manager.Save(record)
				}()
			}

			ready.Wait()
			close(start)
			done.Wait()
			close(results)

			successes := 0
			for err := range results {
				if err == nil {
					successes++
				}
			}
			assert.Equal(t, 1, successes, "concurrent saves must have exactly one winner")

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			credentialFiles := 0
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "discord_") && strings.HasSuffix(entry.Name(), ".json") {
					credentialFiles++
				}
			}
			assert.Equal(t, 1, credentialFiles, "concurrent saves must persist exactly one credential")
		})
	}
}

func TestTokenManagerListValidatesStoredRecords(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	manager := NewTokenManager(t.TempDir())
	requireCredentialSuccess(t, manager.Save(TokenRecord{
		BotUserID: testBotID1, BotUsername: "archive-bot", AccessToken: "token-one", Binding: "work",
	}))

	badPath := manager.TokenPath(testBotID2)
	require.NoError(os.WriteFile(badPath, []byte(`{"bot_user_id":"not-the-filename","bot_username":"bad","access_token":"do-not-leak"}`), 0600))

	_, err := manager.List()
	requireCredentialFailure(t, err)
	assertNoCredentialLeak(t, err.Error(), "do-not-leak")
	if !strings.Contains(err.Error(), filepath.Base(badPath)) {
		assert.Fail("credential error did not identify the invalid file")
	}
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
			manager := NewTokenManager(t.TempDir())
			err := manager.Save(tt.record)
			requireCredentialFailure(t, err)
			if errors.Is(err, ErrDuplicateBinding) {
				require.FailNow(t, "validation error unexpectedly reported a duplicate binding")
			}
			if tt.record.AccessToken != "" {
				assertNoCredentialLeak(t, err.Error(), tt.record.AccessToken)
			}
		})
	}
}
