package store_test

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMCPEventsSchemaStartsDisabled(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	var head, floor, epoch int64
	var enabled bool
	err := f.Store.DB().QueryRow(`SELECT head_seq, pruned_through_seq, capture_epoch, enabled FROM mcp_event_clock WHERE singleton = 1`).Scan(&head, &floor, &epoch, &enabled)
	require.NoError(err)
	assert.Equal(int64(0), head)
	assert.Equal(int64(0), floor)
	assert.Equal(int64(0), epoch)
	assert.False(enabled)
	for _, table := range []string{"mcp_event_log", "mcp_event_subscriptions", "mcp_event_dead_letters", "mcp_live_admissions", "mcp_event_message_refs"} {
		var count int
		require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count))
		assert.Zero(count)
	}
	for _, table := range []string{"gmail_drafts", "imap_drafts", "beeper_drafts", "chat_drafts"} {
		rows, err := f.Store.DB().Query(`SELECT created_by_principal FROM ` + table + ` WHERE 1 = 0`)
		require.NoError(err)
		defer func() { require.NoError(rows.Close()) }()
		assert.False(rows.Next())
		require.NoError(rows.Err())
	}
}

func TestMCPEventMessageReferencesCascadeWithoutDeletingReceipt(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("synthetic-retained-message")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_message_refs (message_id,reference_seq) VALUES (?,1)`), messageID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,message_reference_seq,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (1,1,'msgvault.message_archived','message','conversation',?,'message:1',?,1,?,?,FALSE,'2026-10-05T12:00:00.000000000Z','2026-10-05T12:00:00.000000000Z','{}')`), f.ConvID, messageID, f.ConvID, f.Source.ID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), messageID)
	require.NoError(err)
	var refs, receipts int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_message_refs`).Scan(&refs))
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&receipts))
	assert.Zero(refs)
	assert.Equal(1, receipts)
}
