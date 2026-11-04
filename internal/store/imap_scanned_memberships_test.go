package store_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func completedScannedIMAPSync(t *testing.T, f imapMembershipFixture) int64 {
	t.Helper()
	run, err := f.store.StartSync(f.source.ID, "full")
	require.NoError(t, err)
	require.NoError(t, f.store.CompleteSync(run, "0"))
	return run
}

func scannedIMAPDelta(mailbox string, epoch uint32, keys ...string) store.IMAPMailboxDelta {
	delta := store.IMAPMailboxDelta{
		Mailbox: mailbox, Reset: true,
		State: store.IMAPFolderState{Mailbox: mailbox, UIDValidity: epoch, UIDNext: uint32(len(keys) + 1)},
	}
	for i, key := range keys {
		delta.Memberships = append(delta.Memberships, store.IMAPMembershipObservation{
			UID: uint32(i + 1), SourceMessageID: key,
		})
	}
	return delta
}

func TestApplyIMAPScannedMailboxDeltas_PreservesSkippedAndAbsentMailboxes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newIMAPIdentityFixture(t)
	removed := f.createMessage(t, "INBOX|1", "")
	shared := f.createMessage(t, "INBOX|2", "")
	skipped := f.createMessage(t, "Trash|1", "")
	skippedDeleted := f.createMessage(t, "Trash|2", "")
	absent := f.createMessage(t, "Old|1", "")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		scannedIMAPDelta("INBOX", 11, "INBOX|1", "INBOX|2"),
		scannedIMAPDelta("Old", 33, "Old|1"),
		scannedIMAPDelta("Trash", 22, "Trash|1", "Trash|2", "INBOX|2"),
	}))
	_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE messages
		SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), skippedDeleted)
	require.NoError(err)
	legacy := f.createMessage(t, "Trash|9", "")
	unscanned := func() ([]store.IMAPFolderState, map[string][]uint32) {
		states, err := f.store.GetIMAPFolderStates(f.source.ID)
		require.NoError(err)
		known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
		require.NoError(err)
		delete(known, "INBOX")
		return slices.DeleteFunc(states, func(state store.IMAPFolderState) bool { return state.Mailbox == "INBOX" }), known
	}
	statesBefore, knownBefore := unscanned()

	require.NoError(f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(
		t.Context(), f.source.ID, completedScannedIMAPSync(t, f),
		[]store.IMAPMailboxDelta{scannedIMAPDelta("INBOX", 11)},
	))

	statesAfter, knownAfter := unscanned()
	assert.Equal(statesBefore, statesAfter)
	assert.Equal(knownBefore, knownAfter)
	assert.True(messageTombstoned(t, f.store, removed))
	assert.False(messageTombstoned(t, f.store, shared))
	assert.False(messageTombstoned(t, f.store, skipped))
	assert.True(messageTombstoned(t, f.store, skippedDeleted))
	assert.False(messageTombstoned(t, f.store, legacy))
	assert.False(messageTombstoned(t, f.store, absent))
	assert.Equal([]string{"Trash"}, messageLabels(t, f.store, shared))
	assert.Equal([]string{"Old"}, messageLabels(t, f.store, absent))
}

func TestApplyIMAPScannedMailboxDeltas_ReconcilesUntrackedRows(t *testing.T) {
	for _, name := range []string{"first scan", "incremental", "empty mailbox", "new generation"} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newIMAPIdentityFixture(t)
			if name != "first scan" {
				require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{scannedIMAPDelta("INBOX", 11)}))
			}
			// Earlier filtered syncs archived these rows without memberships.
			moved := f.createMessage(t, "INBOX|1", "<moved@example.com>")
			retained := f.createMessage(t, "INBOX|2", "<retained@example.com>")
			archiveOnly := f.createMessage(t, "INBOX|3", "")
			f.createMessage(t, "Trash|1", "")
			_, err := f.store.DB().Exec(f.store.Rebind(`INSERT INTO source_merge_archive_only_messages (message_id) VALUES (?)`), archiveOnly)
			require.NoError(err)
			label, err := f.store.EnsureLabel(f.source.ID, "INBOX", "INBOX", "system")
			require.NoError(err)
			require.NoError(f.store.ReplaceMessageLabels(moved, []int64{label}))
			delta := scannedIMAPDelta("INBOX", 11)
			delta.KnownUIDs = []uint32{2}
			var newID int64
			switch name {
			case "first scan":
				delta.Memberships = []store.IMAPMembershipObservation{{UID: 2, SourceMessageID: "INBOX|2"}}
			case "incremental":
				delta.Reset = false
				delta.Memberships = []store.IMAPMembershipObservation{{UID: 2, SourceMessageID: "INBOX|2"}}
			case "empty mailbox":
				delta.Reset, delta.KnownUIDs = false, nil
			case "new generation":
				// UID 1 now holds another message, so its old row is not kept by UID.
				newID = f.createMessage(t, "Archive|7", "<new@example.com>")
				delta = scannedIMAPDelta("INBOX", 12)
				delta.KnownUIDs = []uint32{1, 2}
				delta.Memberships = []store.IMAPMembershipObservation{
					{UID: 1, SourceMessageID: "INBOX|1", RFC822MessageID: "<new@example.com>"},
					{UID: 2, SourceMessageID: "INBOX|2", RFC822MessageID: "<retained@example.com>"},
				}
			}
			require.NoError(f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(
				t.Context(), f.source.ID, completedScannedIMAPSync(t, f), []store.IMAPMailboxDelta{delta},
			))
			assert.True(messageTombstoned(t, f.store, moved))
			assert.Empty(messageLabels(t, f.store, moved))
			assert.Equal(name == "empty mailbox", messageTombstoned(t, f.store, retained))
			assert.False(messageTombstoned(t, f.store, archiveOnly))
			if name == "new generation" {
				gotID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 12, 1)
				assert.Equal(newID, gotID)
			}
		})
	}
}
