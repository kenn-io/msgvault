package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestWriteBeeperRunFailuresListsChats(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)

	src, err := st.GetOrCreateSource("beeper", "signal")
	require.NoError(err)
	convID, err := st.EnsureConversation(src.ID, "!book:beeper.local", "Book Club")
	require.NoError(err)
	_, err = st.UpsertMessage(&store.Message{
		SourceID: src.ID, ConversationID: convID, SourceMessageID: "m1", MessageType: "chat",
	})
	require.NoError(err)
	syncID, err := st.StartSync(src.ID, "beeper")
	require.NoError(err)

	record := func(id, phase, status, msg string) {
		require.NoError(st.RecordSyncRunItem(store.SyncRunItem{
			SyncRunID: syncID, SourceMessageID: id, Phase: phase, Status: status,
			ErrorKind: "beeper_test", ErrorMessage: msg,
		}))
	}
	record("!book:beeper.local", "fetch", store.SyncRunItemStatusError, "connection refused")
	record("m1", "attachment", store.SyncRunItemStatusError, "asset timeout\x1b[31m")
	record("!fresh:beeper.local", "fetch", store.SyncRunItemStatusError, "connection refused")
	record("gone", "attachment", store.SyncRunItemStatusError, "asset timeout")
	record("m1", "attachment", store.SyncRunItemStatusSkipped, "policy")

	var out bytes.Buffer
	imp := beeper.NewImporter(st, nil)
	writeBeeperRunFailures(context.Background(), &out, imp,
		&beeper.ImportSummary{SourceID: src.ID, SyncRunID: syncID, Errors: 4})

	got := out.String()
	assert.Contains(got, "Failed chats (4 recorded errors in 3 chats, retried next run):")
	assert.Contains(got, "    Book Club (!book:beeper.local): 2 attachment/fetch error(s); last: asset timeout\n",
		"an archived chat is named, and message-level failures roll up to it")
	assert.Contains(got, "    !fresh:beeper.local: 1 fetch error(s); last: connection refused\n",
		"a chat with no archived history is listed by ID")
	assert.Contains(got, "    (messages not yet archived): 1 attachment error(s); last: asset timeout\n")
	assert.NotContains(got, "\x1b", "error text is sanitized for the terminal")
	assert.NotContains(got, "policy", "skipped items are not failures")

	out.Reset()
	writeBeeperRunFailures(context.Background(), &out, imp,
		&beeper.ImportSummary{SourceID: src.ID, SyncRunID: syncID})
	assert.Empty(out.String(), "a run without errors prints nothing")
}
