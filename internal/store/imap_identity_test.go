package store_test

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newIMAPIdentityFixture(t *testing.T) imapMembershipFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://identity@example.com:143")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(source.ID, "identity-thread", "Identity thread")
	require.NoError(t, err)
	return imapMembershipFixture{store: st, source: source, convID: convID}
}

func TestIMAPIdentity_CanonicalIdentitySkipsRawPriming(t *testing.T) {
	requirements := require.New(t)
	f := newIMAPIdentityFixture(t)
	messageID := f.createMessage(t, "canonical", "")
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 17, UID: 1, SourceMessageID: "canonical",
		}},
	}}))
	_, err := f.store.DB().Exec(f.store.Rebind(`
		INSERT INTO message_raw (message_id, raw_data, raw_format, compression)
		VALUES (?, ?, 'mime', 'zlib')
	`), messageID, []byte("invalid zlib"))
	requirements.NoError(err)

	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 17, UID: 1, SourceMessageID: "INBOX|1",
			CanonicalSourceMessageID: "canonical", RawSHA256: sha256.Sum256([]byte("unused")),
		}},
	}}))
}

func TestIMAPIdentity_ResetSameUIDIdentityMismatchSelectsIncomingMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "INBOX|1", "<old@example.com>")
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 10, UID: 1,
			SourceMessageID: "INBOX|1", RFC822MessageID: "<old@example.com>",
		}},
	}}))
	newID := f.createMessage(t, "new-source", "<new@example.com>")

	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 20, UIDNext: 2},
		Reset:   true,
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 20, UID: 1,
			SourceMessageID: "INBOX|1", RFC822MessageID: "<new@example.com>",
		}},
	}}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 20, 1)
	assertions.Equal(newID, gotMessageID)
	assertions.Equal(1, membershipCount(t, f.store, f.source.ID))
	assertions.True(messageTombstoned(t, f.store, oldID))

	var oldSourceID string
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), oldID).Scan(&oldSourceID))
	assertions.Equal(fmt.Sprintf("msgvault-invalidated:%d", oldID), oldSourceID)
}

func TestIMAPIdentity_ResetPreservesCanonicalIdentity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "INBOX|1", "<old@example.com>")
	initial := []store.IMAPMailboxDelta{
		{
			Mailbox: "INBOX",
			State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "INBOX", UIDValidity: 10, UID: 1,
				SourceMessageID: "INBOX|1", RFC822MessageID: "<old@example.com>",
			}},
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 30, UIDNext: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Archive", UIDValidity: 30, UID: 1,
				SourceMessageID: "Archive|1", CanonicalSourceMessageID: "INBOX|1",
			}},
		},
	}
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, initial))
	newID := f.createMessage(t, "new-source", "<new@example.com>")

	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "INBOX",
			State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 20, UIDNext: 2},
			Reset:   true,
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "INBOX", UIDValidity: 20, UID: 1,
				SourceMessageID: "INBOX|1", RFC822MessageID: "<new@example.com>",
			}},
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 30, UIDNext: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Archive", UIDValidity: 30, UID: 1,
				SourceMessageID: "Archive|1", CanonicalSourceMessageID: "INBOX|1",
			}},
		},
	}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 20, 1)
	assertions.Equal(newID, gotMessageID)
	var archiveMessageID int64
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), f.source.ID, "Archive", 30, 1).Scan(&archiveMessageID))
	assertions.Equal(oldID, archiveMessageID)
	assertions.Equal(2, membershipCount(t, f.store, f.source.ID))
}

