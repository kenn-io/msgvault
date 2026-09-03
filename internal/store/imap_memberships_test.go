package store_test

import (
	"crypto/sha256"
	"database/sql"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type imapMembershipFixture struct {
	store  *store.Store
	source *store.Source
	convID int64
}

func newIMAPMembershipFixture(t *testing.T) imapMembershipFixture {
	t.Helper()
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("imap", "membership@example.com")
	require.NoError(t, err)
	convID, err := st.EnsureConversation(source.ID, "membership-thread", "Membership thread")
	require.NoError(t, err)
	return imapMembershipFixture{store: st, source: source, convID: convID}
}

func (f imapMembershipFixture) createMessage(
	t *testing.T, sourceMessageID, rfc822MessageID string,
) int64 {
	t.Helper()
	messageID, err := f.store.UpsertMessage(&store.Message{
		ConversationID:  f.convID,
		SourceID:        f.source.ID,
		SourceMessageID: sourceMessageID,
		RFC822MessageID: sql.NullString{String: rfc822MessageID, Valid: rfc822MessageID != ""},
		MessageType:     "email",
	})
	require.NoError(t, err)
	return messageID
}

func membershipCount(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ?
	`), sourceID).Scan(&count))
	return count
}

func membershipMessageAndFlags(
	t *testing.T, st *store.Store, sourceID int64, uidValidity, uid uint32,
) (int64, string) {
	t.Helper()
	var messageID int64
	var flags string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT message_id, flags FROM imap_message_memberships
		WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?
	`), sourceID, "INBOX", uidValidity, uid).Scan(&messageID, &flags))
	return messageID, flags
}

func messageLabels(t *testing.T, st *store.Store, messageID int64) []string {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT l.name
		FROM labels l
		JOIN message_labels ml ON ml.label_id = l.id
		WHERE ml.message_id = ?
		ORDER BY l.name
	`), messageID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var labels []string
	for rows.Next() {
		var label string
		require.NoError(t, rows.Scan(&label))
		labels = append(labels, label)
	}
	require.NoError(t, rows.Err())
	return labels
}

func messageTombstoned(t *testing.T, st *store.Store, messageID int64) bool {
	t.Helper()
	var deleted sql.NullTime
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT deleted_from_source_at FROM messages WHERE id = ?
	`), messageID).Scan(&deleted))
	return deleted.Valid
}

