package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
)

func inboxIMAPMoveFixture(t *testing.T) (imapMembershipFixture, inboxcontrol.State, inboxcontrol.State) {
	t.Helper()
	f := newIMAPIdentityFixture(t)
	id := f.createMessage(t, "move-canonical", "<duplicate@example.com>")
	other := f.createMessage(t, "other-canonical", "<duplicate@example.com>")
	require.NoError(t, f.store.UpsertMessageBody(id, sql.NullString{String: "Synthetic body retained on move", Valid: true}, sql.NullString{}))
	require.NoError(t, f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{
		{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 3}, Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "INBOX", UIDValidity: 10, UID: 1, SourceMessageID: "move-canonical", Flags: []string{"Todo"}},
			{Mailbox: "INBOX", UIDValidity: 10, UID: 2, SourceMessageID: "other-canonical"},
		}},
		{Mailbox: "Watch", State: store.IMAPFolderState{Mailbox: "Watch", UIDValidity: 20, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{
			{Mailbox: "Watch", UIDValidity: 20, UID: 1, SourceMessageID: "move-canonical"},
		}},
		{Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 30, UIDNext: 1}},
	}))
	target := inboxcontrol.Target{SourceID: f.source.ID, SourceType: "imap", SourceIdentifier: f.source.Identifier, AccountID: "identity@example.com", Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "move-canonical", Mailbox: "INBOX", UIDValidity: 10, UID: 1}
	before := inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), Flags: []string{"Todo"}, Tags: []string{"Todo"}, Location: "INBOX", ObservedAt: time.Now().UTC()}
	after := before
	after.Target.Mailbox, after.Target.UIDValidity, after.Target.UID = "Archive", 30, 50
	after.Inbox, after.Location, after.ObservedAt = new(false), "Archive", before.ObservedAt.Add(time.Second)
	_, err := f.store.ObserveInboxState(t.Context(), before)
	require.NoError(t, err)
	assert.False(t, messageTombstoned(t, f.store, other))
	return f, before, after
}

func TestInboxMoveReconcilePreservesOtherMemberships(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, before, after := inboxIMAPMoveFixture(t)
	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after))
	assertions.Equal(3, membershipCount(t, f.store, f.source.ID))
	assertions.Equal([]string{"Archive", "Watch"}, messageLabels(t, f.store, before.Target.ItemID))
	assertions.False(messageTombstoned(t, f.store, before.Target.ItemID))
	var inboxCount int
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT COUNT(*) FROM imap_message_memberships WHERE source_id = ? AND mailbox = 'INBOX'`), f.source.ID).Scan(&inboxCount))
	assertions.Equal(1, inboxCount, "other copy with the same Message-ID is untouched")
	body, err := f.store.GetMessageBodyText(before.Target.ItemID)
	requirements.NoError(err)
	assertions.Equal("Synthetic body retained on move", body)
	state, err := f.store.GetInboxProviderState(t.Context(), after.Target)
	requirements.NoError(err)
	requirements.NotNil(state)
	assertions.Equal(after, *state)
	old, err := f.store.GetInboxProviderState(t.Context(), before.Target)
	requirements.NoError(err)
	assertions.Nil(old, "retired membership must not remain a candidate")
	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	requirements.NoError(err)
	assertions.Len(states, 3, "partial move must not retire source topology")
	for _, state := range states {
		if state.Mailbox == "Archive" {
			assertions.Zero(state.HighestModSeq)
			assertions.Zero(state.UIDNext, "invalidate sync freshness rather than invent a complete snapshot")
		}
	}
}

func TestInboxMoveReconcileRejectsDestinationCollisionAtomically(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, before, after := inboxIMAPMoveFixture(t)
	other := f.createMessage(t, "destination-collision", "<duplicate@example.com>")
	_, err := f.store.DB().Exec(f.store.Rebind(`INSERT INTO imap_message_memberships (source_id, mailbox, uidvalidity, uid, message_id, flags) VALUES (?, 'Archive', 30, 50, ?, '[]')`), f.source.ID, other)
	requirements.NoError(err)
	err = f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after)
	require.ErrorIs(t, err, inboxcontrol.ErrConflict)
	assertions.Equal(4, membershipCount(t, f.store, f.source.ID))
	assertions.Equal([]string{"INBOX", "Watch"}, messageLabels(t, f.store, before.Target.ItemID))
	got, err := f.store.GetInboxProviderState(t.Context(), after.Target)
	requirements.NoError(err)
	assertions.Nil(got)
}

func TestInboxMoveReconcileRejectsMissingExactMapping(t *testing.T) {
	f, before, after := inboxIMAPMoveFixture(t)
	after.Target.UID = 0
	err := f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after)
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	assert.Equal(t, 3, membershipCount(t, f.store, f.source.ID))
	assert.Equal(t, []string{"INBOX", "Watch"}, messageLabels(t, f.store, before.Target.ItemID))
}

func TestInboxIMAPObservationRejectsMismatchedProviderBinding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, before, after := inboxIMAPMoveFixture(t)
	wrong := before
	wrong.Target.ProviderID = "wrong-canonical"
	_, err := f.store.ObserveInboxState(t.Context(), wrong)
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	state, err := f.store.GetInboxProviderState(t.Context(), wrong.Target)
	requirements.NoError(err)
	assertions.Nil(state)

	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after))
	before.Target.ProviderID, after.Target.ProviderID = "wrong-canonical", "wrong-canonical"
	err = f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after)
	assertions.ErrorIs(err, inboxcontrol.ErrInvalid, "repeat-move fallback must retain canonical identity checks")
}

func TestInboxMoveReconcilePreservesNewerFlags(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, before, after := inboxIMAPMoveFixture(t)
	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after))
	newer := after
	newer.Flags, newer.Read = []string{"Todo", "\\Seen"}, new(true)
	newer.ObservedAt = after.ObservedAt.Add(time.Second)
	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), after.Target, after, newer))
	err := f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after)
	require.ErrorIs(t, err, inboxcontrol.ErrConflict)
	var flags string
	requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id = ? AND mailbox = 'Archive' AND uidvalidity = 30 AND uid = 50`), f.source.ID).Scan(&flags))
	assertions.Contains(flags, "Seen", "stale move cannot replace newer membership flags")
	state, err := f.store.GetInboxProviderState(t.Context(), after.Target)
	requirements.NoError(err)
	requirements.NotNil(state)
	assertions.Equal(newer, *state)
}