func TestIMAPIdentity_RekeysRemovedDraftKeyWithOtherMembership(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "Drafts|1", "<draft-shared@example.com>")

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "Drafts",
			State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Drafts", UIDValidity: 10, UID: 1, SourceMessageID: "Drafts|1",
			}},
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 20, UIDNext: 8},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Archive", UIDValidity: 20, UID: 7, SourceMessageID: "Drafts|1",
			}},
		},
	}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "Drafts",
			State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 30, UIDNext: 2},
			Reset:   true,
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 20, UIDNext: 8},
		},
	}))

	var sourceMessageID string
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT source_message_id FROM messages WHERE id = ?
	`), oldID).Scan(&sourceMessageID))
	assert.Equal(fmt.Sprintf("msgvault-invalidated:%d", oldID), sourceMessageID)

	newReceipt := store.IMAPDraftReceipt{
		SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 30, UID: 1,
	}
	newID, err := f.store.PersistIMAPDraftContext(t.Context(), newReceipt, nil,
		func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        f.source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(newReceipt),
					ConversationID:  f.convID,
					MessageType:     "email",
					RFC822MessageID: sql.NullString{String: "<draft-reused@example.com>", Valid: true},
				},
				BodyText: sql.NullString{String: "reused", Valid: true},
				RawMIME:  []byte("From: alice@example.com\r\n\r\nreused\r\n"),
			}
		})
	require.NoError(err)
	assert.NotEqual(oldID, newID)
}

func TestIMAPIdentity_UIDValidityResetRekeysOrphanedDraftBeforeUpsert(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPIdentityFixture(t)
	receipt := store.IMAPDraftReceipt{
		SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 10, UID: 1,
	}
	draftID, err := f.store.PersistIMAPDraftContext(t.Context(), receipt, nil,
		func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        f.source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
					ConversationID:  f.convID,
					MessageType:     "email",
				},
				BodyText: sql.NullString{String: "old draft", Valid: true},
				RawMIME:  []byte("Subject: Old draft\r\n\r\nold draft\r\n"),
			}
		})
	require.NoError(err)
	require.NoError(f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{{
		Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2,
	}}))
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts",
		State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
		Reset:   true,
	}}))
	newID := f.createMessage(t, "new-source", "<new-draft@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts",
		State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 20, UIDNext: 2},
		Reset:   true,
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Drafts", UIDValidity: 20, UID: 1,
			SourceMessageID: "Drafts|1", RFC822MessageID: "<new-draft@example.com>",
		}},
	}}))

	var gotID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), f.source.ID, "Drafts", 20, 1).Scan(&gotID))
	assert.Equal(newID, gotID)
	var sourceMessageID string
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), draftID).Scan(&sourceMessageID))
	assert.Equal(fmt.Sprintf("msgvault-invalidated:%d", draftID), sourceMessageID)
}

func TestIMAPIdentity_RetiredMailboxRekeysOrphanedDraftSourceKey(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPIdentityFixture(t)
	receipt := store.IMAPDraftReceipt{
		SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 10, UID: 1,
	}
	draftID, err := f.store.PersistIMAPDraftContext(t.Context(), receipt, nil,
		func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        f.source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
					ConversationID:  f.convID,
					MessageType:     "email",
				},
				BodyText: sql.NullString{String: "old draft", Valid: true},
				RawMIME:  []byte("Subject: Old draft\r\n\r\nold draft\r\n"),
			}
		})
	require.NoError(err)
	require.NoError(f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{{
		Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2,
	}}))
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts", Reset: true,
		State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
	}}))
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 30, UIDNext: 1},
	}}))
	var sourceMessageID string
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(
		"SELECT source_message_id FROM messages WHERE id = ?"), draftID).Scan(&sourceMessageID))
	assert.Equal(fmt.Sprintf("msgvault-invalidated:%d", draftID), sourceMessageID)

	newID := f.createMessage(t, "new-source", "<recreated-draft@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts", Reset: true,
		State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 20, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Drafts", UIDValidity: 20, UID: 1,
			SourceMessageID: "Drafts|1", RFC822MessageID: "<recreated-draft@example.com>",
		}},
	}}))
	var gotID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), f.source.ID, "Drafts", 20, 1).Scan(&gotID))
	assert.Equal(newID, gotID)
}

func TestIMAPIdentity_ResolvesQueuedCanonicalKeyAfterMailboxRetirement(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPIdentityFixture(t)
	messageID := f.createMessage(t, "Old|1", "")

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "Old",
			State:   store.IMAPFolderState{Mailbox: "Old", UIDValidity: 10, UIDNext: 2},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Old", UIDValidity: 10, UID: 1, SourceMessageID: "Old|1",
			}},
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 20, UIDNext: 8},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Archive", UIDValidity: 20, UID: 7, SourceMessageID: "Old|1",
			}},
		},
	}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Archive",
		State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 20, UIDNext: 8},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Archive", UIDValidity: 20, UID: 7,
			SourceMessageID: "Archive|7", CanonicalSourceMessageID: "Old|1",
		}},
	}}))

	var gotMessageID int64
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`
		SELECT message_id FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), f.source.ID, "Archive", 20, 7).Scan(&gotMessageID))
	assert.Equal(messageID, gotMessageID)
	assert.Equal(1, membershipCount(t, f.store, f.source.ID))
}