func messageIsRead(t *testing.T, st *store.Store, messageID int64) bool {
	t.Helper()
	var isRead bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT is_read FROM messages WHERE id = ?
	`), messageID).Scan(&isRead))
	return isRead
}

func TestApplyIMAPMailboxDeltas_PersistsMembershipsFlagsLabelsAndKnownUIDs(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	exactID := f.createMessage(t, "provider-exact", "<exact@example.com>")
	fallbackID := f.createMessage(t, "provider-fallback", "<fallback@example.com>")
	_, err := f.store.DB().Exec(f.store.Rebind(`
		UPDATE messages SET is_read = FALSE WHERE id = ?
	`), exactID)
	require.NoError(err)

	delta := store.IMAPMailboxDelta{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 17, UIDNext: 103, HighestModSeq: 900,
		},
		Memberships: []store.IMAPMembershipObservation{
			{
				Mailbox: "INBOX", UIDValidity: 17, UID: 101,
				SourceMessageID: "provider-exact", RFC822MessageID: "<wrong@example.com>",
				Flags: []string{"\\Seen", "$Forwarded"},
			},
			{
				Mailbox: "INBOX", UIDValidity: 17, UID: 102,
				RFC822MessageID: "<fallback@example.com>", Flags: []string{"\\Flagged"},
			},
		},
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))

	known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(err)
	assert.Equal(map[string][]uint32{"INBOX": {101, 102}}, known)
	assert.Equal(2, membershipCount(t, f.store, f.source.ID))
	gotExactID, gotExactFlags := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 101)
	gotFallbackID, gotFallbackFlags := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 102)
	assert.Equal(exactID, gotExactID, "exact source identity must win over RFC822 Message-ID")
	assert.JSONEq(`["\\Seen","$Forwarded"]`, gotExactFlags)
	assert.False(messageIsRead(t, f.store, exactID), "provider flags must not change local read state")
	assert.Equal(fallbackID, gotFallbackID)
	assert.JSONEq(`["\\Flagged"]`, gotFallbackFlags)
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, exactID))
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, fallbackID))

	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	require.NoError(err)
	assert.Equal([]store.IMAPFolderState{delta.State}, states)

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	assert.Equal(2, membershipCount(t, f.store, f.source.ID), "replaying a delta must be idempotent")
}

func TestApplyIMAPMailboxDeltas_RFC822FallbackDoesNotCrossSources(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	otherSource, err := f.store.GetOrCreateSource("imap", "other-membership@example.com")
	require.NoError(err)
	otherConversationID, err := f.store.EnsureConversation(
		otherSource.ID, "other-membership-thread", "Other membership thread",
	)
	require.NoError(err)
	_, err = f.store.UpsertMessage(&store.Message{
		ConversationID:  otherConversationID,
		SourceID:        otherSource.ID,
		SourceMessageID: "other-provider-id",
		RFC822MessageID: sql.NullString{String: "<shared@example.com>", Valid: true},
		MessageType:     "email",
	})
	require.NoError(err)

	err = f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 5, UIDNext: 2},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 5, UID: 1, RFC822MessageID: "<shared@example.com>",
		}},
	}})
	require.Error(err)
	assert.Contains(err.Error(), "no message matches")
	assert.Zero(membershipCount(t, f.store, f.source.ID))
	states, statesErr := f.store.GetIMAPFolderStates(f.source.ID)
	require.NoError(statesErr)
	assert.Empty(states)
}

func TestApplyIMAPMailboxDeltas_NormalizesOmittedMembershipIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID := f.createMessage(t, "normalized", "<normalized@example.com>")

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{UIDValidity: 17, UIDNext: 2, HighestModSeq: 100},
		Memberships: []store.IMAPMembershipObservation{{
			UID: 1, SourceMessageID: "normalized",
		}},
	}}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
	assert.Equal(messageID, gotMessageID)
	known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(err)
	assert.Equal(map[string][]uint32{"INBOX": {1}}, known)
	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	require.NoError(err)
	assert.Equal([]store.IMAPFolderState{{
		Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2, HighestModSeq: 100,
	}}, states)
}

func TestGetIMAPKnownUIDs_PreservesEmptyMailboxBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Empty",
		State:   store.IMAPFolderState{Mailbox: "Empty", UIDValidity: 8, UIDNext: 1, HighestModSeq: 80},
	}}))

	known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(err)
	require.Contains(known, "Empty")
	assert.NotNil(known["Empty"])
	assert.Empty(known["Empty"])
}

func TestApplyIMAPMailboxDeltas_VanishedMembershipReconcilesLabelsAndTombstone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID := f.createMessage(t, "multi-mailbox", "<multi@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "INBOX",
			State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 10},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "INBOX", UIDValidity: 1, UID: 1, SourceMessageID: "multi-mailbox",
			}},
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 2, UIDNext: 8, HighestModSeq: 20},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Archive", UIDValidity: 2, UID: 7, SourceMessageID: "multi-mailbox",
			}},
		},
	}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox:      "INBOX",
			State:        store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 11},
			VanishedUIDs: []uint32{1},
		},
		{
			Mailbox: "Archive",
			State:   store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 2, UIDNext: 8, HighestModSeq: 20},
		},
	}))

	assert.False(messageTombstoned(t, f.store, messageID))
	assert.Equal([]string{"Archive"}, messageLabels(t, f.store, messageID))
	known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(err)
	assert.Equal(map[string][]uint32{"Archive": {7}, "INBOX": {}}, known)

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox: "INBOX",
			State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 11},
		},
		{
			Mailbox:      "Archive",
			State:        store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 2, UIDNext: 8, HighestModSeq: 21},
			VanishedUIDs: []uint32{7},
		},
	}))
	assert.True(messageTombstoned(t, f.store, messageID))
	assert.Empty(messageLabels(t, f.store, messageID))
}

func TestApplyIMAPMailboxDeltas_ReappearanceClearsTombstone(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID := f.createMessage(t, "reappearing", "<reappearing@example.com>")
	require.NoError(f.store.MarkMessageDeleted(f.source.ID, "reappearing"))
	assert.True(messageTombstoned(t, f.store, messageID))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Recovered",
		State:   store.IMAPFolderState{Mailbox: "Recovered", UIDValidity: 3, UIDNext: 45, HighestModSeq: 30},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "Recovered", UIDValidity: 3, UID: 44, RFC822MessageID: "<reappearing@example.com>",
		}},
	}}))

	assert.False(messageTombstoned(t, f.store, messageID))
	assert.Equal([]string{"Recovered"}, messageLabels(t, f.store, messageID))
}

func TestApplyIMAPMailboxDeltas_UIDValidityResetDeletesOldEpoch(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	removedID := f.createMessage(t, "old-only", "<old-only@example.com>")
	survivingID := f.createMessage(t, "old-and-new", "<old-and-new@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 3, HighestModSeq: 100},
		Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 10, UID: 1, SourceMessageID: "old-only"},
			{Mailbox: "INBOX", UIDValidity: 10, UID: 2, SourceMessageID: "old-and-new"},
		},
	}}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 20, UIDNext: 10, HighestModSeq: 200},
		Reset:   true,
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 20, UID: 9, SourceMessageID: "old-and-new",
		}},
	}}))

	assert.Equal(1, membershipCount(t, f.store, f.source.ID))
	known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(err)
	assert.Equal(map[string][]uint32{"INBOX": {9}}, known)
	assert.True(messageTombstoned(t, f.store, removedID))
	assert.False(messageTombstoned(t, f.store, survivingID))
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, survivingID))
}

func TestApplyIMAPMailboxDeltas_RetiresMailboxesAbsentFromAuthoritativeTopology(t *testing.T) {
	t.Run("deleted mailbox reconciles labels and tombstones last membership", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newIMAPMembershipFixture(t)
		retiredOnlyID := f.createMessage(t, "retired-only", "<retired-only@example.com>")
		sharedID := f.createMessage(t, "shared", "<shared-topology@example.com>")
		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
			{
				Mailbox: "INBOX",
				State: store.IMAPFolderState{
					Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 10,
				},
				Memberships: []store.IMAPMembershipObservation{{
					UID: 1, SourceMessageID: "shared",
				}},
			},
			{
				Mailbox: "Retired",
				State: store.IMAPFolderState{
					Mailbox: "Retired", UIDValidity: 2, UIDNext: 3, HighestModSeq: 20,
				},
				Memberships: []store.IMAPMembershipObservation{
					{UID: 1, SourceMessageID: "retired-only"},
					{UID: 2, SourceMessageID: "shared"},
				},
			},
		}))

		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
			Mailbox: "INBOX",
			State: store.IMAPFolderState{
				Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 11,
			},
		}}))

		states, err := f.store.GetIMAPFolderStates(f.source.ID)
		require.NoError(err)
		assert.Equal([]store.IMAPFolderState{{
			Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 11,
		}}, states)
		assert.Equal(1, membershipCount(t, f.store, f.source.ID))
		assert.True(messageTombstoned(t, f.store, retiredOnlyID))
		assert.Empty(messageLabels(t, f.store, retiredOnlyID))
		assert.False(messageTombstoned(t, f.store, sharedID))
		assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, sharedID))
	})

	t.Run("rename removes the old cursor without recreating it", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newIMAPMembershipFixture(t)
		messageID := f.createMessage(t, "Old|1", "<renamed-topology@example.com>")
		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
			Mailbox: "Old",
			State: store.IMAPFolderState{
				Mailbox: "Old", UIDValidity: 3, UIDNext: 2, HighestModSeq: 30,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, SourceMessageID: "Old|1",
			}},
		}}))

		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
			Mailbox: "New",
			State: store.IMAPFolderState{
				Mailbox: "New", UIDValidity: 4, UIDNext: 2, HighestModSeq: 40,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, RFC822MessageID: "<renamed-topology@example.com>",
			}},
		}}))

		states, err := f.store.GetIMAPFolderStates(f.source.ID)
		require.NoError(err)
		assert.Equal([]store.IMAPFolderState{{
			Mailbox: "New", UIDValidity: 4, UIDNext: 2, HighestModSeq: 40,
		}}, states)
		assert.Equal([]string{"New"}, messageLabels(t, f.store, messageID))
		assert.False(messageTombstoned(t, f.store, messageID))
	})

	t.Run("zero current mailboxes clears every cursor and membership", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newIMAPMembershipFixture(t)
		messageID := f.createMessage(t, "Solo|1", "<zero-topology@example.com>")
		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
			Mailbox: "Solo",
			State: store.IMAPFolderState{
				Mailbox: "Solo", UIDValidity: 5, UIDNext: 2, HighestModSeq: 50,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, SourceMessageID: "Solo|1",
			}},
		}}))

		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{}))

		states, err := f.store.GetIMAPFolderStates(f.source.ID)
		require.NoError(err)
		assert.Empty(states)
		assert.Zero(membershipCount(t, f.store, f.source.ID))
		assert.Empty(messageLabels(t, f.store, messageID))
		assert.True(messageTombstoned(t, f.store, messageID))
	})

	t.Run("nil topology is rejected without changing saved state", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newIMAPMembershipFixture(t)
		messageID := f.createMessage(t, "Preserved|1", "<nil-topology@example.com>")
		require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
			Mailbox: "Preserved",
			State: store.IMAPFolderState{
				Mailbox: "Preserved", UIDValidity: 6, UIDNext: 2, HighestModSeq: 60,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, SourceMessageID: "Preserved|1",
			}},
		}}))

		err := f.store.ApplyIMAPMailboxDeltas(f.source.ID, nil)
		require.ErrorContains(err, "nil authoritative topology")

		states, statesErr := f.store.GetIMAPFolderStates(f.source.ID)
		require.NoError(statesErr)
		assert.Equal([]store.IMAPFolderState{{
			Mailbox: "Preserved", UIDValidity: 6, UIDNext: 2, HighestModSeq: 60,
		}}, states)
		assert.Equal(1, membershipCount(t, f.store, f.source.ID))
		assert.Equal([]string{"Preserved"}, messageLabels(t, f.store, messageID))
		assert.False(messageTombstoned(t, f.store, messageID))
	})
}

func TestApplyIMAPMailboxDeltas_InitialBaselineReconcilesPreviouslyArchivedMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	presentID := f.createMessage(t, "INBOX|1", "<baseline-present@example.com>")
	missingID := f.createMessage(t, "Deleted|1", "<baseline-missing@example.com>")
	inboxLabelID, err := f.store.EnsureLabel(f.source.ID, "INBOX", "INBOX", "user")
	require.NoError(err)
	deletedLabelID, err := f.store.EnsureLabel(f.source.ID, "Deleted", "Deleted", "user")
	require.NoError(err)
	require.NoError(f.store.ReplaceMessageLabels(presentID, []int64{inboxLabelID}))
	require.NoError(f.store.ReplaceMessageLabels(missingID, []int64{deletedLabelID}))
	assert.Zero(membershipCount(t, f.store, f.source.ID), "fixture must model the pre-membership upgrade")

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 7, UIDNext: 2, HighestModSeq: 70,
		},
		Memberships: []store.IMAPMembershipObservation{{
			UID: 1, SourceMessageID: "INBOX|1",
		}},
	}}))

	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, presentID))
	assert.False(messageTombstoned(t, f.store, presentID))
	assert.Empty(messageLabels(t, f.store, missingID))
	assert.True(messageTombstoned(t, f.store, missingID))
}

func TestApplyIMAPMailboxDeltas_ReconcilesLiveMessageImportedAfterBaseline(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	trackedID := f.createMessage(t, "INBOX|1", "<tracked@example.test>")
	state := store.IMAPFolderState{
		Mailbox: "INBOX", UIDValidity: 7, UIDNext: 2, HighestModSeq: 70,
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX", State: state,
		Memberships: []store.IMAPMembershipObservation{{
			UID: 1, SourceMessageID: "INBOX|1",
		}},
	}}))

	untrackedID := f.createMessage(t, "Archive|1", "<untracked@example.test>")
	archiveLabelID, err := f.store.EnsureLabel(f.source.ID, "Archive", "Archive", "user")
	require.NoError(err)
	require.NoError(f.store.ReplaceMessageLabels(untrackedID, []int64{archiveLabelID}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX", State: state,
	}}))

	assert.False(messageTombstoned(t, f.store, trackedID))
	assert.True(messageTombstoned(t, f.store, untrackedID))
	assert.Empty(messageLabels(t, f.store, untrackedID))
}

func TestApplyIMAPMailboxDeltas_CanonicalSourceHintWinsOverSecondaryCopy(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	canonicalID := f.createMessage(t, "[Gmail]/All Mail|1", "")
	secondaryID := f.createMessage(t, "INBOX|1", "")

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State: store.IMAPFolderState{
			Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2, HighestModSeq: 100,
		},
		Memberships: []store.IMAPMembershipObservation{{
			UID: 1, SourceMessageID: "INBOX|1",
			CanonicalSourceMessageID: "[Gmail]/All Mail|1",
		}},
	}}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
	assert.Equal(canonicalID, gotMessageID)
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, canonicalID))
	assert.True(messageTombstoned(t, f.store, secondaryID))
}

func TestApplyIMAPMailboxDeltas_RawDigestResolvesDurableCanonicalMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	canonicalID := f.createMessage(t, "[Gmail]/All Mail|1", "")
	secondaryID := f.createMessage(t, "INBOX|1", "")
	raw := []byte("From: sender@example.test\r\nSubject: no identity\r\n\r\nbody\r\n")
	require.NoError(f.store.UpsertMessageRaw(canonicalID, raw))
	require.NoError(f.store.UpsertMessageRaw(secondaryID, raw))
	allMailState := store.IMAPFolderState{
		Mailbox: "[Gmail]/All Mail", UIDValidity: 16, UIDNext: 2, HighestModSeq: 90,
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "[Gmail]/All Mail", State: allMailState,
		Memberships: []store.IMAPMembershipObservation{{
			UID: 1, SourceMessageID: "[Gmail]/All Mail|1",
		}},
	}}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "[Gmail]/All Mail", State: allMailState}, {
			Mailbox: "INBOX", State: store.IMAPFolderState{
				Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2, HighestModSeq: 100,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, SourceMessageID: "INBOX|1", RawSHA256: sha256.Sum256(raw),
			}},
		}}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
	assert.Equal(canonicalID, gotMessageID)
	assert.Equal([]string{"INBOX", "[Gmail]/All Mail"}, messageLabels(t, f.store, canonicalID))
	assert.True(messageTombstoned(t, f.store, secondaryID))
}

func TestApplyIMAPMailboxDeltas_AmbiguousRawDigestFallsBackToExactSourceID(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	firstID := f.createMessage(t, "[Gmail]/All Mail|1", "")
	secondID := f.createMessage(t, "[Gmail]/All Mail|2", "")
	secondaryID := f.createMessage(t, "INBOX|1", "")
	raw := []byte("From: sender@example.test\r\nSubject: ambiguous identity\r\n\r\nbody\r\n")
	for _, messageID := range []int64{firstID, secondID, secondaryID} {
		require.NoError(f.store.UpsertMessageRaw(messageID, raw))
	}
	allMailState := store.IMAPFolderState{
		Mailbox: "[Gmail]/All Mail", UIDValidity: 16, UIDNext: 3, HighestModSeq: 90,
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "[Gmail]/All Mail", State: allMailState,
		Memberships: []store.IMAPMembershipObservation{
			{UID: 1, SourceMessageID: "[Gmail]/All Mail|1"},
			{UID: 2, SourceMessageID: "[Gmail]/All Mail|2"},
		},
	}}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "[Gmail]/All Mail", State: allMailState}, {
			Mailbox: "INBOX", State: store.IMAPFolderState{
				Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2, HighestModSeq: 100,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, SourceMessageID: "INBOX|1", RawSHA256: sha256.Sum256(raw),
			}},
		}}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
	assert.Equal(secondaryID, gotMessageID)
	assert.False(messageTombstoned(t, f.store, firstID))
	assert.False(messageTombstoned(t, f.store, secondID))
	assert.False(messageTombstoned(t, f.store, secondaryID))
}

func TestApplyIMAPMailboxDeltas_RawResolutionUsesPreMutationMembershipSnapshot(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	canonicalID := f.createMessage(t, "Old|1", "")
	secondaryID := f.createMessage(t, "INBOX|1", "")
	raw := []byte("From: sender@example.test\r\nSubject: moved identity\r\n\r\nbody\r\n")
	require.NoError(f.store.UpsertMessageRaw(canonicalID, raw))
	require.NoError(f.store.UpsertMessageRaw(secondaryID, raw))
	oldState := store.IMAPFolderState{
		Mailbox: "Old", UIDValidity: 16, UIDNext: 2, HighestModSeq: 90,
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "Old", State: oldState,
		Memberships: []store.IMAPMembershipObservation{{
			UID: 1, SourceMessageID: "Old|1",
		}},
	}}))

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "Old", State: oldState, Reset: true},
		{
			Mailbox: "INBOX",
			State: store.IMAPFolderState{
				Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2, HighestModSeq: 100,
			},
			Memberships: []store.IMAPMembershipObservation{{
				UID: 1, SourceMessageID: "INBOX|1", RawSHA256: sha256.Sum256(raw),
			}},
		},
	}))

	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
	assert.Equal(canonicalID, gotMessageID)
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, canonicalID))
	assert.True(messageTombstoned(t, f.store, secondaryID))
}

func TestApplyIMAPMailboxDeltas_UnresolvedIdentityRollsBackEverything(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	messageID := f.createMessage(t, "transactional", "<transactional@example.com>")
	initial := store.IMAPMailboxDelta{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 10},
		Memberships: []store.IMAPMembershipObservation{{
			Mailbox: "INBOX", UIDValidity: 1, UID: 1, SourceMessageID: "transactional",
		}},
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{initial}))

	err := f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{
			Mailbox:      "INBOX",
			State:        store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 3, HighestModSeq: 20},
			VanishedUIDs: []uint32{1},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "INBOX", UIDValidity: 1, UID: 2, SourceMessageID: "transactional",
			}},
		},
		{
			Mailbox: "Broken",
			State:   store.IMAPFolderState{Mailbox: "Broken", UIDValidity: 9, UIDNext: 10, HighestModSeq: 99},
			Memberships: []store.IMAPMembershipObservation{{
				Mailbox: "Broken", UIDValidity: 9, UID: 9, RFC822MessageID: "",
			}},
		},
	})
	require.Error(err)
	assert.Contains(err.Error(), "resolve IMAP membership")

	assert.Equal(1, membershipCount(t, f.store, f.source.ID))
	gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 1, 1)
	assert.Equal(messageID, gotMessageID)
	known, knownErr := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(knownErr)
	assert.Equal(map[string][]uint32{"INBOX": {1}}, known)
	states, statesErr := f.store.GetIMAPFolderStates(f.source.ID)
	require.NoError(statesErr)
	sort.Slice(states, func(i, j int) bool { return states[i].Mailbox < states[j].Mailbox })
	assert.Equal([]store.IMAPFolderState{initial.State}, states,
		"cursor advancement must roll back with membership changes")
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, messageID))
	assert.False(messageTombstoned(t, f.store, messageID))
}

func TestApplyIMAPMailboxDeltas_ConflictingDeltaIdentityRollsBackEverything(t *testing.T) {
	tests := []struct {
		name        string
		invalid     store.IMAPMailboxDelta
		errorDetail string
	}{
		{
			name: "folder state mailbox",
			invalid: store.IMAPMailboxDelta{
				Mailbox: "Broken",
				State:   store.IMAPFolderState{Mailbox: "Elsewhere", UIDValidity: 9, UIDNext: 10},
				Memberships: []store.IMAPMembershipObservation{{
					Mailbox: "Broken", UIDValidity: 9, UID: 9, SourceMessageID: "transactional",
				}},
			},
			errorDetail: "mailbox",
		},
		{
			name: "membership mailbox",
			invalid: store.IMAPMailboxDelta{
				Mailbox: "Broken",
				State:   store.IMAPFolderState{Mailbox: "Broken", UIDValidity: 9, UIDNext: 10},
				Memberships: []store.IMAPMembershipObservation{{
					Mailbox: "Elsewhere", UIDValidity: 9, UID: 9, SourceMessageID: "transactional",
				}},
			},
			errorDetail: "mailbox",
		},
		{
			name: "membership UIDVALIDITY",
			invalid: store.IMAPMailboxDelta{
				Mailbox: "Broken",
				State:   store.IMAPFolderState{Mailbox: "Broken", UIDValidity: 9, UIDNext: 10},
				Memberships: []store.IMAPMembershipObservation{{
					Mailbox: "Broken", UIDValidity: 10, UID: 9, SourceMessageID: "transactional",
				}},
			},
			errorDetail: "UIDVALIDITY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newIMAPMembershipFixture(t)
			messageID := f.createMessage(t, "transactional", "<transactional@example.com>")
			initial := store.IMAPMailboxDelta{
				Mailbox: "INBOX",
				State: store.IMAPFolderState{
					Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2, HighestModSeq: 10,
				},
				Memberships: []store.IMAPMembershipObservation{{
					Mailbox: "INBOX", UIDValidity: 1, UID: 1, SourceMessageID: "transactional",
				}},
			}
			require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{initial}))

			err := f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
				{
					Mailbox:      "INBOX",
					State:        store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 3},
					VanishedUIDs: []uint32{1},
					Memberships: []store.IMAPMembershipObservation{{
						Mailbox: "INBOX", UIDValidity: 1, UID: 2, SourceMessageID: "transactional",
					}},
				},
				tt.invalid,
			})
			require.Error(err)
			assert.Contains(err.Error(), tt.errorDetail)

			assert.Equal(1, membershipCount(t, f.store, f.source.ID))
			gotMessageID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 1, 1)
			assert.Equal(messageID, gotMessageID)
			known, knownErr := f.store.GetIMAPKnownUIDs(f.source.ID)
			require.NoError(knownErr)
			assert.Equal(map[string][]uint32{"INBOX": {1}}, known)
			states, statesErr := f.store.GetIMAPFolderStates(f.source.ID)
			require.NoError(statesErr)
			assert.Equal([]store.IMAPFolderState{initial.State}, states)
			assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, messageID))
			assert.False(messageTombstoned(t, f.store, messageID))
		})
	}
}

const membershipStamp = "1999-01-01 00:00:00"

// stampMemberships rewrites updated_at on every saved membership so a later
// apply can be measured by which rows moved off the stamp.
func stampMemberships(t *testing.T, st *store.Store, sourceID int64) {
	t.Helper()
	_, err := st.DB().Exec(st.Rebind(`
		UPDATE imap_message_memberships SET updated_at = ? WHERE source_id = ?
	`), membershipStamp, sourceID)
	require.NoError(t, err)
}

// membershipsWritten counts the rows an apply inserted or updated since the
// last stampMemberships call.
func membershipsWritten(t *testing.T, st *store.Store, sourceID int64) int {
	t.Helper()
	var count int
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM imap_message_memberships
		WHERE source_id = ? AND updated_at <> ?
	`), sourceID, membershipStamp).Scan(&count))
	return count
}

