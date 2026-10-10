package store_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestChatMembersRefreshVsSourceRemovalPG(t *testing.T) {
	requirements := require.New(t)
	st := requirePostgreSQLStore(t)
	chat := newAttrFixtureOn(t, st, "beeper", "chat-account")
	victim := newAttrFixtureOn(t, st, "gmail", "victim@example.net")
	conversation, err := st.EnsureConversationWithType(chat.source.ID, "refresh-room", "direct_chat", "Refresh Room")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: chat.source.ID, SourceMessageID: "refresh", ConversationID: conversation,
		MessageType: "beeper", SentAt: sql.NullTime{Time: time.Unix(100, 0), Valid: true}})
	requirements.NoError(err)

	paused := make(chan struct{})
	resume := make(chan struct{})
	var pauseOnce sync.Once
	restore := st.SetChatMembersAfterClaimHookForTest(func() {
		pauseOnce.Do(func() {
			close(paused)
			<-resume
		})
	})
	defer restore()
	resumeOnce := sync.OnceFunc(func() { close(resume) })
	t.Cleanup(resumeOnce)

	searchDone := make(chan error, 1)
	go func() {
		_, err := st.SearchChatsContext(context.Background(), store.ChatDiscoveryQuery{Query: "Refresh"})
		searchDone <- err
	}()
	<-paused
	removeDone := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(context.Background(), victim.source.ID)
		removeDone <- err
	}()
	waitForLockWait(t, st, "archive_metadata", "source removal must wait at the identity row, before its table locks")
	resumeOnce()
	assertNoDeadlock(t, waitResult(t, searchDone, "chat discovery refresh"))
	assertNoDeadlock(t, waitResult(t, removeDone, "source removal"))
}
