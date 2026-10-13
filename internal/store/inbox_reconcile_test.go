package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func TestInboxGmailReconcileUpdatesLabelsAndKeepsUIRead(t *testing.T) {
	for _, kind := range []string{"gmail", ""} {
		t.Run(kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st, source, id := tagFixture(t, kind, "gmail-reconcile")
			native, err := st.EmailTagTargetContext(t.Context(), id, "")
			requirements.NoError(err)
			requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD", "LabelNext", "LabelOther"}, AvailableTags: []emailtags.MessageTag{{ID: "LabelNext", Name: "Next"}, {ID: "LabelOther", Name: "Other"}}, Verified: true}))
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
	requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
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
	requirements.NoError(st.SaveEmailTagsContext(t.Context(), native, &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD"}, Verified: true}))
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