func TestIMAPIdentity_SameEpochOmissionReappears(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newIMAPIdentityFixture(t)
	messageID := f.createMessage(t, "INBOX|4", "<stable@example.com>")
	raw := []byte("Subject: Stable\r\n\r\nretained content\r\n")
	requirements.NoError(f.store.UpsertMessageRaw(messageID, raw))
	delta := store.IMAPMailboxDelta{
		Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 5},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 10, UID: 4, SourceMessageID: "INBOX|4"}},
	}
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	omitted := delta
	omitted.Reset = true
	omitted.Memberships = nil
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{omitted}))
	requirements.True(messageTombstoned(t, f.store, messageID))
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	gotID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 10, 4)
	assertions.Equal(messageID, gotID)
	assertions.False(messageTombstoned(t, f.store, messageID))
	gotRaw, err := f.store.GetMessageRaw(messageID)
	requirements.NoError(err)
	assertions.Equal(raw, gotRaw)
}

func TestIMAPIdentity_UnobservedUIDLaterReuse(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newIMAPIdentityFixture(t)
	keptID := f.createMessage(t, "Old|1", "<kept@example.com>")
	oldID := f.createMessage(t, "Old|2", "<old@example.com>")
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Old", State: store.IMAPFolderState{Mailbox: "Old", UIDValidity: 10, UIDNext: 3},
		Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "Old", UIDValidity: 10, UID: 1, SourceMessageID: "Old|1", RFC822MessageID: "<kept@example.com>"},
			{Mailbox: "Old", UIDValidity: 10, UID: 2, SourceMessageID: "Old|2", RFC822MessageID: "<old@example.com>"},
		},
	}}))
	delta := store.IMAPMailboxDelta{
		Mailbox: "Old", Reset: true, State: store.IMAPFolderState{Mailbox: "Old", UIDValidity: 20, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "Old", UIDValidity: 20, UID: 1, SourceMessageID: "Old|1", RFC822MessageID: "<kept@example.com>"}},
	}
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	newID := f.createMessage(t, "new-source", "<new@example.com>")
	delta.Reset = false
	delta.State.UIDNext = 3
	delta.Memberships = []store.IMAPMembershipObservation{{Mailbox: "Old", UIDValidity: 20, UID: 2, SourceMessageID: "Old|2", RFC822MessageID: "<new@example.com>"}}
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	var actualID int64
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, "Old", 20, 2).Scan(&actualID))
	assertions.Equal(newID, actualID)
	assertions.True(messageTombstoned(t, f.store, oldID))
	assertions.False(messageTombstoned(t, f.store, keptID))
}

func TestIMAPIdentity_NewPublishedCopyKeepsKey(t *testing.T) {
	for _, mode := range []string{"append", "ingest", "ingest-shared"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newIMAPIdentityFixture(t)
			receipt := store.IMAPDraftReceipt{SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 10, UID: 1}
			build := func([]int64) *store.MessagePersistData {
				return &store.MessagePersistData{
					Message: &store.Message{SourceID: f.source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt), ConversationID: f.convID, MessageType: store.MessageTypeEmail, RFC822MessageID: sql.NullString{String: fmt.Sprintf("<generation-%d@example.com>", receipt.UIDValidity), Valid: true}},
					RawMIME: fmt.Appendf(nil, "Subject: Generation %d\r\n\r\ncontent %d\r\n", receipt.UIDValidity, receipt.UIDValidity),
				}
			}
			oldID, err := f.store.PersistIMAPDraftContext(t.Context(), receipt, nil, build)
			requirements.NoError(err)
			oldRaw, err := f.store.GetMessageRaw(oldID)
			requirements.NoError(err)
			requirements.NoError(f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2}}))
			receipt.UIDValidity = 20
			var newID int64
			if mode == "append" {
				newID, err = f.store.PersistIMAPDraftContext(t.Context(), receipt, nil, build)
			} else {
				changed, rekeyErr := f.store.RekeyMessageSourceID(oldID, "Drafts|1", fmt.Sprintf("msgvault-invalidated:%d", oldID))
				requirements.NoError(rekeyErr)
				requirements.True(changed)
				newID, err = f.store.PersistMessage(build(nil))
			}
			requirements.NoError(err)
			if mode == "ingest-shared" {
				requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
					{Mailbox: "Drafts", State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2}},
					{Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 30, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: "Drafts|1"}}},
				}))
			}
			requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
				Mailbox: "Drafts", Reset: true, State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 20, UIDNext: 2},
				Memberships: []store.IMAPMembershipObservation{{Mailbox: "Drafts", UIDValidity: 20, UID: 1, SourceMessageID: "Drafts|1", RFC822MessageID: "<generation-20@example.com>"}},
			}}))
			key, err := f.store.GetMessageSourceID(newID)
			requirements.NoError(err)
			assertions.Equal("Drafts|1", key)
			var currentID int64
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, "Drafts", 20, 1).Scan(&currentID))
			assertions.Equal(newID, currentID)
			retainedRaw, err := f.store.GetMessageRaw(oldID)
			requirements.NoError(err)
			assertions.Equal(oldRaw, retainedRaw)
		})
	}
}

