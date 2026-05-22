package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
)

// seedAttachmentFile creates a file under attachmentsDir at relPath and returns
// its absolute path. Intermediate directories are created as needed.
func seedAttachmentFile(t *testing.T, attachmentsDir, relPath, content string) string {
	t.Helper()
	absPath := filepath.Join(attachmentsDir, relPath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(absPath), err)
	}
	if err := os.WriteFile(absPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", absPath, err)
	}
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
	if err != nil {
		t.Fatalf("GetOrCreateSource(%s): %v", email, err)
	}
	convID, err := s.EnsureConversation(src.ID, threadKey, "Thread")
	if err != nil {
		t.Fatalf("EnsureConversation: %v", err)
	}
	msgID, err := s.UpsertMessage(&store.Message{
		ConversationID:  convID,
		SourceID:        src.ID,
		SourceMessageID: msgKey,
		MessageType:     "email",
	})
	if err != nil {
		t.Fatalf("UpsertMessage: %v", err)
	}
	if err := s.UpsertAttachment(msgID, "a.pdf", "application/pdf",
		storagePath, contentHash, 0); err != nil {
		t.Fatalf("UpsertAttachment: %v", err)
	}
}

func TestRemoveAccountCmd_DeletesUniqueAttachmentFiles(t *testing.T) {
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
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
	if err := root.Execute(); err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Errorf("expected attachment file deleted, err = %v", err)
	}
}

func TestRemoveAccountCmd_PreservesSharedAttachments(t *testing.T) {
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
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
	if err := root.Execute(); err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	if _, err := os.Stat(filePath); err != nil {
		t.Errorf("shared attachment file should be preserved, err = %v", err)
	}
}

func TestRemoveAccountCmd_SkipsDeletionDuringActiveSync(t *testing.T) {
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"cc/hashA", "hashA")
	// Simulate a concurrent sync on an unrelated source.
	otherSrc, err := s.GetOrCreateSource("gmail", "bob@example.com")
	if err != nil {
		t.Fatalf("create other source: %v", err)
	}
	if _, err := s.StartSync(otherSrc.ID, "full"); err != nil {
		t.Fatalf("StartSync: %v", err)
	}
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
	if err := root.Execute(); err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	// File should remain because an active sync on another source blocks deletion.
	if _, err := os.Stat(filePath); err != nil {
		t.Errorf(
			"attachment file should be preserved while another sync is active, err = %v",
			err,
		)
	}

	// DB cleanup still runs — account is gone.
	s2, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.InitSchema(); err != nil {
		t.Fatalf("reinit schema: %v", err)
	}
	src, err := s2.GetSourceByIdentifier("alice@example.com")
	if err != nil {
		t.Fatalf("GetSourceByIdentifier: %v", err)
	}
	if src != nil {
		t.Error("source should have been removed from DB despite skipped file deletion")
	}
}

// Regression test: if the account being removed has its own active sync,
// RemoveSource's cascade deletes that sync_runs row. A post-RemoveSource
// HasAnyActiveSync would return false and the deletion loop would run even
// though the sync worker may still be writing attachment files. The
// pre-RemoveSource check must catch this and skip file deletion.
func TestRemoveAccountCmd_SkipsDeletionWhenRemovedAccountHasActiveSync(t *testing.T) {
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	seedMessageWithAttachment(t, s,
		"alice@example.com", "thread-a", "msg-a",
		"dd/hashA", "hashA")
	aliceSrc, err := s.GetSourceByIdentifier("alice@example.com")
	if err != nil {
		t.Fatalf("GetSourceByIdentifier: %v", err)
	}
	if aliceSrc == nil {
		t.Fatal("expected alice source to exist")
	}
	// Active sync on the account being removed — this is the row that
	// RemoveSource cascades away.
	if _, err := s.StartSync(aliceSrc.ID, "full"); err != nil {
		t.Fatalf("StartSync: %v", err)
	}
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
	if err := root.Execute(); err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	if _, err := os.Stat(filePath); err != nil {
		t.Errorf(
			"attachment file should be preserved when the removed account "+
				"has an active sync, err = %v",
			err,
		)
	}
}

func TestRemoveAccountCmd_RejectsPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	attachmentsDir := filepath.Join(tmpDir, "attachments")
	if err := os.MkdirAll(attachmentsDir, 0o755); err != nil {
		t.Fatalf("mkdir attachments: %v", err)
	}

	// Create a file outside the attachments directory that MUST NOT be deleted.
	outsidePath := filepath.Join(tmpDir, "escape.txt")
	if err := os.WriteFile(outsidePath, []byte("do not delete"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
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
	if err := root.Execute(); err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	if _, err := os.Stat(outsidePath); err != nil {
		t.Errorf(
			"file outside attachments dir must not be deleted, err = %v",
			err,
		)
	}
}

func TestRemoveAccountCmd_RequiresEmail(t *testing.T) {
	root := newTestRootCmd()
	root.AddCommand(newRemoveAccountCmd())
	root.SetArgs([]string{"remove-account"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for missing email arg")
	}
}

func TestRemoveAccountCmd_NotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected error for unknown email")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err.Error())
	}
}

