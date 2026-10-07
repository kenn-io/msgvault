package mcpevents

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestKeyCreationDisabledAndRestoreRules(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	path := filepath.Join(t.TempDir(), "events.key")
	_, err := New(t.Context(), f.Store, Options{KeyPath: path})
	require.NoError(err)
	_, err = os.Stat(path)
	require.True(os.IsNotExist(err))
	s, err := New(t.Context(), f.Store, Options{Enabled: true, Sources: []string{"gmail"}, KeyPath: path, OwnerKey: "synthetic-owner"})
	require.NoError(err)
	key, err := os.ReadFile(path)
	require.NoError(err)
	assert.Len(key, 32)
	info, err := os.Stat(path)
	require.NoError(err)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0600), info.Mode().Perm())
	}
	enc, err := encryptSecret(key, "sub_test", "current", []byte("synthetic secret"))
	require.NoError(err)
	now := time.Now()
	_, _, err = f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Now: now, Subscription: store.MCPSubscription{ID: "sub_test", Principal: s.principal, Name: messageFamily, Arguments: []byte(`{"conversation_id":"1","include_from_me":false}`), ScopeKind: "conversation", ScopeID: f.ConvID, SourceID: f.Source.ID, CallbackURL: "https://receiver.example.net/events", SecretEnc: enc, SecretRevision: 1, VerifiedRevision: 1, ExpiresAt: now.Add(time.Hour)}})
	require.NoError(err)
	require.NoError(os.Remove(path))
	_, err = New(t.Context(), f.Store, s.opts)
	require.Error(err)
	_, statErr := os.Stat(path)
	require.True(os.IsNotExist(statErr), "must not silently replace restore key")
	require.NoError(os.WriteFile(path, make([]byte, 31), 0600))
	_, err = New(t.Context(), f.Store, s.opts)
	require.Error(err)
	require.NoError(os.WriteFile(path, make([]byte, 32), 0600))
	_, err = New(t.Context(), f.Store, s.opts)
	require.Error(err, "wrong but correctly sized key must fail before callbacks")
	require.NoError(os.WriteFile(path, key, 0600))
	_, err = New(t.Context(), f.Store, s.opts)
	require.NoError(err)
}

func TestKeyFileModePolicy(t *testing.T) {
	// Windows reports writable regular files as 0666 even when SecureOpenFile
	// applies the repository's current-user DACL. Unix permission bits remain
	// authoritative on platforms that implement them.
	for _, tc := range []struct {
		name, platform string
		mode           os.FileMode
		want           bool
	}{
		{"linux_owner_only", "linux", 0600, true},
		{"linux_group_read", "linux", 0640, false},
		{"linux_other_read", "linux", 0604, false},
		{"darwin_world_write", "darwin", 0666, false},
		{"freebsd_owner_read", "freebsd", 0400, true},
		{"windows_reported_writable", "windows", 0666, true},
		{"windows_reported_readonly", "windows", 0444, true},
		{"windows_symlink", "windows", os.ModeSymlink | 0666, false},
		{"windows_directory", "windows", os.ModeDir | 0666, false},
		{"linux_symlink", "linux", os.ModeSymlink | 0600, false},
	} {
		t.Run(tc.name, func(t *testing.T) { Assert.Equal(t, tc.want, keyFileModeAllowed(tc.mode, tc.platform)) })
	}
}