func TestIMAPIdentity_AmbiguousLiveAppendKeyRefuses(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "Drafts|1", "<old@example.com>")
	receipt := store.IMAPDraftReceipt{SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 20, UID: 1}
	_, err := f.store.PersistIMAPDraftContext(t.Context(), receipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{Message: &store.Message{SourceID: f.source.ID, SourceMessageID: "Drafts|1", ConversationID: f.convID, MessageType: store.MessageTypeEmail}}
	})
	requirements.ErrorContains(err, "source_key_conflict")
	key, err := f.store.GetMessageSourceID(oldID)
	requirements.NoError(err)
	assertions.Equal("Drafts|1", key)
	assertions.Zero(membershipCount(t, f.store, f.source.ID))
}

func TestIMAPIdentity_LegacyOrphanIdentity(t *testing.T) {
	for _, mode := range []string{"unique", "same", "missing", "duplicate", "canonical", "known-missing", "known-duplicate"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			f := newIMAPIdentityFixture(t)
			oldID := f.createMessage(t, "Drafts|1", "<old@example.com>")
			requirements.NoError(f.store.MarkMessageDeleted(f.source.ID, "Drafts|1"))
			if mode == "known-missing" || mode == "known-duplicate" {
				requirements.NoError(f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2}}))
			}
			beforeStates, err := f.store.GetIMAPFolderStates(f.source.ID)
			requirements.NoError(err)
			newID := f.createMessage(t, "new-source", "<new@example.com>")
			obs := store.IMAPMembershipObservation{Mailbox: "Drafts", UIDValidity: 20, UID: 1, SourceMessageID: "Drafts|1", RFC822MessageID: "<new@example.com>"}
			if mode == "same" {
				obs.RFC822MessageID = "<old@example.com>"
				newID = oldID
			}
			if mode == "missing" || mode == "known-missing" {
				obs.RFC822MessageID = ""
			}
			if mode == "duplicate" || mode == "canonical" || mode == "known-duplicate" {
				f.createMessage(t, "duplicate-source", "<new@example.com>")
			}
			if mode == "canonical" {
				obs.CanonicalSourceMessageID = "new-source"
			}
			err = f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{Mailbox: "Drafts", Reset: true, State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 20, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{obs}}})
			if mode == "missing" || mode == "duplicate" || mode == "known-missing" || mode == "known-duplicate" {
				requirements.Error(err)
				assertions.Zero(membershipCount(t, f.store, f.source.ID))
				states, stateErr := f.store.GetIMAPFolderStates(f.source.ID)
				requirements.NoError(stateErr)
				assertions.ElementsMatch(beforeStates, states)
				key, keyErr := f.store.GetMessageSourceID(oldID)
				requirements.NoError(keyErr)
				assertions.Equal("Drafts|1", key)
				assertions.True(messageTombstoned(t, f.store, oldID))
				return
			}
			requirements.NoError(err)
			var currentID int64
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, "Drafts", 20, 1).Scan(&currentID))
			assertions.Equal(newID, currentID)
			if mode == "same" {
				assertions.False(messageTombstoned(t, f.store, oldID))
			}
		})
	}
}