func TestApplyIMAPMailboxDeltas_UnchangedResetWritesNothing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	firstID := f.createMessage(t, "reset-first", "<reset-first@example.com>")
	secondID := f.createMessage(t, "reset-second", "<reset-second@example.com>")
	delta := store.IMAPMailboxDelta{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 5, UIDNext: 3, HighestModSeq: 40},
		Memberships: []store.IMAPMembershipObservation{
			{
				Mailbox: "INBOX", UIDValidity: 5, UID: 1,
				SourceMessageID: "reset-first", Flags: []string{"\\Seen"},
			},
			{Mailbox: "INBOX", UIDValidity: 5, UID: 2, SourceMessageID: "reset-second"},
		},
	}
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	stampMemberships(t, f.store, f.source.ID)

	delta.Reset = true
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))

	assert.Equal(0, membershipsWritten(t, f.store, f.source.ID),
		"a reset that observes the saved set must rewrite no membership")
	assert.Equal(2, membershipCount(t, f.store, f.source.ID))
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, firstID))
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, secondID))
	assert.False(messageTombstoned(t, f.store, firstID))
	assert.False(messageTombstoned(t, f.store, secondID))
}

func TestApplyIMAPMailboxDeltas_ResetWritesOnlyChangedMemberships(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	expungedID := f.createMessage(t, "reset-expunged", "<reset-expunged@example.com>")
	keptID := f.createMessage(t, "reset-kept", "<reset-kept@example.com>")
	arrivedID := f.createMessage(t, "reset-arrived", "<reset-arrived@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 5, UIDNext: 3, HighestModSeq: 40},
		Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 5, UID: 1, SourceMessageID: "reset-expunged"},
			{Mailbox: "INBOX", UIDValidity: 5, UID: 2, SourceMessageID: "reset-kept"},
		},
	}}))
	stampMemberships(t, f.store, f.source.ID)

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 5, UIDNext: 4, HighestModSeq: 41},
		Reset:   true,
		Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 5, UID: 2, SourceMessageID: "reset-kept"},
			{Mailbox: "INBOX", UIDValidity: 5, UID: 3, SourceMessageID: "reset-arrived"},
		},
	}}))

	assert.Equal(1, membershipsWritten(t, f.store, f.source.ID),
		"only the arrival is written; the kept UID keeps its stamp")
	known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
	require.NoError(err)
	assert.Equal(map[string][]uint32{"INBOX": {2, 3}}, known)
	gotKeptID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 5, 2)
	assert.Equal(keptID, gotKeptID)
	assert.True(messageTombstoned(t, f.store, expungedID))
	assert.Empty(messageLabels(t, f.store, expungedID))
	assert.Equal([]string{"INBOX"}, messageLabels(t, f.store, arrivedID))
	assert.False(messageTombstoned(t, f.store, keptID))
}

