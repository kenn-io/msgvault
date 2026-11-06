package cmd

import (
	"fmt"

	"go.kenn.io/msgvault/internal/store"
)

func initializeMailboxSnapshot(snapshot *cacheSourceSnapshot) (*cacheSourceSnapshot, error) {
	var count int
	if err := snapshot.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='provider_mailbox_observations'").Scan(&count); err != nil {
		_ = snapshot.Close()
		return nil, fmt.Errorf("inspect provider mailbox observations: %w", err)
	}
	snapshot.hasMailboxObservations = count > 0
	if buildCacheAfterSnapshotInitializationHook != nil {
		buildCacheAfterSnapshotInitializationHook()
	}
	return snapshot, nil
}

func (s *cacheSourceSnapshot) mailboxStateSelectSQL(prefix string) string {
	if !s.hasMailboxObservations {
		return "SELECT CAST(NULL AS BIGINT) AS message_id,CAST(NULL AS VARCHAR) AS inbox_state,CAST(NULL AS VARCHAR) AS read_state WHERE FALSE"
	}
	return store.MailboxStateSelectSQL(prefix)
}

func (s *cacheSourceSnapshot) mailboxStateExportSQL(through int64) string {
	projection := s.mailboxStateSelectSQL("sqlite_db.")
	if s.csvSnapshot {
		projection = "SELECT * FROM sqlite_db.mailbox_state"
	}
	return fmt.Sprintf("SELECT CAST(message_id AS BIGINT) AS message_id,inbox_state,read_state FROM (%s) state WHERE message_id<=%d", projection, through)
}