func TestIMAPIdentity_OrphanRetirementMailboxBoundary(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newIMAPIdentityFixture(t)
	mailbox := "Drafts%_|box"
	nested := mailbox + "|nested"
	oldID := f.createMessage(t, mailbox+"|1", "<old@example.com>")
	nestedID := f.createMessage(t, nested+"|1", "<nested@example.com>")
	for _, key := range []string{mailbox + "|1", nested + "|1"} {
		requirements.NoError(f.store.MarkMessageDeleted(f.source.ID, key))
	}
	requirements.NoError(f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{{Mailbox: mailbox, UIDValidity: 10, UIDNext: 2}, {Mailbox: nested, UIDValidity: 30, UIDNext: 2}}))
	newID := f.createMessage(t, "new-source", "<new@example.com>")
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: mailbox, Reset: true, State: store.IMAPFolderState{Mailbox: mailbox, UIDValidity: 20, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{Mailbox: mailbox, UIDValidity: 20, UID: 1, SourceMessageID: mailbox + "|1", RFC822MessageID: "<new@example.com>"}}},
		{Mailbox: nested, State: store.IMAPFolderState{Mailbox: nested, UIDValidity: 30, UIDNext: 2}},
	}))
	var currentID int64
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, mailbox, 20, 1).Scan(&currentID))
	assertions.Equal(newID, currentID)
	assertions.True(messageTombstoned(t, f.store, oldID))
	nestedKey, err := f.store.GetMessageSourceID(nestedID)
	requirements.NoError(err)
	assertions.Equal(nested+"|1", nestedKey)
}
func TestIMAPIdentity_MovedCopyMembershipBeforeSync(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "Drafts|1", "<moved@example.com>")
	oldRaw := []byte("Subject: Moved\r\n\r\nold content\r\n")
	requirements.NoError(f.store.UpsertMessageRaw(oldID, oldRaw))
	drafts := store.IMAPMailboxDelta{
		Mailbox: "Drafts", State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "Drafts", UIDValidity: 10, UID: 1, SourceMessageID: "Drafts|1"}},
	}
	archive := store.IMAPMailboxDelta{
		Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 30, UIDNext: 8},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "Archive", UIDValidity: 30, UID: 7, CanonicalSourceMessageID: "Drafts|1"}},
	}
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{drafts, archive}))
	drafts.Reset = true
	drafts.Memberships = nil
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{drafts, archive}))
	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	requirements.NoError(err)
	newID := f.createMessage(t, "new-source", "<new@example.com>")
	drafts.State.UIDValidity = 20
	drafts.Memberships = []store.IMAPMembershipObservation{{Mailbox: "Drafts", UIDValidity: 20, UID: 1, SourceMessageID: "Drafts|1", RFC822MessageID: "<new@example.com>"}}
	archive.Memberships = nil
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{drafts, archive}))
	var currentID, archiveID int64
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, "Drafts", 20, 1).Scan(&currentID))
	assertions.Equal(newID, currentID)
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, "Archive", 30, 7).Scan(&archiveID))
	assertions.Equal(oldID, archiveID)
	retainedRaw, err := f.store.GetMessageRaw(oldID)
	requirements.NoError(err)
	assertions.Equal(oldRaw, retainedRaw)
	assertions.Equal([]string{"Archive"}, messageLabels(t, f.store, oldID))
	assertions.Len(states, 2)
}
func TestIMAPIdentity_SameEpochResetRemovalReleasesSourceKeyForLaterEpoch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "Drafts|1", "")

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts",
		State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Drafts", UIDValidity: 10, UID: 1, SourceMessageID: "Drafts|1",
		}},
	}}))
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Drafts",
		State:   store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
		Reset:   true,
	}}))

	receipt := store.IMAPDraftReceipt{
		SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 20, UID: 1,
	}
	newID, err := f.store.PersistIMAPDraftContext(t.Context(), receipt, nil,
		func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        f.source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
					ConversationID:  f.convID,
					MessageType:     "email",
				},
				BodyText: sql.NullString{String: "new draft", Valid: true},
				RawMIME:  []byte("From: alice@example.com\r\n\r\nnew draft\r\n"),
			}
		})
	require.NoError(err)
	assert.NotEqual(oldID, newID)
	oldSourceMessageID, err := f.store.GetMessageSourceID(oldID)
	require.NoError(err)
	assert.Equal(fmt.Sprintf("msgvault-invalidated:%d", oldID), oldSourceMessageID)
}

