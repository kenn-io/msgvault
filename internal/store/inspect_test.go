package store_test

import (
	"database/sql"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestInspectMessage_NotFound verifies that InspectMessage returns sql.ErrNoRows
// when the message does not exist.
func TestInspectMessage_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, err := st.InspectMessage("nonexistent-msg-id")
	assertpkg.ErrorIs(t, err, sql.ErrNoRows, "InspectMessage(nonexistent)")
}

// TestInspectMessage_BasicFields verifies that InspectMessage returns correct
// basic fields for a message.
func TestInspectMessage_BasicFields(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	// Create a message with a specific source_message_id
	f.CreateMessage("inspect-test-msg")

	insp, err := f.Store.InspectMessage("inspect-test-msg")
	require.NoError(err, "InspectMessage")

	require.NotNil(insp, "InspectMessage returned nil inspection")

	// Thread source ID should be populated from the default thread
	assert.Equal("default-thread", insp.ThreadSourceID)

	// RawDataExists should be false since we didn't add raw data
	assert.False(insp.RawDataExists, "RawDataExists should be false when no raw data exists")
}

// TestInspectMessage_WithRawData verifies that InspectMessage correctly detects
// when raw data exists for a message.
func TestInspectMessage_WithRawData(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("inspect-raw-test-msg")

	// Add raw data
	rawData := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody")
	err := f.Store.UpsertMessageRaw(msgID, rawData)
	requirepkg.NoError(t, err, "UpsertMessageRaw")

	insp, err := f.Store.InspectMessage("inspect-raw-test-msg")
	requirepkg.NoError(t, err, "InspectMessage")

	assertpkg.True(t, insp.RawDataExists, "RawDataExists should be true when raw data exists")
}

// TestInspectMessage_DBError verifies that InspectMessage returns DB errors
// instead of masking them. This tests the behavior where errors from the
// raw-data existence query are propagated rather than treated as "no rows".
func TestInspectMessage_DBError(t *testing.T) {
	f := storetest.New(t)

	// Create a message first
	f.CreateMessage("inspect-db-error-msg")

	// Drop the message_raw table to cause a DB error during the raw check
	_, err := f.Store.DB().Exec("DROP TABLE message_raw")
	requirepkg.NoError(t, err, "DROP TABLE message_raw")

	// InspectMessage should now return an error when checking raw data existence
	_, err = f.Store.InspectMessage("inspect-db-error-msg")
	assertpkg.Error(t, err, "InspectMessage should return error when message_raw table is missing")
}

// TestInspectRawDataExists_NotFound verifies that InspectRawDataExists returns
// false (not an error) when no raw data exists.
func TestInspectRawDataExists_NotFound(t *testing.T) {
	f := storetest.New(t)

	f.CreateMessage("raw-exists-not-found-msg")

	exists, err := f.Store.InspectRawDataExists("raw-exists-not-found-msg")
	requirepkg.NoError(t, err, "InspectRawDataExists")

	assertpkg.False(t, exists, "InspectRawDataExists should return false when no raw data exists")
}

// TestInspectRawDataExists_Found verifies that InspectRawDataExists returns
// true when raw data exists.
func TestInspectRawDataExists_Found(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("raw-exists-found-msg")

	rawData := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody")
	err := f.Store.UpsertMessageRaw(msgID, rawData)
	requirepkg.NoError(t, err, "UpsertMessageRaw")

	exists, err := f.Store.InspectRawDataExists("raw-exists-found-msg")
	requirepkg.NoError(t, err, "InspectRawDataExists")

	assertpkg.True(t, exists, "InspectRawDataExists should return true when raw data exists")
}

// TestInspectRawDataExists_DBError verifies that InspectRawDataExists returns
// DB errors instead of masking them.
func TestInspectRawDataExists_DBError(t *testing.T) {
	f := storetest.New(t)

	f.CreateMessage("raw-exists-db-error-msg")

	// Drop the message_raw table to cause a DB error
	_, err := f.Store.DB().Exec("DROP TABLE message_raw")
	requirepkg.NoError(t, err, "DROP TABLE message_raw")

	_, err = f.Store.InspectRawDataExists("raw-exists-db-error-msg")
	assertpkg.Error(t, err, "InspectRawDataExists should return error when message_raw table is missing")
}

// TestInspectRawDataExists_MessageNotFound verifies that InspectRawDataExists
// returns false when the message itself doesn't exist (no raw data for
// non-existent message).
func TestInspectRawDataExists_MessageNotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	exists, err := st.InspectRawDataExists("nonexistent-msg")
	requirepkg.NoError(t, err, "InspectRawDataExists(nonexistent)")

	assertpkg.False(t, exists, "InspectRawDataExists should return false for nonexistent message")
}

// TestInspectMessage_RecipientCounts verifies that InspectMessage correctly
// counts recipients by type.
func TestInspectMessage_RecipientCounts(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	f := storetest.New(t)

	msgID := f.CreateMessage("inspect-recipients-msg")

	// Add recipients
	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	pid2 := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	pid3 := f.EnsureParticipant("carol@example.com", "Carol", "example.com")

	err := f.Store.ReplaceMessageRecipients(msgID, "from", []int64{pid1}, []string{"Alice"})
	require.NoError(err, "ReplaceMessageRecipients(from)")

	err = f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid2, pid3}, []string{"Bob", "Carol"})
	require.NoError(err, "ReplaceMessageRecipients(to)")

	insp, err := f.Store.InspectMessage("inspect-recipients-msg")
	require.NoError(err, "InspectMessage")

	assert.Equal(1, insp.RecipientCounts["from"], "RecipientCounts[from]")
	assert.Equal(2, insp.RecipientCounts["to"], "RecipientCounts[to]")
}

// TestInspectMessage_RecipientDisplayNames verifies that InspectMessage correctly
// returns recipient display names.
func TestInspectMessage_RecipientDisplayNames(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("inspect-display-names-msg")

	pid := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err := f.Store.ReplaceMessageRecipients(msgID, "from", []int64{pid}, []string{"Custom Display Name"})
	requirepkg.NoError(t, err, "ReplaceMessageRecipients")

	insp, err := f.Store.InspectMessage("inspect-display-names-msg")
	requirepkg.NoError(t, err, "InspectMessage")

	key := "from:sender@example.com"
	assertpkg.Equal(t, "Custom Display Name", insp.RecipientDisplayName[key], "RecipientDisplayName[%s]", key)
}
