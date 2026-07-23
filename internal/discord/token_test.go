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
	"time"

	"github.com/gofrs/flock"
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
	assert.Equal(t, sha256.Sum256([]byte(want.AccessToken())), sha256.Sum256([]byte(got.AccessToken())))
}

func newTestTokenRecord(botUserID, botUsername, accessToken, binding string) TokenRecord {
	return NewTokenRecord(botUserID, botUsername, accessToken, binding)
}

func TestTokenRecordJSONDoesNotExposeAccessToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	record := newTestTokenRecord(testBotID1, "archive-bot", "secret.discord.token", "work")

	data, err := json.Marshal(record)
	require.NoError(err)
	var public map[string]any
	require.NoError(json.Unmarshal(data, &public))
	_, hasAccessToken := public["access_token"]
	assert.False(hasAccessToken, "public JSON exposed the access_token field")
	assert.Equal(testBotID1, public["bot_user_id"])
	assertNoCredentialLeak(t, record.String(), record.AccessToken())
}

func TestTokenRecordFormattingDoesNotExposeAccessToken(t *testing.T) {
	record := newTestTokenRecord(testBotID1, "archive-bot", "format-only-secret-token", "work")

	formats := []string{
		"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%o", "%O",
		"%b", "%c", "%e", "%E", "%f", "%F", "%g", "%G", "%U", "%t",
	}
	for _, verb := range "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ" {
		// fmt reserves %p, %T, and %w and bypasses Formatter for them. %p on
		// pointers and %T expose only an address/type; %w is valid only in Errorf.
		if verb == 'p' || verb == 'T' || verb == 'w' {
			continue
		}
		formats = append(formats, "%"+string(verb))
	}
	for _, format := range formats {
		t.Run(format, func(t *testing.T) {
			assertNoCredentialLeak(t, fmt.Sprintf(format, record), record.AccessToken())
			assertNoCredentialLeak(t, fmt.Sprintf(format, &record), record.AccessToken())
		})
	}
	assertNoCredentialLeak(t, fmt.Sprintf("%p", record), record.AccessToken())
}

func TestTokenManagerSaveUsesSecureNamedFile(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	dir := filepath.Join(t.TempDir(), "tokens")
	manager := NewTokenManager(dir)
	record := newTestTokenRecord(testBotID1, "archive-bot", "secret.discord.token", "work")

	requireCredentialSuccess(t, manager.Save(record))
	path := filepath.Join(dir, "discord_"+testBotID1+".json")
	assert.Equal(path, manager.TokenPath(testBotID1))

	data, err := os.ReadFile(path)
	require.NoError(err)
	var stored map[string]any
	require.NoError(json.Unmarshal(data, &stored))
	storedToken, ok := stored["access_token"].(string)
	require.True(ok, "stored credential JSON access_token is not a string")
	assert.Equal(sha256.Sum256([]byte(record.AccessToken())), sha256.Sum256([]byte(storedToken)))

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
	work := newTestTokenRecord(testBotID1, "work-bot", "token-one", "work")
	personal := newTestTokenRecord(testBotID2, "personal-bot", "token-two", "personal")
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
	want := newTestTokenRecord(testBotID1, "archive-bot", "token-one", "")
	requireCredentialSuccess(t, manager.Save(want))

	got, err := manager.Resolve("")
	requireCredentialSuccess(t, err)
	assertTokenRecord(t, want, got)
}

func TestTokenManagerPromotesUnnamedToken(t *testing.T) {
	assert := assert.New(t)
	manager := NewTokenManager(t.TempDir())
	unnamed := newTestTokenRecord(testBotID1, "archive-bot", "token-one", "")
	requireCredentialSuccess(t, manager.Save(unnamed))

	promoted, err := manager.Promote(testBotID1, "work")
	requireCredentialSuccess(t, err)
	assert.Equal("work", promoted.Binding)
	assert.Equal(sha256.Sum256([]byte(unnamed.AccessToken())), sha256.Sum256([]byte(promoted.AccessToken())))

	got, err := manager.Resolve("work")
	requireCredentialSuccess(t, err)
	assertTokenRecord(t, promoted, got)

	second := newTestTokenRecord(testBotID2, "second-bot", "token-two", "personal")
	requireCredentialSuccess(t, manager.Save(second))
}

func TestTokenManagerResolveUnnamedRejectsAmbiguity(t *testing.T) {
	manager := NewTokenManager(t.TempDir())
	requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID1, "work-bot", "token-one", "work")))
	requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID2, "personal-bot", "token-two", "personal")))

	_, err := manager.Resolve("")
	requireCredentialFailure(t, err)
	requireCredentialErrorIs(t, err, ErrAmbiguousBinding)
	assertNoCredentialLeak(t, err.Error(), "token-one")
	assertNoCredentialLeak(t, err.Error(), "token-two")
}