func TestRemoveAccountCmd_WithYesFlag(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = s.GetOrCreateSource("gmail", "test@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
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

	err = root.Execute()
	if err != nil {
		t.Fatalf("remove-account --yes: %v", err)
	}

	// Verify account is gone
	s, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("reinit schema: %v", err)
	}

	src, err := s.GetSourceByIdentifier("test@example.com")
	if err != nil {
		t.Fatalf("GetSourceByIdentifier: %v", err)
	}
	if src != nil {
		t.Error("account should be removed after --yes")
	}
}

func TestRemoveAccountCmd_DuplicateIdentifierRequiresType(
	t *testing.T,
) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = s.GetOrCreateSource("gmail", "dup@example.com")
	if err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	_, err = s.GetOrCreateSource("mbox", "dup@example.com")
	if err != nil {
		t.Fatalf("create mbox source: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected error for ambiguous identifier")
	}
	if !strings.Contains(err.Error(), "multiple accounts") {
		t.Errorf("error = %q, want 'multiple accounts'", err.Error())
	}

	// With --type should succeed
	root2 := newTestRootCmd()
	root2.AddCommand(newRemoveAccountCmd())
	root2.SetArgs([]string{
		"remove-account", "dup@example.com",
		"--yes", "--type", "mbox",
	})

	err = root2.Execute()
	if err != nil {
		t.Fatalf("remove-account --type mbox: %v", err)
	}

	// Verify only mbox source was removed
	s, err = store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("reinit schema: %v", err)
	}

	sources, err := s.GetSourcesByIdentifier("dup@example.com")
	if err != nil {
		t.Fatalf("GetSourcesByIdentifier: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("got %d sources, want 1", len(sources))
	}
	if sources[0].SourceType != "gmail" {
		t.Errorf(
			"remaining source type = %q, want gmail",
			sources[0].SourceType,
		)
	}
}

func TestRemoveAccountCmd_GmailRemovesToken(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"
	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	_, err = s.GetOrCreateSource("gmail", "tok@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	_ = s.Close()

	// Create a fake token file
	tokenPath := oauth.TokenFilePath(tokensDir, "tok@example.com")
	if err := os.WriteFile(tokenPath, []byte(`{}`), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

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

	err = root.Execute()
	if err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Error("token file should be removed for gmail source")
	}
}

func TestRemoveAccountCmd_NonGmailSkipsToken(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"
	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	_, err = s.GetOrCreateSource("mbox", "imp@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	_ = s.Close()

	// Create a token file that should NOT be removed
	tokenPath := oauth.TokenFilePath(tokensDir, "imp@example.com")
	if err := os.WriteFile(tokenPath, []byte(`{}`), 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

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

	err = root.Execute()
	if err != nil {
		t.Fatalf("remove-account: %v", err)
	}

	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		t.Error("token file should NOT be removed for non-gmail source")
	}
}

func TestResolveSource_IMAPDisplayName(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Create an IMAP source whose identifier is a URL, display_name is the email.
	src, err := s.GetOrCreateSource("imap", "imaps://user%40outlook.com@outlook.office365.com:993")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	if err := s.UpdateSourceDisplayName(src.ID, "user@outlook.com"); err != nil {
		t.Fatalf("set display name: %v", err)
	}
	_ = s.Close()

	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	if err := s2.InitSchema(); err != nil {
		t.Fatalf("reinit schema: %v", err)
	}
	defer func() { _ = s2.Close() }()

	found, err := resolveSource(s2, "user@outlook.com", "")
	if err != nil {
		t.Fatalf("resolveSource by display name: %v", err)
	}
	if found.Identifier != "imaps://user%40outlook.com@outlook.office365.com:993" {
		t.Errorf("got identifier %q, want IMAP URL", found.Identifier)
	}
}

func TestRemoveAccountCmd_ClosedStdinReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	_, err = s.GetOrCreateSource("gmail", "eof@example.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	defer func() { cfg = savedCfg }()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}

	// Replace stdin with a closed pipe to simulate EOF
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("create pipe: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected error when stdin is closed")
	}
	if !strings.Contains(err.Error(), "use --yes") {
		t.Errorf("error = %q, want 'use --yes'", err.Error())
	}
}
