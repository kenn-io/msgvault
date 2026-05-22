package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// TestInspectMessage_NotFound verifies that InspectMessage returns sql.ErrNoRows
// when the message does not exist.
func TestInspectMessage_NotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	_, err := st.InspectMessage("nonexistent-msg-id")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("InspectMessage(nonexistent) error = %v, want sql.ErrNoRows", err)
	}
}

// TestInspectMessage_BasicFields verifies that InspectMessage returns correct
// basic fields for a message.
func TestInspectMessage_BasicFields(t *testing.T) {
	f := storetest.New(t)

	// Create a message with a specific source_message_id
	f.CreateMessage("inspect-test-msg")

	insp, err := f.Store.InspectMessage("inspect-test-msg")
	testutil.MustNoErr(t, err, "InspectMessage")

	if insp == nil {
		t.Fatal("InspectMessage returned nil inspection")
	}

	// Thread source ID should be populated from the default thread
	if insp.ThreadSourceID != "default-thread" {
		t.Errorf("ThreadSourceID = %q, want %q", insp.ThreadSourceID, "default-thread")
	}

	// RawDataExists should be false since we didn't add raw data
	if insp.RawDataExists {
		t.Error("RawDataExists should be false when no raw data exists")
	}
}

// TestInspectMessage_WithRawData verifies that InspectMessage correctly detects
// when raw data exists for a message.
func TestInspectMessage_WithRawData(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("inspect-raw-test-msg")

	// Add raw data
	rawData := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody")
	err := f.Store.UpsertMessageRaw(msgID, rawData)
	testutil.MustNoErr(t, err, "UpsertMessageRaw")

	insp, err := f.Store.InspectMessage("inspect-raw-test-msg")
	testutil.MustNoErr(t, err, "InspectMessage")

	if !insp.RawDataExists {
		t.Error("RawDataExists should be true when raw data exists")
	}
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
	testutil.MustNoErr(t, err, "DROP TABLE message_raw")

	// InspectMessage should now return an error when checking raw data existence
	_, err = f.Store.InspectMessage("inspect-db-error-msg")
	if err == nil {
		t.Error("InspectMessage should return error when message_raw table is missing")
	}
}

// TestInspectRawDataExists_NotFound verifies that InspectRawDataExists returns
// false (not an error) when no raw data exists.
func TestInspectRawDataExists_NotFound(t *testing.T) {
	f := storetest.New(t)

	f.CreateMessage("raw-exists-not-found-msg")

	exists, err := f.Store.InspectRawDataExists("raw-exists-not-found-msg")
	testutil.MustNoErr(t, err, "InspectRawDataExists")

	if exists {
		t.Error("InspectRawDataExists should return false when no raw data exists")
	}
}

// TestInspectRawDataExists_Found verifies that InspectRawDataExists returns
// true when raw data exists.
func TestInspectRawDataExists_Found(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("raw-exists-found-msg")

	rawData := []byte("From: test@example.com\r\nSubject: Test\r\n\r\nBody")
	err := f.Store.UpsertMessageRaw(msgID, rawData)
	testutil.MustNoErr(t, err, "UpsertMessageRaw")

	exists, err := f.Store.InspectRawDataExists("raw-exists-found-msg")
	testutil.MustNoErr(t, err, "InspectRawDataExists")

	if !exists {
		t.Error("InspectRawDataExists should return true when raw data exists")
	}
}

// TestInspectRawDataExists_DBError verifies that InspectRawDataExists returns
// DB errors instead of masking them.
func TestInspectRawDataExists_DBError(t *testing.T) {
	f := storetest.New(t)

	f.CreateMessage("raw-exists-db-error-msg")

	// Drop the message_raw table to cause a DB error
	_, err := f.Store.DB().Exec("DROP TABLE message_raw")
	testutil.MustNoErr(t, err, "DROP TABLE message_raw")

	_, err = f.Store.InspectRawDataExists("raw-exists-db-error-msg")
	if err == nil {
		t.Error("InspectRawDataExists should return error when message_raw table is missing")
	}
}

// TestInspectRawDataExists_MessageNotFound verifies that InspectRawDataExists
// returns false when the message itself doesn't exist (no raw data for
// non-existent message).
func TestInspectRawDataExists_MessageNotFound(t *testing.T) {
	st := testutil.NewTestStore(t)

	exists, err := st.InspectRawDataExists("nonexistent-msg")
	testutil.MustNoErr(t, err, "InspectRawDataExists(nonexistent)")

	if exists {
		t.Error("InspectRawDataExists should return false for nonexistent message")
	}
}

// TestInspectMessage_RecipientCounts verifies that InspectMessage correctly
// counts recipients by type.
func TestInspectMessage_RecipientCounts(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("inspect-recipients-msg")

	// Add recipients
	pid1 := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	pid2 := f.EnsureParticipant("bob@example.com", "Bob", "example.com")
	pid3 := f.EnsureParticipant("carol@example.com", "Carol", "example.com")

	err := f.Store.ReplaceMessageRecipients(msgID, "from", []int64{pid1}, []string{"Alice"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients(from)")

	err = f.Store.ReplaceMessageRecipients(msgID, "to", []int64{pid2, pid3}, []string{"Bob", "Carol"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients(to)")

	insp, err := f.Store.InspectMessage("inspect-recipients-msg")
	testutil.MustNoErr(t, err, "InspectMessage")

	if insp.RecipientCounts["from"] != 1 {
		t.Errorf("RecipientCounts[from] = %d, want 1", insp.RecipientCounts["from"])
	}
	if insp.RecipientCounts["to"] != 2 {
		t.Errorf("RecipientCounts[to] = %d, want 2", insp.RecipientCounts["to"])
	}
}

// TestInspectMessage_RecipientDisplayNames verifies that InspectMessage correctly
// returns recipient display names.
func TestInspectMessage_RecipientDisplayNames(t *testing.T) {
	f := storetest.New(t)

	msgID := f.CreateMessage("inspect-display-names-msg")

	pid := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	err := f.Store.ReplaceMessageRecipients(msgID, "from", []int64{pid}, []string{"Custom Display Name"})
	testutil.MustNoErr(t, err, "ReplaceMessageRecipients")

	insp, err := f.Store.InspectMessage("inspect-display-names-msg")
	testutil.MustNoErr(t, err, "InspectMessage")

	key := "from:sender@example.com"
	if insp.RecipientDisplayName[key] != "Custom Display Name" {
		t.Errorf("RecipientDisplayName[%s] = %q, want %q", key, insp.RecipientDisplayName[key], "Custom Display Name")
	}
}
