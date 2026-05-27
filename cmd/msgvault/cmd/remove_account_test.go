package cmd

import (
	"os"
	"path/filepath"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

// seedAttachmentFile creates a file under attachmentsDir at relPath and returns
// its absolute path. Intermediate directories are created as needed.
func seedAttachmentFile(t *testing.T, attachmentsDir, relPath, content string) string {
	t.Helper()
	absPath := filepath.Join(attachmentsDir, relPath)
	requirepkg.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755), "mkdir %s", filepath.Dir(absPath))
	requirepkg.NoError(t, os.WriteFile(absPath, []byte(content), 0o600), "write %s", absPath)
	return absPath
}

// seedMessageWithAttachment creates a source (if new), conversation, message, and
// attachment row for use in remove-account tests. Returns nothing; callers that
// need IDs should read them back via the store.
func seedMessageWithAttachment(
	t *testing.T, s *store.Store,
	email, threadKey, msgKey, storagePath, contentHash string,
) {
	t.Helper()
	src, err := s.GetOrCreateSource("gmail", email)
	requirepkg.NoError(t, err, "GetOrCreateSource(%s)", email)
	convID, err := s.EnsureConversation(src.ID, threadKey, "Thread")
	requirepkg.NoError(t, err, "EnsureConversation")
	msgID, err := s.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: msgKey,
		MessageType:     "email",
	})
	requirepkg.NoError(t, err, "UpsertMessage")
	requirepkg.NoError(t, s.UpsertAttachment(msgID, "a.pdf", "application/pdf",
		storagePath, contentHash, 0), "UpsertAttachment")
}

func TestRemoveAccountCmd_DeletesUniqueAttachmentFiles(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"aa/hashA", "hashA")
	_ = s.Close()

	filePath := seedAttachmentFile(t, attachmentsDir, "aa/hashA", "content-a")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account", "alice@example.com", "--yes"})
	require.NoError(root.Execute(), "remove-account")

	_, err = os.Stat(filePath)
	assertpkg.True(t, os.IsNotExist(err), "expected attachment file deleted, err = %v", err)
}

func TestRemoveAccountCmd_PreservesSharedAttachments(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	// Both accounts reference the same content_hash/storage_path.
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"bb/sharedhash", "sharedhash")
	seedMessageWithAttachment(t, s,
		"bob@example.com", "thread-b", "msg-b",
		"bb/sharedhash", "sharedhash")
	_ = s.Close()

	filePath := seedAttachmentFile(t, attachmentsDir, "bb/sharedhash", "shared-content")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account", "alice@example.com", "--yes"})
	require.NoError(root.Execute(), "remove-account")

	_, err = os.Stat(filePath)
	assertpkg.NoError(t, err, "shared attachment file should be preserved")
}

func TestRemoveAccountCmd_SkipsDeletionDuringActiveSync(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"cc/hashA", "hashA")
	// Simulate a concurrent sync on an unrelated source.
	otherSrc, err := s.GetOrCreateSource("gmail", "bob@example.com")
	require.NoError(err, "create other source")
	_, err = s.StartSync(otherSrc.ID, "full")
	require.NoError(err, "StartSync")
	_ = s.Close()

	filePath := seedAttachmentFile(t, attachmentsDir, "cc/hashA", "content-a")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account", "alice@example.com", "--yes"})
	require.NoError(root.Execute(), "remove-account")

	// File should remain because an active sync on another source blocks deletion.
	_, err = os.Stat(filePath)
	require.NoError(err, "attachment file should be preserved while another sync is active")

	// DB cleanup still runs — account is gone.
	s2, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "reopen store")
	defer func() { _ = s2.Close() }()
	require.NoError(s2.InitSchema(), "reinit schema")
	src, err := s2.GetSourceByIdentifier("alice@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assert.Nil(src, "source should have been removed from DB despite skipped file deletion")
}

// Regression test: if the account being removed has its own active sync,
// RemoveSource's cascade deletes that sync_runs row. A post-RemoveSource
// HasAnyActiveSync would return false and the deletion loop would run even
// though the sync worker may still be writing attachment files. The
// pre-RemoveSource check must catch this and skip file deletion.
func TestRemoveAccountCmd_SkipsDeletionWhenRemovedAccountHasActiveSync(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"dd/hashA", "hashA")
	aliceSrc, err := s.GetSourceByIdentifier("alice@example.com")
	require.NoError(err, "GetSourceByIdentifier")
	require.NotNil(aliceSrc, "expected alice source to exist")
	// Active sync on the account being removed — this is the row that
	// RemoveSource cascades away.
	_, err = s.StartSync(aliceSrc.ID, "full")
	require.NoError(err, "StartSync")
	_ = s.Close()

	filePath := seedAttachmentFile(t, attachmentsDir, "dd/hashA", "content-a")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	// --yes bypasses the initial GetActiveSync guard so we exercise the
	// later file-deletion path.
	root.SetArgs([]string{"remove-account", "alice@example.com", "--yes"})
	require.NoError(root.Execute(), "remove-account")

	_, err = os.Stat(filePath)
	assertpkg.NoError(t, err, "attachment file should be preserved when the removed account has an active sync")
}

