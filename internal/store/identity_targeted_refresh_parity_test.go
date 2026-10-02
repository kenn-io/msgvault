package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type attributionParityFixture struct {
	f        *storetest.Fixture
	other    int64
	expected map[int64]bool
}

// buildAttributionParityFixture covers every shape messageIdentityAttributionMatch
// distinguishes: envelope with no sender, an envelope that overrides an owned
// sender, legacy senders by primary email, email identifier and exact chat
// identifier, a merged participant whose alias survives only in the envelope,
// and a second source sharing participants and addresses.
func buildAttributionParityFixture(t *testing.T) attributionParityFixture {
	t.Helper()
	require := require.New(t)
	f := storetest.New(t)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.test")
	require.NoError(err)
	otherConv, err := f.Store.EnsureConversation(other.ID, "other-thread", "Other")
	require.NoError(err)
	withoutEmail := func(placeholder, kind, value string) int64 {
		id := f.EnsureParticipant(placeholder, "Synthetic", "example.test")
		require.NoError(f.Store.SetParticipantIdentifier(id, kind, value))
		_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE participants SET email_address=NULL WHERE id=?`), id)
		require.NoError(err)
		return id
	}
	maskSender := f.EnsureParticipant("mask@example.test", "Synthetic", "example.test")
	unicodeSender := f.EnsureParticipant("MÄSK@example.test", "Synthetic", "example.test")
	identSender := withoutEmail("ident-placeholder@example.test", "email", "Ident@Example.test")
	chatSender := withoutEmail("chat-placeholder@example.test", "imessage", "chat-handle-1")
	chatCaseSender := withoutEmail("chat-case-placeholder@example.test", "imessage", "Chat-Handle-1")
	imsgSender := withoutEmail("imsg-placeholder@example.test", "imessage", "imsg@example.test")
	survivor := f.EnsureParticipant("survivor@example.test", "Synthetic", "example.test")

	expected := map[int64]bool{}
	create := func(sourceID, conv int64, key string, sender int64, envelope string, want bool) {
		t.Helper()
		message := &store.Message{SourceID: sourceID, ConversationID: conv, SourceMessageID: key, MessageType: "email"}
		recipient := survivor
		if sender != 0 {
			message.SenderID = sql.NullInt64{Int64: sender, Valid: true}
			recipient = sender
		}
		data := &store.MessagePersistData{Message: message}
		if envelope != "" {
			data.Recipients = []store.RecipientSet{{Type: "from", ParticipantIDs: []int64{recipient}, DisplayNames: []string{"Synthetic"}, EmailAddresses: []string{envelope}}}
		}
		id, err := f.Store.PersistMessage(data)
		require.NoError(err)
		expected[id] = want
	}
	create(f.Source.ID, f.ConvID, "envelope-no-sender", 0, "Mask@Example.test", true)
	create(f.Source.ID, f.ConvID, "other-envelope-owned-sender", maskSender, "other@example.test", false)
	create(f.Source.ID, f.ConvID, "legacy-unicode-email", unicodeSender, "", true)
	create(f.Source.ID, f.ConvID, "legacy-email-identifier", identSender, "", true)
	create(f.Source.ID, f.ConvID, "legacy-chat-identifier", chatSender, "", true)
	create(f.Source.ID, f.ConvID, "legacy-chat-identifier-case", chatCaseSender, "", false)
	create(f.Source.ID, f.ConvID, "legacy-imessage-email-shaped", imsgSender, "", true)
	create(f.Source.ID, f.ConvID, "merged-alias-envelope", survivor, "alias@example.test", true)
	create(f.Source.ID, f.ConvID, "unrelated", survivor, "survivor@example.test", false)
	create(other.ID, otherConv, "other-source-envelope", maskSender, "mask@example.test", false)
	create(other.ID, otherConv, "other-source-legacy", unicodeSender, "", false)
	require.NoError(store.RefreshSourceMessageAttributionForTest(f.Store, f.Source.ID))
	require.NoError(store.RefreshSourceMessageAttributionForTest(f.Store, other.ID))
	return attributionParityFixture{f: f, other: other.ID, expected: expected}
}

func attributionSnapshot(t *testing.T, st *store.Store) map[int64][2]bool {
	t.Helper()
	rows, err := st.DB().Query(`SELECT id, identity_is_from_me, COALESCE(is_from_me, FALSE) FROM messages ORDER BY id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	result := map[int64][2]bool{}
	for rows.Next() {
		var id int64
		var identity, effective bool
		require.NoError(t, rows.Scan(&id, &identity, &effective))
		result[id] = [2]bool{identity, effective}
	}
	require.NoError(t, rows.Err())
	return result
}

// assertAttributionParity compares the targeted result with what the
// full-source refresh produces from the same identities.
func assertAttributionParity(t *testing.T, fx attributionParityFixture) map[int64][2]bool {
	t.Helper()
	got := attributionSnapshot(t, fx.f.Store)
	require.NoError(t, store.RefreshSourceMessageAttributionForTest(fx.f.Store, fx.f.Source.ID))
	require.NoError(t, store.RefreshSourceMessageAttributionForTest(fx.f.Store, fx.other))
	assert.Equal(t, attributionSnapshot(t, fx.f.Store), got, "targeted refresh must match the full-source refresh")
	return got
}

func TestIdentityTargetedRefreshMatchesFullSourceRefresh(t *testing.T) {
	identities := []string{"mask@example.test", "MÄSK@example.test", "ident@example.test", "chat-handle-1", "alias@example.test", "imsg@example.test"}
	// Removal spellings differ in case from the stored identities wherever the
	// identity is email-shaped, which deletes case-insensitively.
	removals := []string{"MASK@EXAMPLE.TEST", "MÄSK@example.test", "IDENT@example.test", "chat-handle-1", "Alias@Example.test", "IMSG@example.test"}

	t.Run("single", func(t *testing.T) {
		fx := buildAttributionParityFixture(t)
		var got map[int64][2]bool
		for _, identity := range identities {
			require.NoError(t, fx.f.Store.AddAccountIdentity(fx.f.Source.ID, identity, "manual"))
			got = assertAttributionParity(t, fx)
		}
		for id, want := range fx.expected {
			assert.Equal(t, want, got[id][0], "message %d", id)
		}
	})
	t.Run("batch", func(t *testing.T) {
		fx := buildAttributionParityFixture(t)
		confirmations := make([]store.IdentityConfirmation, 0, len(identities))
		for _, identity := range identities {
			confirmations = append(confirmations, store.IdentityConfirmation{Identifier: identity, Signals: []string{"provider-alias"}})
		}
		_, err := fx.f.Store.AddAccountIdentitiesBatchContext(t.Context(), fx.f.Source.ID, confirmations)
		require.NoError(t, err)
		got := assertAttributionParity(t, fx)
		for id, want := range fx.expected {
			assert.Equal(t, want, got[id][0], "message %d", id)
		}
	})
	t.Run("remove", func(t *testing.T) {
		require := require.New(t)
		fx := buildAttributionParityFixture(t)
		for _, identity := range identities {
			require.NoError(fx.f.Store.AddAccountIdentity(fx.f.Source.ID, identity, "manual"))
		}
		for _, removal := range removals {
			removed, err := fx.f.Store.RemoveAccountIdentity(fx.f.Source.ID, removal)
			require.NoError(err)
			require.EqualValues(1, removed, removal)
			assertAttributionParity(t, fx)
		}
		for id, value := range attributionSnapshot(t, fx.f.Store) {
			assert.False(t, value[0], "message %d", id)
		}
	})
}