func TestInboxMoveReconcileIdenticalRepeatIsSafe(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, before, after := inboxIMAPMoveFixture(t)
	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after))
	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), before.Target, before, after))
	assertions.Equal(3, membershipCount(t, f.store, f.source.ID))
	assertions.Equal([]string{"Archive", "Watch"}, messageLabels(t, f.store, before.Target.ItemID))
}

func TestInboxGmailReconcileUpdatesLabelsAndKeepsUIRead(t *testing.T) {
	for _, kind := range []string{"gmail", ""} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st, source, id := tagFixture(t, kind, "gmail-reconcile")
			native, err := st.EmailTagTargetContext(t.Context(), id, "")
			requirements.NoError(err)
			requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.Result{Provider: "gmail", Tags: []string{"INBOX", "UNREAD", "LabelNext", "LabelOther"}, AvailableTags: []emailtags.Tag{{ID: "LabelNext", Name: "Next"}, {ID: "LabelOther", Name: "Other"}}, Verified: true}))
			requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "Synthetic retained body", Valid: true}, sql.NullString{}))
			_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read=FALSE WHERE id=?`), id)
			requirements.NoError(err)
			target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "gmail-reconcile"}
			before := inboxcontrol.State{Target: target, Tags: []string{"INBOX", "UNREAD", "LabelNext", "LabelOther"}, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()}
			after := before
			after.Tags = []string{"UNREAD", "LabelNext", "LabelOther"}
			after.Inbox = new(false)
			after.ObservedAt = before.ObservedAt.Add(time.Second)
			requirements.NoError(st.ReconcileInboxProviderState(t.Context(), target, before, after))
			assertions.Equal([]string{"Next", "Other", "UNREAD"}, messageLabels(t, st, id))
			observed, err := st.GetInboxProviderState(t.Context(), target)
			requirements.NoError(err)
			requirements.NotNil(observed)
			assertions.Equal(after, *observed)
			read := after
			read.Tags = []string{"LabelNext", "LabelOther"}
			read.Read = new(true)
			read.ObservedAt = after.ObservedAt.Add(time.Second)
			requirements.NoError(st.ReconcileInboxProviderState(t.Context(), target, after, read))
			assertions.Equal([]string{"Next", "Other"}, messageLabels(t, st, id))
			var uiRead bool
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), id).Scan(&uiRead))
			assertions.False(uiRead)
			body, err := st.GetMessageBodyText(id)
			requirements.NoError(err)
			assertions.Equal("Synthetic retained body", body)
			assertions.False(messageTombstoned(t, st, id))
			// An older receipt cannot restore old labels over a newer verified observation.
			require.ErrorIs(t, st.ReconcileInboxProviderState(t.Context(), target, before, after), inboxcontrol.ErrConflict)
			assertions.Equal([]string{"Next", "Other"}, messageLabels(t, st, id))
		})
	}
}

func TestInboxGmailReconcileUnavailableLocalLabelRollsBack(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, id := tagFixture(t, "gmail", "gmail-rollback")
	native, err := st.EmailTagTargetContext(t.Context(), id, "")
	requirements.NoError(err)
	requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.Result{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "gmail-rollback"}
	before := inboxcontrol.State{Target: target, Tags: []string{"INBOX", "UNREAD"}, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()}
	_, err = st.ObserveInboxState(t.Context(), before)
	requirements.NoError(err)
	after := before
	after.Tags = []string{"UNREAD", "RemoteLabelMissingLocally"}
	after.Inbox = new(false)
	after.ObservedAt = before.ObservedAt.Add(time.Second)
	requirements.Error(st.ReconcileInboxProviderState(t.Context(), target, before, after))
	assertions.Equal([]string{"INBOX", "UNREAD"}, messageLabels(t, st, id))
	observed, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(observed)
	assertions.Equal(before, *observed)
}

func TestInboxGmailUnknownTagsDoNotEraseLabels(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, id := tagFixture(t, "gmail", "gmail-unknown")
	native, err := st.EmailTagTargetContext(t.Context(), id, "")
	requirements.NoError(err)
	requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.Result{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: id, ProviderID: "gmail-unknown"}
	before := inboxcontrol.State{Target: target, Tags: []string{"INBOX", "UNREAD"}, Inbox: new(true), Read: new(false), ObservedAt: time.Now().UTC()}
	after := inboxcontrol.State{Target: target, ObservedAt: before.ObservedAt.Add(time.Second)}
	requirements.NoError(st.ReconcileInboxProviderState(t.Context(), target, before, after))
	assertions.Equal([]string{"INBOX", "UNREAD"}, messageLabels(t, st, id))
	observed, err := st.GetInboxProviderState(t.Context(), target)
	requirements.NoError(err)
	requirements.NotNil(observed)
	assertions.Nil(observed.Inbox)
	assertions.Nil(observed.Read)
}

func TestInboxFolderReconcileDoesNotAdoptAnotherNativeID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, id := tagFixture(t, "gmail", "folder-native-conflict")
	oldID, err := st.EnsureLabel(source.ID, "LabelOld", "Review", "user")
	requirements.NoError(err)
	requirements.NoError(st.AddMessageLabels(id, []int64{oldID}))
	binding := inboxcontrol.SourceIdentity{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier}
	before := inboxcontrol.State{Source: binding, ObservedAt: time.Now().UTC()}
	folder := inboxcontrol.Folder{ID: "LabelNew", Name: "Review"}
	after := inboxcontrol.State{Source: binding, Folders: []inboxcontrol.Folder{folder}, ProvisionedFolder: &folder, ObservedAt: before.ObservedAt.Add(time.Second)}
	require.ErrorIs(t, st.ReconcileInboxProviderState(t.Context(), inboxcontrol.Target{}, before, after), inboxcontrol.ErrConflict)
	var got string
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT source_label_id FROM labels WHERE id = ?"), oldID).Scan(&got))
	assertions.Equal("LabelOld", got)
	assertions.Equal([]string{"Review"}, messageLabels(t, st, id))
}

func TestInboxFolderReconcileDoesNotAdoptUnknownNativeID(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, source, id := tagFixture(t, "gmail", "folder-unknown-native")
	oldID, err := st.EnsureLabel(source.ID, "LabelOld", "Review", "user")
	requirements.NoError(err)
	requirements.NoError(st.AddMessageLabels(id, []int64{oldID}))
	_, err = st.DB().Exec(st.Rebind("UPDATE labels SET source_label_id = NULL WHERE id = ?"), oldID)
	requirements.NoError(err)
	binding := inboxcontrol.SourceIdentity{SourceID: source.ID, SourceType: "gmail", SourceIdentifier: source.Identifier, AccountID: source.Identifier}
	before := inboxcontrol.State{Source: binding, ObservedAt: time.Now().UTC()}
	folder := inboxcontrol.Folder{ID: "LabelNew", Name: "Review"}
	after := inboxcontrol.State{Source: binding, Folders: []inboxcontrol.Folder{folder}, ProvisionedFolder: &folder, ObservedAt: before.ObservedAt.Add(time.Second)}
	require.ErrorIs(t, st.ReconcileInboxProviderState(t.Context(), inboxcontrol.Target{}, before, after), inboxcontrol.ErrConflict)
	var known bool
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT source_label_id IS NOT NULL FROM labels WHERE id = ?"), oldID).Scan(&known))
	assertions.False(known)
	assertions.Equal([]string{"Review"}, messageLabels(t, st, id))
}

func TestInboxIMAPFolderReconcileRecordsEpochWithoutTouchingMemberships(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	f, beforeItem, _ := inboxIMAPMoveFixture(t)
	source := inboxcontrol.SourceIdentity{SourceID: f.source.ID, SourceType: "imap", SourceIdentifier: f.source.Identifier, AccountID: "identity"}
	folder := inboxcontrol.Folder{ID: "Followups", Name: "Followups", UIDValidity: 50}
	before := inboxcontrol.State{Source: source, ObservedAt: time.Now().UTC()}
	after := inboxcontrol.State{Source: source, Folders: []inboxcontrol.Folder{folder}, ProvisionedFolder: &folder, ObservedAt: before.ObservedAt.Add(time.Second)}
	requirements.NoError(f.store.ReconcileInboxProviderState(t.Context(), inboxcontrol.Target{}, before, after))
	states, err := f.store.GetIMAPFolderStates(f.source.ID)
	requirements.NoError(err)
	assertions.Contains(states, store.IMAPFolderState{Mailbox: "Followups", UIDValidity: 50})
	assertions.Equal(3, membershipCount(t, f.store, f.source.ID))
	body, err := f.store.GetMessageBodyText(beforeItem.Target.ItemID)
	requirements.NoError(err)
	assertions.Equal("Synthetic body retained on move", body)
	// A name match must not overwrite an existing mailbox epoch or its messages.
	changed := after
	different := folder
	different.UIDValidity++
	changed.Folders = []inboxcontrol.Folder{different}
	changed.ProvisionedFolder = &different
	require.ErrorIs(t, f.store.ReconcileInboxProviderState(t.Context(), inboxcontrol.Target{}, before, changed), inboxcontrol.ErrConflict)
	states, err = f.store.GetIMAPFolderStates(f.source.ID)
	requirements.NoError(err)
	assertions.Contains(states, store.IMAPFolderState{Mailbox: "Followups", UIDValidity: 50})
}

func TestInboxIMAPFolderReconcileDoesNotAdoptLocalLabel(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "different native ID"
		if unknown {
			name = "unknown native ID"
		}
		t.Run(name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f, item, _ := inboxIMAPMoveFixture(t)
			old, err := f.store.EnsureLabel(f.source.ID, "old-native-id", "Followups", "user")
			requirements.NoError(err)
			requirements.NoError(f.store.AddMessageLabels(item.Target.ItemID, []int64{old}))
			if unknown {
				_, err = f.store.DB().Exec(f.store.Rebind("UPDATE labels SET source_label_id = NULL WHERE id = ?"), old)
				requirements.NoError(err)
			}
			source := inboxcontrol.SourceIdentity{SourceID: f.source.ID, SourceType: "imap", SourceIdentifier: f.source.Identifier, AccountID: "identity"}
			before := inboxcontrol.State{Source: source, ObservedAt: time.Now().UTC()}
			folder := inboxcontrol.Folder{ID: "Followups", Name: "Followups", UIDValidity: 50}
			after := inboxcontrol.State{Source: source, Folders: []inboxcontrol.Folder{folder}, ProvisionedFolder: &folder, ObservedAt: before.ObservedAt.Add(time.Second)}
			require.ErrorIs(t, f.store.ReconcileInboxProviderState(t.Context(), inboxcontrol.Target{}, before, after), inboxcontrol.ErrConflict)
			var native sql.NullString
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind("SELECT source_label_id FROM labels WHERE id = ?"), old).Scan(&native))
			if unknown {
				assertions.False(native.Valid)
			} else {
				assertions.Equal("old-native-id", native.String)
			}
			states, err := f.store.GetIMAPFolderStates(f.source.ID)
			requirements.NoError(err)
			assertions.NotContains(states, store.IMAPFolderState{Mailbox: "Followups", UIDValidity: 50})
			assertions.Contains(messageLabels(t, f.store, item.Target.ItemID), "Followups")
		})
	}
}
