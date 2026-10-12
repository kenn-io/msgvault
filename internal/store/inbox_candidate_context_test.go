package store_test

import (
	"database/sql"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// The admitted candidate page must supply a usable exact archived message ID
// for chat context without a generic archive-read grant or a body list scan.
func TestInboxChatCandidatesSelectScopedContextMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	requirements.NoError(err)
	chat, err := st.EnsureConversation(source.ID, "native-chat", "Synthetic chat")
	requirements.NoError(err)
	otherChat, err := st.EnsureConversation(source.ID, "other-chat", "Synthetic other chat")
	requirements.NoError(err)
	foreign, err := st.GetOrCreateSource("beeper", "foreign-account")
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeChat, ItemID: chat, ProviderID: "native-chat"}
	_, err = st.ObserveInboxState(t.Context(), inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), MarkedUnread: new(false), ObservedAt: time.Now().UTC()})
	requirements.NoError(err)
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	add := func(sourceID, conversationID int64, providerID string, offset int) int64 {
		t.Helper()
		id, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: conversationID, SourceMessageID: providerID, MessageType: "beeper", SentAt: sql.NullTime{Time: epoch.Add(time.Duration(offset) * time.Hour), Valid: true}})
		require.NoError(t, err)
		return id
	}
	add(source.ID, chat, "older-context", 1)
	want := add(source.ID, chat, "latest-context", 3)
	// Importing history later must not replace the latest conversational context.
	add(source.ID, chat, "backfilled-context", 2)
	deleted := add(source.ID, chat, "deleted-context", 4)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_at=? WHERE id=?`), epoch, deleted)
	requirements.NoError(err)
	removed := add(source.ID, chat, "removed-context", 5)
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), epoch, removed)
	requirements.NoError(err)
	add(source.ID, otherChat, "other-conversation-context", 6)
	add(foreign.ID, chat, "foreign-source-context", 7)
	// Exercise the real metadata query with no body relation available.
	_, err = st.DB().Exec(`DROP TABLE message_bodies`)
	requirements.NoError(err)
	identity := inboxcontrol.SourceIdentity{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier}
	page, err := st.InboxCandidates(t.Context(), identity, inboxcontrol.ScopeChat, 1, "")
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	encoded, err := json.Marshal(page.Candidates[0])
	requirements.NoError(err)
	var wire struct {
		MessageID *int64 `json:"context_message_id"`
	}
	requirements.NoError(json.Unmarshal(encoded, &wire))
	requirements.NotNil(wire.MessageID, "chat metadata must expose the exact archived context selector")
	assertions.Equal(want, *wire.MessageID)
	assertions.Equal(target, page.Candidates[0].State.Target)
	assertions.True(page.Candidates[0].Available)
}

func TestInboxCandidatesAndContextRetainCallerSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Parallel()
	f, source, states := inboxCandidateFixture(t)
	for _, state := range states {
		requirements.NoError(f.Store.UpsertMessageBody(state.Target.ItemID, sql.NullString{String: "original text", Valid: true}, sql.NullString{}))
	}
	ctx, release, err := f.Store.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	before, err := f.Store.InboxCandidates(ctx, source, inboxcontrol.ScopeMessage, 1, "")
	requirements.NoError(err)
	requirements.Len(before.Candidates, 1)
	requirements.NotEmpty(before.NextCursor)
	selected := before.Candidates[0].State
	request := inboxcontrol.ContextRequest{Target: selected.Target}
	content, err := f.Store.InboxContext(ctx, request)
	requirements.NoError(err)
	assertions.Equal("original text", content.Text)

	// Commit real writes outside the caller's snapshot. Later reads in that
	// snapshot must keep its original metadata, cursor revision, and body.
	selected.Inbox = new(false)
	selected.ObservedAt = selected.ObservedAt.Add(time.Second)
	_, err = f.Store.ObserveInboxState(t.Context(), selected)
	requirements.NoError(err)
	requirements.NoError(f.Store.UpsertMessageBody(selected.Target.ItemID, sql.NullString{String: "replacement text", Valid: true}, sql.NullString{}))
	same, err := f.Store.InboxCandidates(ctx, source, inboxcontrol.ScopeMessage, 1, "")
	requirements.NoError(err)
	assertions.Equal(before, same)
	content, err = f.Store.InboxContext(ctx, request)
	requirements.NoError(err)
	assertions.Equal("original text", content.Text)
	continued, err := f.Store.InboxCandidates(ctx, source, inboxcontrol.ScopeMessage, 1, before.NextCursor)
	requirements.NoError(err)
	assertions.Equal(before.ArchiveRevision, continued.ArchiveRevision)

	release()
	current, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	assertions.NotEqual(before.ArchiveRevision, current.ArchiveRevision)
	for _, candidate := range current.Candidates {
		assertions.NotEqual(selected.Target.ItemID, candidate.State.Target.ItemID)
	}
	_, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, before.NextCursor)
	assertions.ErrorIs(err, inboxcontrol.ErrPlanChanged)
	content, err = f.Store.InboxContext(t.Context(), request)
	requirements.NoError(err)
	assertions.Equal("replacement text", content.Text)
}
