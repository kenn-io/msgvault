package storetest

import (
	"fmt"
	"time"

	"github.com/stretchr/testify/require"
)

// InsertMCPConversationEvent appends a synthetic msgvault.message_archived
// row to mcp_event_log for a conversation scope and moves the clock head to
// seq. It bypasses capture so tests can control recorded_at and epoch.
func (f *Fixture) InsertMCPConversationEvent(epoch, seq, scopeID int64, recorded time.Time) {
	f.T.Helper()
	at := recorded.UTC().Format("2006-01-02T15:04:05.000000000Z")
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO mcp_event_log (seq,epoch,family,kind,scope_kind,scope_id,item_key,message_id,conversation_id,source_id,from_me,occurred_at,recorded_at,data) VALUES (?,?,'msgvault.message_archived','message','conversation',?,?,?,?,?,FALSE,?,?,?)`), seq, epoch, scopeID, fmt.Sprintf("message:%d", seq), seq, scopeID, f.Source.ID, at, at, `{"kind":"message","from_me":false}`)
	require.NoError(f.T, err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE mcp_event_clock SET head_seq=? WHERE singleton=1`), seq)
	require.NoError(f.T, err)
}
