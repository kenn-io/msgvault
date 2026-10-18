package sync_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	msgsync "go.kenn.io/msgvault/internal/sync"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLoopAutomationSignalsSyncAndRepair(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	mock := gmail.NewMockAPI()
	mock.Profile = &gmail.Profile{EmailAddress: "owner@example.test", MessagesTotal: 1, HistoryID: 1000}
	raw := []byte("From: peer@example.test\r\nTo: owner@example.test\r\nDate: Sat, 17 Oct 2026 10:00:00 +0000\r\nSubject: automation\r\nAuto-Submitted: auto-replied\r\nPrecedence: bulk\r\nList-Unsubscribe: <mailto:unsubscribe@example.test>\r\nReply-To: no-reply@example.test\r\nContent-Type: text/plain\r\n\r\nquestion?\r\n")
	mock.AddMessage("signals", raw, nil)
	syncer := msgsync.New(mock, st, nil)
	_, err := syncer.Full(t.Context(), "owner@example.test")
	requirements.NoError(err)
	var id, sourceID int64
	var auto, bulk, unsubscribe, reply bool
	var version int
	requirements.NoError(st.DB().QueryRow("SELECT id,source_id FROM messages WHERE source_message_id='signals'").Scan(&id, &sourceID))
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT auto_submitted,precedence_bulk,list_unsubscribe,reply_to_noreply,parser_version FROM message_automation_signals WHERE message_id=?"), id).Scan(&auto, &bulk, &unsubscribe, &reply, &version))
	assertions.True(auto)
	assertions.True(bulk)
	assertions.True(unsubscribe)
	assertions.True(reply)
	assertions.Equal(1, version)
	mock.Messages["signals"].Raw = []byte("From: peer@example.test\r\nTo: owner@example.test\r\nSubject: human\r\nContent-Type: text/plain\r\n\r\nquestion?\r\n")
	_, err = syncer.RepairMessage(t.Context(), msgsync.RepairRequest{Reference: "signals", SourceID: sourceID})
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT auto_submitted FROM message_automation_signals WHERE message_id=?"), id).Scan(&auto))
	assertions.False(auto)
}