func TestIMAPIdentity_RekeysOrphanedSourceDeletedMessage(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "imap://legacy@example.com:143")
	requirements.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "legacy-draft", "Legacy draft")
	requirements.NoError(err)
	oldID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			SourceID: source.ID, SourceMessageID: "Drafts|1",
			ConversationID: conversationID, MessageType: store.MessageTypeEmail,
		},
		BodyText: sql.NullString{String: "old draft", Valid: true},
		RawMIME:  []byte("Subject: Old draft\r\n\r\nold draft\r\n"),
	})
	requirements.NoError(err)
	requirements.NoError(st.MarkMessageDeleted(source.ID, "Drafts|1"))
	oldRaw, err := st.GetMessageRaw(oldID)
	requirements.NoError(err)

	receipt := store.IMAPDraftReceipt{
		SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 20, UID: 1,
	}
	_, err = st.PersistIMAPDraftContext(t.Context(), receipt, nil, func([]int64) *store.MessagePersistData { return nil })
	requirements.ErrorContains(err, "persist message requires a message")
	key, err := st.GetMessageSourceID(oldID)
	requirements.NoError(err)
	assertions.Equal("Drafts|1", key)
	assertions.Zero(membershipCount(t, st, source.ID))
	assertions.True(messageTombstoned(t, st, oldID))
	newID, err := st.PersistIMAPDraftContext(t.Context(), receipt, nil,
		func([]int64) *store.MessagePersistData {
			return &store.MessagePersistData{
				Message: &store.Message{
					SourceID:        source.ID,
					SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
					ConversationID:  conversationID,
					MessageType:     store.MessageTypeEmail,
				},
				BodyText: sql.NullString{String: "new draft", Valid: true},
				RawMIME:  []byte("Subject: New draft\r\n\r\nnew draft\r\n"),
			}
		})
	requirements.NoError(err)
	assertions.NotEqual(oldID, newID)
	retainedRaw, err := st.GetMessageRaw(oldID)
	requirements.NoError(err)
	assertions.Equal(oldRaw, retainedRaw)
	oldSourceMessageID, err := st.GetMessageSourceID(oldID)
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("msgvault-invalidated:%d", oldID), oldSourceMessageID)
}

func TestIMAPIdentity_MovedCopyAppendBeforeSync(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := newIMAPIdentityFixture(t)
	oldID := f.createMessage(t, "Drafts|1", "<moved@example.com>")
	oldRaw := []byte("Subject: Moved\r\n\r\nold content\r\n")
	requirements.NoError(f.store.UpsertMessageRaw(oldID, oldRaw))
	drafts := store.IMAPMailboxDelta{
		Mailbox: "Drafts", State: store.IMAPFolderState{Mailbox: "Drafts", UIDValidity: 10, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "Drafts", UIDValidity: 10, UID: 1, SourceMessageID: "Drafts|1"}},
	}
	archive := store.IMAPMailboxDelta{
		Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 30, UIDNext: 8},
		Memberships: []store.IMAPMembershipObservation{{Mailbox: "Archive", UIDValidity: 30, UID: 7, CanonicalSourceMessageID: "Drafts|1"}},
	}
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{drafts, archive}))
	drafts.Reset = true
	drafts.Memberships = nil
	requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{drafts, archive}))
	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: f.source.ID, Mailbox: "Drafts", UIDValidity: 20, UID: 1}
	newRaw := []byte("Subject: Replacement\r\n\r\nnew content\r\n")
	newID, err := f.store.PersistIMAPDraftContext(t.Context(), receipt, nil, func([]int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{SourceID: f.source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt), ConversationID: f.convID, MessageType: store.MessageTypeEmail},
			RawMIME: newRaw,
		}
	})
	requirements.NoError(err)
	assertions.NotEqual(oldID, newID)
	retainedRaw, err := f.store.GetMessageRaw(oldID)
	requirements.NoError(err)
	assertions.Equal(oldRaw, retainedRaw)
	retainedStates, err := f.store.GetIMAPFolderStates(f.source.ID)
	requirements.NoError(err)
	assertions.ElementsMatch(states, retainedStates)
	assertions.Equal([]string{"Archive"}, messageLabels(t, f.store, oldID))
	var archiveID int64
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`), f.source.ID, "Archive", 30, 7).Scan(&archiveID))
	assertions.Equal(oldID, archiveID)
}
