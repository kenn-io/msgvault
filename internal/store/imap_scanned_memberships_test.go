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
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		scannedIMAPDelta("INBOX", 11, "INBOX|1", "INBOX|2"),
		scannedIMAPDelta("Trash", 22, "Trash|1", "Trash|2", "INBOX|2"),
	}))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE messages
		SET deleted_from_source_at = '2026-01-01 00:00:00' WHERE id = ?`), skippedDeleted)
	require.NoError(err)
	deletedBefore, err := f.store.GetMessage(skippedDeleted)
	require.NoError(err)
	require.NotNil(deletedBefore.DeletedAt)
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
	deletedAfter, err := f.store.GetMessage(skippedDeleted)
	require.NoError(err)
	assert.True(messageTombstoned(t, f.store, skippedDeleted))
	assert.Equal(deletedBefore, deletedAfter)
	assert.False(messageTombstoned(t, f.store, legacy))
	assert.Equal([]string{"Trash"}, messageLabels(t, f.store, shared))
}

func TestApplyIMAPScannedMailboxDeltas_ReconcilesUntrackedRows(t *testing.T) {
	for _, name := range []string{"incremental", "empty mailbox", "new generation"} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newIMAPIdentityFixture(t)
			require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
				scannedIMAPDelta("INBOX", 11), scannedIMAPDelta("Archive|Projects", 21),
			}))
			// Earlier filtered syncs archived these rows without memberships.
			moved := f.createMessage(t, "INBOX|1", "<moved@example.com>")
			retained := f.createMessage(t, "INBOX|2", "<retained@example.com>")
			archiveOnly := f.createMessage(t, "INBOX|3", "")
			archiveMoved := f.createMessage(t, "Archive|Projects|1", "")
			archiveRetained := f.createMessage(t, "Archive|Projects|2", "")
			malformed := f.createMessage(t, "INBOX|01", "")
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO source_merge_archive_only_messages (message_id) VALUES (?)`), archiveOnly)
			require.NoError(err)
			label, err := f.store.EnsureLabel(t.Context(), f.source.ID, "INBOX", "INBOX", "system")
			require.NoError(err)
			require.NoError(f.store.ReplaceMessageLabels(moved, []int64{label}))
			delta := scannedIMAPDelta("INBOX", 11)
			delta.KnownUIDs = []uint32{2}
			var newID int64
			switch name {
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
				t.Context(), f.source.ID, completedScannedIMAPSync(t, f), []store.IMAPMailboxDelta{
					delta,
					{Mailbox: "Archive|Projects", State: store.IMAPFolderState{Mailbox: "Archive|Projects", UIDValidity: 21}, KnownUIDs: []uint32{2}},
				},
			))
			assert.True(messageTombstoned(t, f.store, moved))
			assert.Empty(messageLabels(t, f.store, moved))
			assert.Equal(name == "empty mailbox", messageTombstoned(t, f.store, retained))
			assert.False(messageTombstoned(t, f.store, archiveOnly))
			assert.True(messageTombstoned(t, f.store, archiveMoved))
			assert.False(messageTombstoned(t, f.store, archiveRetained))
			assert.False(messageTombstoned(t, f.store, malformed))
			if name == "new generation" {
				gotID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 12, 1)
				assert.Equal(newID, gotID)
			}
		})
	}
}

// A scoped apply that skips Trash cannot see it, so it clears a deletion mark
// only for a message it found in a scanned folder outside Trash. Saved
// memberships in skipped mailboxes never count as that evidence.
func TestApplyIMAPScannedMailboxDeltas_TrashDeletionMarks(t *testing.T) {
	for _, tc := range []struct {
		name string
		// savedTrash records the message in Trash during the prior complete sync.
		savedTrash bool
		// marked simulates msgvault moving the message to Trash.
		marked bool
		// scanned lists the mailboxes the scoped sync finds the message in.
		// "Trash" is scanned with the Trash role, as when Trash also advertises
		// All Mail and the saved exclusions skip only Junk.
		scanned    []string
		wantMarked bool
	}{
		{name: "msgvault trashed then restored to INBOX", marked: true, scanned: []string{"INBOX"}},
		{name: "msgvault trashed and still in skipped Trash", marked: true, wantMarked: true},
		{name: "msgvault trashed with a saved Trash membership", savedTrash: true, marked: true, wantMarked: true},
		{name: "moved to skipped Trash by the user", wantMarked: true},
		{name: "marked but in INBOX and skipped Trash", savedTrash: true, marked: true, scanned: []string{"INBOX"}},
		{name: "unmarked in INBOX and skipped Trash", savedTrash: true, scanned: []string{"INBOX"}},
		{name: "marked in scanned Trash role mailbox and INBOX", marked: true, scanned: []string{"INBOX", "Trash"}, wantMarked: true},
		{name: "marked in scanned Trash role mailbox only", marked: true, scanned: []string{"Trash"}, wantMarked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIMAPIdentityFixture(t)
			id := f.createMessage(t, "INBOX|1", "<trash-mark@example.com>")
			trash := scannedIMAPDelta("Trash", 22)
			if tc.savedTrash {
				trash = scannedIMAPDelta("Trash", 22, "INBOX|1")
			}
			trash.Trash = true
			require.NoError(t, f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
				scannedIMAPDelta("INBOX", 11, "INBOX|1"), trash,
			}))
			if tc.marked {
				require.NoError(t, f.store.MarkMessageDeleted(f.source.ID, "INBOX|1"))
			}

			deltas := []store.IMAPMailboxDelta{scannedIMAPDelta("INBOX", 11)}
			if slices.Contains(tc.scanned, "INBOX") {
				deltas[0] = scannedIMAPDelta("INBOX", 11, "INBOX|1")
			}
			if slices.Contains(tc.scanned, "Trash") {
				scannedTrash := scannedIMAPDelta("Trash", 22, "INBOX|1")
				scannedTrash.Trash = true
				deltas = append(deltas, scannedTrash)
			}
			require.NoError(t, f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(
				t.Context(), f.source.ID, completedScannedIMAPSync(t, f), deltas,
			))
			assert.Equal(t, tc.wantMarked, messageTombstoned(t, f.store, id))
		})
	}
}
