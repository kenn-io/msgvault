package store_test

import (
	"database/sql"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func mcpReferenceFixture(t *testing.T) (*storetest.Fixture, *store.MCPSubscription, int64, time.Time) {
	t.Helper()
	f := storetest.New(t)
	now := time.Now().UTC()
	clock, err := f.Store.ConfigureMCPEvents(t.Context(), mcpStoreConfig())
	Require.NoError(t, err)
	active, _, err := f.Store.ActivateMCPSubscription(t.Context(), store.MCPActivation{Subscription: newMCPStoreSubscription(t, f, 1, now), Now: now})
	Require.NoError(t, err)
	id := f.CreateMessage("synthetic-original-message")
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_message_refs (message_id,reference_seq) VALUES (?,1)`), id)
	Require.NoError(t, err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,message_reference_seq,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (1,?,'msgvault.message_archived','message','conversation',?,'message:1',?,1,?,?,FALSE,?,?,?)`), clock.Epoch, f.ConvID, id, f.ConvID, f.Source.ID, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), `{"kind":"message","from_me":false}`)
	Require.NoError(t, err)
	_, err = f.Store.DB().Exec(`UPDATE mcp_event_clock SET head_seq=1 WHERE singleton=1`)
	Require.NoError(t, err)
	return f, active, id, now
}

func TestMCPEventsReferenceReadReturnsCurrentSurvivingMessage(t *testing.T) {
	f, active, id, now := mcpReferenceFixture(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET subject='Synthetic edited subject' WHERE id=?`), id)
	Require.NoError(t, err)
	var subject string
	err = f.Store.ReadMCPEventMessage(t.Context(), active.ID, 1, active.Principal, id, now, func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), f.Store.Rebind(`SELECT subject FROM messages WHERE id=?`), id).Scan(&subject)
	})
	Require.NoError(t, err)
	Assert.Equal(t, "Synthetic edited subject", subject)
}

func TestMCPEventsReferenceReadRejectsPhysicallyReusedMessageID(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f, active, id, now := mcpReferenceFixture(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM messages WHERE id=?`), id)
	require.NoError(err)
	var refs int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_message_refs`).Scan(&refs))
	assert.Zero(refs, "physical deletion must cascade only the live reference")
	_, err = f.Store.GetMCPEvent(t.Context(), active.ID, 1, active.Principal, now)
	require.NoError(err, "the retained occurrence survives deletion")
	// Explicit reuse also exercises PostgreSQL, where generated IDs normally
	// increase. SQLite can reuse the highest rowid without an explicit ID.
	override := ""
	if f.Store.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO messages (id,source_id,conversation_id,source_message_id,message_type,subject)`+override+` VALUES (?,?,?,'synthetic-replacement-message','email','Synthetic replacement subject')`), id, f.Source.ID, f.ConvID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_message_refs (message_id,reference_seq) VALUES (?,2)`), id)
	require.NoError(err)
	called := false
	err = f.Store.ReadMCPEventMessage(t.Context(), active.ID, 1, active.Principal, id, now, func(*sql.Tx) error {
		called = true
		return nil
	})
	require.Error(err)
	assert.Equal("message_unavailable", err.Error())
	assert.False(called, "reject the replacement before reading any content")
}

func TestMCPEventsReferenceReadRejectsAnotherRequestedMessage(t *testing.T) {
	f, active, _, now := mcpReferenceFixture(t)
	otherID := f.CreateMessage("synthetic-unrelated-message")
	called := false
	err := f.Store.ReadMCPEventMessage(t.Context(), active.ID, 1, active.Principal, otherID, now, func(*sql.Tx) error {
		called = true
		return nil
	})
	Require.Error(t, err)
	Assert.Equal(t, "message_unavailable", err.Error())
	Assert.False(t, called)
}

func TestMCPEventsReferenceReadUsesOneSnapshot(t *testing.T) {
	f, active, id, now := mcpReferenceFixture(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET subject='Synthetic original subject' WHERE id=?`), id)
	Require.NoError(t, err)
	// A separate writer commits after the read guard. Both backends must
	// retain the old snapshot for the callback's subsequent detail query.
	writerDone := make(chan error, 1)
	var subject string
	err = f.Store.ReadMCPEventMessage(t.Context(), active.ID, 1, active.Principal, id, now, func(tx *sql.Tx) error {
		go func() {
			_, writeErr := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET subject='Synthetic replacement subject' WHERE id=?`), id)
			writerDone <- writeErr
		}()
		if writeErr := <-writerDone; writeErr != nil {
			return writeErr
		}
		return tx.QueryRowContext(t.Context(), f.Store.Rebind(`SELECT subject FROM messages WHERE id=?`), id).Scan(&subject)
	})
	Require.NoError(t, err)
	Assert.Equal(t, "Synthetic original subject", subject)
}
