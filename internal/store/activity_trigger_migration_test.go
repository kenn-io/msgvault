package store_test

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// This is the RED regression for the current-main trigger-migration conflict.
// The helper initializes an archive through the current watermark migration,
// then this test proves that activity triggers are installed even though
// message_watermark_triggers_v1 is already recorded. Keep the implementation
// paused until the remote PostgreSQL lease can run this regression.
func TestPostgresActivityTriggersInstallAfterWatermarkMigration(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") &&
		!strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("PostgreSQL trigger migration regression")
	}
	require := require.New(t)
	assert := assert.New(t)
	st, _, _ := newActivityQueryPostgresStores(t, dbURL)

	var watermarkMigrationApplied bool
	err := st.DB().QueryRowContext(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM applied_migrations
			WHERE name = 'message_watermark_triggers_v1'
		)
	`).Scan(&watermarkMigrationApplied)
	require.NoError(err)
	assert.True(watermarkMigrationApplied)

	source, err := st.GetOrCreateSource("gmail", "activity-trigger@example.invalid")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(
		source.ID, "activity-trigger-thread", "Activity trigger")
	require.NoError(err)
	messageID, err := st.UpsertMessage(&store.Message{
		ConversationID:  conversationID,
		SourceID:        source.ID,
		SourceMessageID: "activity-trigger-message",
		MessageType:     "email",
		SentAt:          sql.NullTime{Valid: false},
	})
	require.NoError(err)

	var revision int64
	//nolint:gosec // The query is a fixed test literal; Rebind only changes placeholders.
	err = st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT revision
		FROM activity_projection_queue
		WHERE message_id = ?
	`), messageID).Scan(&revision)
	require.NoError(err,
		"activity trigger migration must install queue triggers on an upgraded archive")
	assert.Equal(int64(1), revision)
}
