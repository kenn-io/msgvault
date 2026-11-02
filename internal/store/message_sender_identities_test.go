package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMessageSenderIdentitiesRejectLegacyRawImportOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	var ids []int64
	for _, sourceMessageID := range []string{
		"mbox-" + strings.Repeat("a", 64) + "-1",
		"eml-" + strings.Repeat("b", 64),
		"maildir-" + strings.Repeat("c", 64),
		"emlx-" + strings.Repeat("d", 64),
		"pst-archive-entry",
	} {
		id := senderRelationshipMessage(t, f, true, "to", "target@example.com")
		_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), sourceMessageID, id)
		requirements.NoError(err)
		ids = append(ids, id)
	}
	identities, err := f.Store.MessageSenderIdentities(t.Context(), ids, nil)
	requirements.NoError(err)
	requirements.Len(identities, len(ids))
	for _, id := range ids {
		assertions.False(identities[id].SourceIsFromMe, "old import flags derived from From must not establish self")
	}
	nativeChat := senderRelationshipMessage(t, f, true, "to", "target@example.com")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET source_message_id='eml-native-chat-id',message_type='whatsapp' WHERE id=?`), nativeChat)
	requirements.NoError(err)
	identities, err = f.Store.MessageSenderIdentities(t.Context(), []int64{nativeChat}, nil)
	requirements.NoError(err)
	assertions.True(identities[nativeChat].SourceIsFromMe, "opaque chat IDs do not lose native ownership")
}

func TestMessageSenderIdentitiesPreserveEnvelopeAndNativeOwnership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	inbound := senderRelationshipMessage(t, f, false, "to", "sender@example.com")
	outbound := senderRelationshipMessage(t, f, true, "to", "target@example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET identity_is_from_me=TRUE,is_from_me=TRUE WHERE id=?`), inbound)
	requirements.NoError(err)
	absorbed := f.EnsureParticipant("sender@example.com", "Sender", "example.com")
	survivor := f.EnsureParticipant("different@example.com", "Different", "example.com")
	requirements.NoError(f.Store.MergeParticipants(absorbed, survivor))
	got, err := f.Store.MessageSenderIdentities(t.Context(), []int64{inbound, outbound, inbound}, nil)
	requirements.NoError(err)
	assertions.Len(got, 2)
	assertions.Equal("sender@example.com", got[inbound].Address)
	assertions.False(got[inbound].SourceIsFromMe, "address-derived ownership must not grant self")
	assertions.True(got[outbound].SourceIsFromMe)
	assertions.Equal(f.Source.ID, got[inbound].SourceID)
	for _, scope := range [][]int64{{}, {f.Source.ID + 100}} {
		got, err = f.Store.MessageSenderIdentities(t.Context(), []int64{inbound}, scope)
		requirements.NoError(err)
		assertions.Empty(got)
	}
}

func TestMessageSenderIdentitiesRejectAmbiguousAuthors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, false, "to", "first@example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type,email_address) VALUES (?,?,'from','second@example.com')`), id, second)
	requirements.NoError(err)
	got, err := f.Store.MessageSenderIdentities(t.Context(), []int64{id}, nil)
	requirements.NoError(err)
	assertions.Empty(got[id].Address)
	assertions.False(got[id].SourceIsFromMe)
}

func TestMessageSenderIdentitiesRejectIncompleteAdditionalAuthor(t *testing.T) {
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, false, "to", "first@example.com")
	second := f.EnsureParticipant("second@example.com", "Second", "example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES (?,?,'from')`), id, second)
	require.NoError(t, err)
	got, err := f.Store.MessageSenderIdentities(t.Context(), []int64{id}, nil)
	require.NoError(t, err)
	assert.Empty(t, got[id].Address, "missing historical author addresses cannot be ignored")
}

func TestMessageSenderIdentitiesValidateBeforeRead(t *testing.T) {
	f := storetest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.Store.MessageSenderIdentities(ctx, []int64{1}, nil)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = f.Store.MessageSenderIdentities(t.Context(), make([]int64, 501), nil)
	assert.ErrorIs(t, err, store.ErrSenderRelationshipLimit)
	_, err = f.Store.MessageSenderIdentities(t.Context(), []int64{-1}, nil)
	assert.Error(t, err)
}

func TestMessageSenderIdentitiesMissingSnapshotCannotFollowMerge(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	id := senderRelationshipMessage(t, f, false, "to", "old@example.com")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM message_recipients WHERE message_id=? AND recipient_type='from'`), id)
	requirements.NoError(err)
	old := f.EnsureParticipant("old@example.com", "Old", "example.com")
	other := f.EnsureParticipant("other@example.com", "Other", "example.com")
	requirements.NoError(f.Store.MergeParticipants(old, other))
	got, err := f.Store.MessageSenderIdentities(t.Context(), []int64{id}, nil)
	requirements.NoError(err)
	assert.Empty(t, got[id].Address)
}
