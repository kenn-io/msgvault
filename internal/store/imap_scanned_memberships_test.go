package store_test

import (
	"fmt"
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

// Include timestamps and source keys: preserving row counts alone would miss
// an accidental rewrite or retirement of the skipped mailbox's epoch.
func skippedIMAPSnapshot(t *testing.T, f imapMembershipFixture) []string {
	t.Helper()
	var snapshot []string
	rows, err := f.store.DB().Query(f.store.Rebind(`
		SELECT uidvalidity, uidnext, highest_modseq, CAST(updated_at AS TEXT)
		FROM imap_folder_state WHERE source_id = ? AND mailbox = 'Trash'
	`), f.source.ID)
	require.NoError(t, err)
	for rows.Next() {
		var epoch, next uint32
		var modseq, updated string
		require.NoError(t, rows.Scan(&epoch, &next, &modseq, &updated))
		snapshot = append(snapshot, fmt.Sprint(epoch, next, modseq, updated))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	rows, err = f.store.DB().Query(f.store.Rebind(`
		SELECT m.uidvalidity, m.uid, m.message_id, CAST(m.flags AS TEXT),
		       CAST(m.updated_at AS TEXT), messages.source_message_id
		FROM imap_message_memberships m JOIN messages ON messages.id = m.message_id
		WHERE m.source_id = ? AND m.mailbox = 'Trash' ORDER BY m.uid
	`), f.source.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	for rows.Next() {
		var epoch, uid uint32
		var id int64
		var flags, updated, key string
		require.NoError(t, rows.Scan(&epoch, &uid, &id, &flags, &updated, &key))
		snapshot = append(snapshot, fmt.Sprint(epoch, uid, id, flags, updated, key))
	}
	require.NoError(t, rows.Err())
	return snapshot
}

func TestApplyIMAPScannedMailboxDeltas_PreservesSkippedMailboxes(t *testing.T) {
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
	_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE messages
		SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), skippedDeleted)
	require.NoError(err)
	legacy := f.createMessage(t, "Trash|9", "")
	before := skippedIMAPSnapshot(t, f)
	require.Len(before, 4)

	require.NoError(f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(
		t.Context(), f.source.ID, completedScannedIMAPSync(t, f),
		[]store.IMAPMailboxDelta{scannedIMAPDelta("INBOX", 11)}, []string{"INBOX", "Trash"},
	))

	assert.Equal(before, skippedIMAPSnapshot(t, f))
	assert.True(messageTombstoned(t, f.store, removed))
	assert.False(messageTombstoned(t, f.store, shared))
	assert.False(messageTombstoned(t, f.store, skipped))
	assert.True(messageTombstoned(t, f.store, skippedDeleted))
	assert.False(messageTombstoned(t, f.store, legacy))
	assert.Equal([]string{"Trash"}, messageLabels(t, f.store, shared))
}

func TestApplyIMAPScannedMailboxDeltas_UIDValidityChange(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newIMAPIdentityFixture(t)
	old := f.createMessage(t, "INBOX|1", "<old@example.com>")
	skipped := f.createMessage(t, "Trash|1", "<skipped@example.com>")
	require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		scannedIMAPDelta("INBOX", 11, "INBOX|1"), scannedIMAPDelta("Trash", 22, "Trash|1"),
	}))
	newID := f.createMessage(t, "new-message", "<new@example.com>")
	delta := scannedIMAPDelta("INBOX", 12)
	delta.Memberships = []store.IMAPMembershipObservation{{
		UID: 1, SourceMessageID: "INBOX|1", RFC822MessageID: "<new@example.com>",
	}}
	delta.State.UIDNext = 2
	require.NoError(f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(
		t.Context(), f.source.ID, completedScannedIMAPSync(t, f), []store.IMAPMailboxDelta{delta}, []string{"INBOX", "Trash"},
	))
	assert.True(messageTombstoned(t, f.store, old))
	assert.False(messageTombstoned(t, f.store, skipped))
	gotID, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 12, 1)
	assert.Equal(newID, gotID)
}

func TestApplyIMAPScannedMailboxDeltas_EmptyAndFailedApplyPreserveState(t *testing.T) {
	for _, name := range []string{"empty", "unresolved identity", "running", "newer running", "newer failed"} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			f := newIMAPIdentityFixture(t)
			id := f.createMessage(t, "Trash|1", "")
			require.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
				scannedIMAPDelta("Trash", 22, "Trash|1"),
			}))
			before := skippedIMAPSnapshot(t, f)
			run := completedScannedIMAPSync(t, f)
			deltas := []store.IMAPMailboxDelta{}
			wantErr := false
			switch name {
			case "unresolved identity":
				deltas = []store.IMAPMailboxDelta{scannedIMAPDelta("Trash", 23, "missing")}
				wantErr = true
			case "running", "newer running", "newer failed":
				newer, err := f.store.StartSync(f.source.ID, "full")
				require.NoError(err)
				if name == "running" {
					run = newer
				}
				if name == "newer failed" {
					require.NoError(f.store.FailSync(newer, "synthetic interruption"))
				}
				deltas = []store.IMAPMailboxDelta{scannedIMAPDelta("Trash", 22)}
				wantErr = true
			}
			err := f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(t.Context(), f.source.ID, run, deltas, []string{"Trash"})
			if wantErr {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			assert.Equal(before, skippedIMAPSnapshot(t, f))
			assert.False(messageTombstoned(t, f.store, id))
		})
	}
}

func TestApplyIMAPScannedMailboxDeltas_RetiresDeletedMailbox(t *testing.T) {
	f := newIMAPIdentityFixture(t)
	deleted := f.createMessage(t, "Old|1", "")
	skipped := f.createMessage(t, "Trash|1", "")
	require.NoError(t, f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		scannedIMAPDelta("INBOX", 11), scannedIMAPDelta("Old", 33, "Old|1"), scannedIMAPDelta("Trash", 22, "Trash|1"),
	}))
	require.NoError(t, f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(
		t.Context(), f.source.ID, completedScannedIMAPSync(t, f),
		[]store.IMAPMailboxDelta{scannedIMAPDelta("INBOX", 11)}, []string{"INBOX", "Trash"},
	))
	assert.True(t, messageTombstoned(t, f.store, deleted))
	assert.False(t, messageTombstoned(t, f.store, skipped))
	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	require.NoError(t, err)
	for _, state := range states {
		assert.NotEqual(t, "Old", state.Mailbox)
	}
}

func TestApplyIMAPScannedMailboxDeltas_ReconcilesUntrackedScannedRows(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "incremental"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			f := newIMAPIdentityFixture(t)
			// Earlier filtered syncs saved cursors and content without memberships.
			require.NoError(t, f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{
				{Mailbox: "INBOX", UIDValidity: 11, UIDNext: 2},
				{Mailbox: "Trash", UIDValidity: 22, UIDNext: 2},
			}))
			moved := f.createMessage(t, "INBOX|1", "")
			skipped := f.createMessage(t, "Trash|1", "")
			delta := scannedIMAPDelta("INBOX", 11)
			delta.Reset = reset
			if !reset {
				delta.VanishedUIDs = []uint32{1}
			}
			require.NoError(t, f.store.ApplyIMAPScannedMailboxDeltasForSyncContext(t.Context(), f.source.ID, completedScannedIMAPSync(t, f), []store.IMAPMailboxDelta{delta}, []string{"INBOX", "Trash"}))
			assert.True(t, messageTombstoned(t, f.store, moved))
			assert.False(t, messageTombstoned(t, f.store, skipped))
		})
	}
}
