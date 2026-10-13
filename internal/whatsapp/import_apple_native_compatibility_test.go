package whatsapp

import (
	"path/filepath"
	"strconv"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportWhatsAppAppleNotifyingEmptyChatRetainsNativeTrace(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := createAppleChatFixture(t)
	execAppleFixture(t, path, `DELETE FROM ZWAMESSAGE; DELETE FROM ZWAGROUPMEMBER; DELETE FROM ZWACHATSESSION WHERE Z_PK<>1`)
	st := testutil.NewTestStore(t)
	configureSyntheticWhatsAppImports(t, st, true)
	summary, err := NewImporter(st, nil).Import(t.Context(), path, ImportOptions{Phone: "+15555550100", NotifyImports: true})
	require.NoError(err)
	assert.Zero(summary.MessagesAdded)
	for table, want := range map[string]int{"participants": 2, "conversations": 1, "conversation_participants": 2, "messages": 0, "mcp_event_log": 0} {
		var got int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got))
		assert.Equal(want, got, table)
	}
}

func TestImportWhatsAppAppleNativeAdditiveMembershipRetainsObservedRole(t *testing.T) {
	for _, notify := range []bool{false, true} {
		t.Run(strconv.FormatBool(notify), func(t *testing.T) {
			require := Require.New(t)

			path := createAppleChatFixture(t)
			createAppleLIDFixture(t, filepath.Dir(path))
			st := testutil.NewTestStore(t)
			configureSyntheticWhatsAppImports(t, st, true)
			opts := ImportOptions{Phone: "+15555550100"}
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err)
			execAppleFixture(t, path, `UPDATE ZWAGROUPMEMBER SET ZISADMIN=0 WHERE Z_PK=10; UPDATE ZWAMESSAGE SET ZTEXT='synthetic edited group text' WHERE Z_PK=3`)
			opts.NotifyImports, opts.Full = notify, true
			_, err = NewImporter(st, nil).Import(t.Context(), path, opts)
			require.NoError(err)
			var role string
			require.NoError(st.DB().QueryRow(`SELECT cp.role FROM conversation_participants cp JOIN conversations c ON c.id=cp.conversation_id JOIN participants p ON p.id=cp.participant_id WHERE c.source_conversation_id='120363000000000000@g.us' AND p.phone_number='+15555550103'`).Scan(&role))
			Assert.Equal(t, "admin", role, "native imports retain the first observed membership role")
		})
	}
}

func TestImportWhatsAppAppleNotifyingCanonicalSnapshotSurvivesDerivedFTSFailure(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := createAppleChatFixture(t)
	execAppleFixture(t, path, `DELETE FROM ZWAMESSAGE WHERE Z_PK<>1; DELETE FROM ZWAGROUPMEMBER; DELETE FROM ZWACHATSESSION WHERE Z_PK<>1`)
	st := testutil.NewTestStore(t)
	configureSyntheticWhatsAppImports(t, st, true)
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`ALTER TABLE messages ADD CONSTRAINT synthetic_whatsapp_fts_failure CHECK (search_fts IS NULL)`)
	} else {
		_, err = st.DB().Exec(`DROP TABLE messages_fts`)
	}
	require.NoError(err)
	summary, err := NewImporter(st, nil).Import(t.Context(), path, ImportOptions{Phone: "+15555550100", NotifyImports: true})
	require.NoError(err, "the ready archive does not depend on its derived search index")
	assert.Equal(int64(1), summary.MessagesAdded)
	for _, table := range []string{"messages", "message_bodies", "message_raw", "import_message_decisions", "mcp_event_log"} {
		var n int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		assert.Equal(1, n, table)
	}
}

func TestImportWhatsAppAppleNotifyingRerunSkipsUnchangedMessages(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := createApplePushNameFixture(t)
	createAppleLIDFixture(t, filepath.Dir(path))
	st := testutil.NewTestStore(t)
	configureSyntheticWhatsAppImports(t, st, true)
	opts := ImportOptions{Phone: "+15555550100", DisplayName: "Synthetic Owner", NotifyImports: true, BatchSize: 2}
	first, err := NewImporter(st, nil).Import(t.Context(), path, opts)
	require.NoError(err)
	assert.Equal(int64(5), first.MessagesAdded)
	markAppleMessagesUnwritten(t, st)
	for range 2 {
		opts.Full = !opts.Full
		next, err := NewImporter(st, nil).Import(t.Context(), path, opts)
		require.NoError(err)
		assert.Zero(next.MessagesAdded)
		assert.Empty(rewrittenAppleMessages(t, st))
	}
	var name string
	require.NoError(st.DB().QueryRow(`SELECT display_name FROM participants WHERE phone_number='+15555550103'`).Scan(&name))
	assert.Equal("Push Roster Only", name)
	require.NoError(st.DB().QueryRow(`SELECT display_name FROM participants WHERE phone_number='+15555550104'`).Scan(&name))
	assert.Equal("Push Name Only", name)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(5, events)
}
