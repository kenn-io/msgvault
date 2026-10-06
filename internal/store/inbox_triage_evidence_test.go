package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestInboxTriageEvidenceRequiresExactItemMembership(t *testing.T) {
	f, _, states := inboxCandidateFixture(t)
	reader, ok := any(f.Store).(interface {
		ValidateInboxTriageEvidence(context.Context, inboxcontrol.Target, []int64) error
	})
	require.True(t, ok, "triage evidence needs archive membership validation")
	target := states[0].Target
	assert.NoError(t, reader.ValidateInboxTriageEvidence(t.Context(), target, []int64{target.ItemID}))
	assert.NoError(t, reader.ValidateInboxTriageEvidence(t.Context(), target, nil))
	assert.ErrorIs(t, reader.ValidateInboxTriageEvidence(t.Context(), target, []int64{states[1].Target.ItemID}), inboxcontrol.ErrDenied)
	assert.ErrorIs(t, reader.ValidateInboxTriageEvidence(t.Context(), target, []int64{0}), inboxcontrol.ErrInvalid)
	assert.ErrorIs(t, reader.ValidateInboxTriageEvidence(t.Context(), target, []int64{target.ItemID, target.ItemID}), inboxcontrol.ErrInvalid)
	_, err := f.Store.DB().Exec(`DROP TABLE message_bodies`)
	require.NoError(t, err)
	assert.NoError(t, reader.ValidateInboxTriageEvidence(t.Context(), target, []int64{target.ItemID}))
}

func TestInboxTriageEvidenceChatAllowsHistoryWithinExactSourceAndConversation(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	require.NoError(t, err)
	chat, err := st.EnsureConversation(source.ID, "native-chat", "Synthetic chat")
	require.NoError(t, err)
	other, err := st.EnsureConversation(source.ID, "other-chat", "Other synthetic chat")
	require.NoError(t, err)
	foreign, err := st.GetOrCreateSource("beeper", "foreign-account")
	require.NoError(t, err)
	add := func(sourceID, chatID int64, provider string) int64 {
		t.Helper()
		id, err := st.UpsertMessage(&store.Message{SourceID: sourceID, ConversationID: chatID, SourceMessageID: provider, MessageType: "beeper"})
		require.NoError(t, err)
		return id
	}
	older := add(source.ID, chat, "older-evidence")
	newer := add(source.ID, chat, "newer-evidence")
	otherMessage := add(source.ID, other, "other-chat-evidence")
	foreignMessage := add(foreign.ID, chat, "foreign-source-evidence")
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeChat, ItemID: chat, ProviderID: "native-chat"}
	assert.NoError(t, st.ValidateInboxTriageEvidence(t.Context(), target, []int64{older, newer}))
	for _, id := range []int64{otherMessage, foreignMessage} {
		assert.ErrorIs(t, st.ValidateInboxTriageEvidence(t.Context(), target, []int64{id}), inboxcontrol.ErrDenied)
	}
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET deleted_from_source_at=? WHERE id=?`), time.Now(), older)
	require.NoError(t, err)
	assert.ErrorIs(t, st.ValidateInboxTriageEvidence(t.Context(), target, []int64{older}), inboxcontrol.ErrDenied)
	assert.NoError(t, st.ValidateInboxTriageEvidence(t.Context(), target, []int64{newer}))
}