func TestTokenManagerRejectsDuplicateBinding(t *testing.T) {
	assert := assert.New(t)
	manager := NewTokenManager(t.TempDir())
	requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID1, "first-bot", "token-one", "work")))

	err := manager.Save(newTestTokenRecord(testBotID2, "second-bot", "token-two", "work"))
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
			requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID1, "first-bot", "token-one", "")))

			err := manager.Save(newTestTokenRecord(testBotID2, "second-bot", "token-two", tt.binding))
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
					record := newTestTokenRecord(
						strconv.FormatUint(333456789012345678+uint64(i), 10),
						"concurrent-bot", "concurrent-secret-"+strconv.Itoa(i), tt.binding,
					)
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
	requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID1, "archive-bot", "token-one", "work")))

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
		{name: "missing bot id", record: newTestTokenRecord("", "bot", "secret", "")},
		{name: "malformed bot id", record: newTestTokenRecord("../escape", "bot", "secret", "")},
		{name: "missing username", record: newTestTokenRecord(testBotID1, "", "secret", "")},
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
			if tt.record.AccessToken() != "" {
				assertNoCredentialLeak(t, err.Error(), tt.record.AccessToken())
			}
		})
	}
}

func TestTokenManagerDeleteRemovesOnlyValidatedBotCredential(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	manager := NewTokenManager(t.TempDir())
	first := newTestTokenRecord(testBotID1, "first-bot", "token-one", "work")
	second := newTestTokenRecord(testBotID2, "second-bot", "token-two", "personal")
	requireCredentialSuccess(t, manager.Save(first))
	requireCredentialSuccess(t, manager.Save(second))

	require.NoError(manager.Delete(first.BotUserID))
	_, err := os.Stat(manager.TokenPath(first.BotUserID))
	assert.True(os.IsNotExist(err))
	got, err := manager.Resolve(second.Binding)
	require.NoError(err)
	assertTokenRecord(t, second, got)

	err = manager.Delete("../escape")
	require.Error(err)
	_, statErr := os.Stat(manager.TokenPath(second.BotUserID))
	assert.NoError(statErr, "invalid deletion must preserve other credentials")
}

func TestTokenManagerDeleteMissingCredentialRootIsIdempotent(t *testing.T) {
	manager := NewTokenManager(filepath.Join(t.TempDir(), "missing", "tokens"))

	require.NoError(t, manager.Delete(testBotID1))
}

func TestTokenManagerDeleteUsesCredentialStoreLock(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	manager := NewTokenManager(dir)
	requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID1, "archive-bot", "token-one", "")))

	lock := flock.New(filepath.Join(dir, ".discord-token.lock"), flock.SetPermissions(0600))
	require.NoError(lock.Lock())
	done := make(chan error, 1)
	go func() { done <- manager.Delete(testBotID1) }()

	select {
	case err := <-done:
		require.FailNow("credential deletion bypassed store lock", "error: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(lock.Unlock())
	require.NoError(<-done)
}

func TestTokenManagerDeleteRejectsSymlinkedTokenRoot(t *testing.T) {
	realDir := t.TempDir()
	realManager := NewTokenManager(realDir)
	requireCredentialSuccess(t, realManager.Save(newTestTokenRecord(testBotID1, "archive-bot", "token-one", "")))
	link := filepath.Join(t.TempDir(), "tokens-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := NewTokenManager(link).Delete(testBotID1)
	require.Error(t, err)
	_, statErr := os.Stat(realManager.TokenPath(testBotID1))
	assert.NoError(t, statErr, "symlinked token root must preserve credential")
}

func TestTokenManagerDeleteRejectsSymlinkCredentialFile(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	manager := NewTokenManager(dir)
	target := filepath.Join(t.TempDir(), "outside-token.json")
	require.NoError(os.WriteFile(target, []byte(`{"bot_user_id":"`+testBotID1+`","bot_username":"archive-bot","access_token":"token-one"}`), 0o600))
	if err := os.Symlink(target, manager.TokenPath(testBotID1)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := manager.Delete(testBotID1)
	require.Error(err)
	_, statErr := os.Lstat(manager.TokenPath(testBotID1))
	require.NoError(statErr, "symlink credential entry must be preserved")
	_, statErr = os.Stat(target)
	assert.NoError(t, statErr, "symlink target must be preserved")
}

func TestTokenManagerDeletePreservesTargetWhenSiblingRecordIsMalformed(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	manager := NewTokenManager(dir)
	requireCredentialSuccess(t, manager.Save(newTestTokenRecord(testBotID1, "first-bot", "token-one", "work")))
	require.NoError(os.WriteFile(manager.TokenPath(testBotID2), []byte(`{"malformed":`), 0o600))

	err := manager.Delete(testBotID1)
	require.Error(err)
	_, statErr := os.Stat(manager.TokenPath(testBotID1))
	assert.NoError(t, statErr, "invalid sibling store entry must preserve target credential")
}

func TestTokenManagerDeleteRejectsNonDirectoryTokenRoot(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "tokens")
	require.NoError(os.WriteFile(root, []byte("not a directory"), 0o600))

	err := NewTokenManager(root).Delete(testBotID1)
	require.Error(err)
	data, readErr := os.ReadFile(root)
	require.NoError(readErr)
	assert.Equal(t, "not a directory", string(data))
}