func TestApplyIMAPMailboxDeltas_ResetPersistsFlagChange(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newIMAPMembershipFixture(t)
	f.createMessage(t, "reset-flagged", "<reset-flagged@example.com>")
	f.createMessage(t, "reset-steady", "<reset-steady@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 5, UIDNext: 3, HighestModSeq: 40},
		Memberships: []store.IMAPMembershipObservation{
			{
				Mailbox: "INBOX", UIDValidity: 5, UID: 1,
				SourceMessageID: "reset-flagged", Flags: []string{"\\Seen"},
			},
			{
				Mailbox: "INBOX", UIDValidity: 5, UID: 2,
				SourceMessageID: "reset-steady", Flags: []string{"\\Seen"},
			},
		},
	}}))
	stampMemberships(t, f.store, f.source.ID)

	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
		Mailbox: "INBOX",
		State:   store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 5, UIDNext: 3, HighestModSeq: 41},
		Reset:   true,
		Memberships: []store.IMAPMembershipObservation{
			{
				Mailbox: "INBOX", UIDValidity: 5, UID: 1,
				SourceMessageID: "reset-flagged", Flags: []string{"\\Seen", "\\Flagged"},
			},
			{
				Mailbox: "INBOX", UIDValidity: 5, UID: 2,
				SourceMessageID: "reset-steady", Flags: []string{"\\Seen"},
			},
		},
	}}))

	assert.Equal(1, membershipsWritten(t, f.store, f.source.ID),
		"a flag change must still be written, and only it")
	_, gotFlags := membershipMessageAndFlags(t, f.store, f.source.ID, 5, 1)
	assert.JSONEq(`["\\Seen","\\Flagged"]`, gotFlags)
	_, gotSteadyFlags := membershipMessageAndFlags(t, f.store, f.source.ID, 5, 2)
	assert.JSONEq(`["\\Seen"]`, gotSteadyFlags)
}
