// Command smoke-fixture creates the smallest production-shaped archive used by
// the release smoke gate. It is a test-data producer, never part of the binary.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: smoke-fixture DATA_DIR")
		os.Exit(2)
	}
	if err := seed(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(dataDir string) error {
	// #nosec G703 -- this validation-only helper writes solely beneath the
	// explicit scratch data directory supplied by smoke-web-release.sh.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create fixture data directory: %w", err)
	}
	st, err := store.Open(filepath.Join(dataDir, "msgvault.db"))
	if err != nil {
		return fmt.Errorf("open fixture archive: %w", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.InitSchema(); err != nil {
		return fmt.Errorf("initialize fixture archive: %w", err)
	}
	source, err := st.GetOrCreateSource("gmail", "release-smoke@example.com")
	if err != nil {
		return fmt.Errorf("create fixture source: %w", err)
	}
	conversationID, err := st.EnsureConversation(source.ID, "release-smoke-thread", "Release smoke")
	if err != nil {
		return fmt.Errorf("create fixture conversation: %w", err)
	}
	senderID, err := st.EnsureParticipant("sender@example.com", "Synthetic Sender", "example.com")
	if err != nil {
		return fmt.Errorf("create fixture participant: %w", err)
	}
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID, SourceMessageID: "release-smoke-message",
		MessageType: "email", SentAt: sql.NullTime{Time: now, Valid: true},
		SenderID:     sql.NullInt64{Int64: senderID, Valid: true},
		Subject:      sql.NullString{String: "Synthetic release smoke", Valid: true},
		Snippet:      sql.NullString{String: "A local release verification message", Valid: true},
		SizeEstimate: 64, HasAttachments: true, AttachmentCount: 1,
	})
	if err != nil {
		return fmt.Errorf("create fixture message: %w", err)
	}
	if err := st.ReplaceMessageRecipients(messageID, "from", []int64{senderID}, []string{"Synthetic Sender"}); err != nil {
		return fmt.Errorf("create fixture sender: %w", err)
	}
	content := []byte("msgvault release smoke attachment\n")
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	attachmentsDir := filepath.Join(dataDir, "attachments")
	contentPath, err := export.StoragePath(attachmentsDir, hash)
	if err != nil {
		return fmt.Errorf("resolve fixture attachment: %w", err)
	}
	// #nosec G703 -- StoragePath validates the content hash and constructs a
	// content-addressed path beneath attachmentsDir.
	if err := os.MkdirAll(filepath.Dir(contentPath), 0o700); err != nil {
		return fmt.Errorf("create fixture attachment directory: %w", err)
	}
	// #nosec G703 -- contentPath is the validated content-addressed path above.
	if err := os.WriteFile(contentPath, content, 0o600); err != nil {
		return fmt.Errorf("write fixture attachment: %w", err)
	}
	storagePath, err := filepath.Rel(attachmentsDir, contentPath)
	if err != nil {
		return fmt.Errorf("make fixture attachment path relative: %w", err)
	}
	if err := st.UpsertAttachment(messageID, "release-smoke.txt", "text/plain", storagePath, hash, len(content)); err != nil {
		return fmt.Errorf("create fixture attachment record: %w", err)
	}
	if err := st.RecomputeMessageAttachmentStats(messageID); err != nil {
		return fmt.Errorf("refresh fixture attachment stats: %w", err)
	}
	fmt.Println(hash)
	return nil
}