func TestRemoveAccountCmd_RejectsPathTraversal(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")
	require.NoError(os.MkdirAll(attachmentsDir, 0o755), "mkdir attachments")

	// Create a file outside the attachments directory that MUST NOT be deleted.
	outsidePath := filepath.Join(tmpDir, "escape.txt")
	require.NoError(os.WriteFile(outsidePath, []byte("do not delete"), 0o600), "write outside file")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	// Craft a storage_path that escapes the attachments directory.
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"../escape.txt", "evilhash")
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account", "alice@example.com", "--yes"})
	require.NoError(root.Execute(), "remove-account")

	_, err = os.Stat(outsidePath)
	assertpkg.NoError(t, err, "file outside attachments dir must not be deleted")
}

func TestRemoveAccountCmd_RequiresEmail(t *testing.T) {
	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account"})

	requirepkg.Error(t, root.Execute(), "expected error for missing email arg")
}

func TestRemoveAccountCmd_NotFound(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{
		"remove-account", "nobody@example.com", "--yes",
	})

	err = root.Execute()
	require.Error(err, "expected error for unknown email")
	assertpkg.ErrorContains(t, err, "not found")
}

func TestRemoveAccountCmd_WithYesFlag(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("gmail", "test@example.com")
	require.NoError(err, "create source")
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{
		"remove-account", "test@example.com", "--yes",
	})

	require.NoError(root.Execute(), "remove-account --yes")

	// Verify account is gone
	s, err = store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "reinit schema")

	src, err := s.GetSourceByIdentifier("test@example.com")
	require.ErrorIs(err, store.ErrSourceNotFound, "GetSourceByIdentifier")
	assertpkg.Nil(t, src, "account should be removed after --yes")
}

func TestRemoveAccountCmd_DuplicateIdentifierRequiresType(
	t *testing.T,
) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("gmail", "dup@example.com")
	require.NoError(err, "create gmail source")
	_, err = s.GetOrCreateSource("mbox", "dup@example.com")
	require.NoError(err, "create mbox source")
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	// Without --type should fail
	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{
		"remove-account", "dup@example.com", "--yes",
	})

	err = root.Execute()
	require.Error(err, "expected error for ambiguous identifier")
	require.ErrorContains(err, "multiple accounts")

	// With --type should succeed
	root2 := newTestRootCmd()
	root2.AddCommand(newRemoveAccountCmd())
	root2.SetArgs([]string{
		"remove-account", "dup@example.com",
		"--yes", "--type", "mbox",
	})

	require.NoError(root2.Execute(), "remove-account --type mbox")

	// Verify only mbox source was removed
	s, err = store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "reinit schema")

	sources, err := s.GetSourcesByIdentifier("dup@example.com")
	require.NoError(err, "GetSourcesByIdentifier")
	require.Len(sources, 1)
	assert.Equal("gmail", sources[0].SourceType, "remaining source type")
}

func TestRemoveAccountCmd_GmailRemovesToken(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"
	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	_, err = s.GetOrCreateSource("gmail", "tok@example.com")
	require.NoError(err, "create source")
	_ = s.Close()

	// Create a fake token file
	tokenPath := oauth.TokenFilePath(tokensDir, "tok@example.com")
	require.NoError(os.WriteFile(tokenPath, []byte(`{}`), 0600), "write token")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{
		"remove-account", "tok@example.com", "--yes",
	})

	require.NoError(root.Execute(), "remove-account")

	_, err = os.Stat(tokenPath)
	assertpkg.True(t, os.IsNotExist(err), "token file should be removed for gmail source")
}

func TestRemoveAccountCmd_NonGmailSkipsToken(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"
	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	_, err = s.GetOrCreateSource("mbox", "imp@example.com")
	require.NoError(err, "create source")
	_ = s.Close()

	// Create a token file that should NOT be removed
	tokenPath := oauth.TokenFilePath(tokensDir, "imp@example.com")
	require.NoError(os.WriteFile(tokenPath, []byte(`{}`), 0600), "write token")

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{
		"remove-account", "imp@example.com", "--yes",
	})

	require.NoError(root.Execute(), "remove-account")

	_, err = os.Stat(tokenPath)
	assertpkg.False(t, os.IsNotExist(err), "token file should NOT be removed for non-gmail source")
}

func TestResolveSource_IMAPDisplayName(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	// Create an IMAP source whose identifier is a URL, display_name is the email.
	src, err := s.GetOrCreateSource("imap", "imaps://user%40outlook.com@outlook.office365.com:993")
	require.NoError(err, "create source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, "user@outlook.com"), "set display name")
	_ = s.Close()

	s2, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	require.NoError(s2.InitSchema(), "reinit schema")
	defer func() { _ = s2.Close() }()

	found, err := resolveSource(s2, "user@outlook.com", "")
	require.NoError(err, "resolveSource by display name")
	assertpkg.Equal(t, "imaps://user%40outlook.com@outlook.office365.com:993", found.Identifier, "identifier should be IMAP URL")
}

func TestRemoveAccountCmd_ClosedStdinReturnsError(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	_, err = s.GetOrCreateSource("gmail", "eof@example.com")
	require.NoError(err, "create source")
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	// Replace stdin with a closed pipe to simulate EOF
	r, w, err := os.Pipe()
	require.NoError(err, "create pipe")
	_ = w.Close()

	origStdin := os.Stdin
	os.Stdin = r
	defer func() {
		os.Stdin = origStdin
		_ = r.Close()
	}()

	// Run WITHOUT --yes so it tries to read confirmation
	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account", "eof@example.com"})

	err = root.Execute()
	require.Error(err, "expected error when stdin is closed")
	assertpkg.ErrorContains(t, err, "use --yes")
}
