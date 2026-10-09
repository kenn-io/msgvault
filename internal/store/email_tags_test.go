package store_test

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func tagFixture(t *testing.T, provider, id string) (*store.Store, *store.Source, int64) {
	t.Helper()
	require := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(provider, "owner@example.test")
	require.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread-1", "Tags fixture")
	require.NoError(err)
	mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, SourceMessageID: id, ConversationID: conv, MessageType: "email", RFC822MessageID: sql.NullString{String: "duplicate@example.test", Valid: true}})
	require.NoError(err)
	return st, source, mid
}
func tagMembership(t *testing.T, st *store.Store, source, id int64, mailbox string, epoch, uid uint32) {
	t.Helper()
	require := require.New(t)
	_, err := st.DB().Exec(st.Rebind(`INSERT INTO imap_folder_state (source_id,mailbox,uidvalidity,uidnext) VALUES (?,?,?,?)`), source, mailbox, epoch, uid+1)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO imap_message_memberships (source_id,mailbox,uidvalidity,uid,message_id,flags) VALUES (?,?,?,?,?,?)`), source, mailbox, epoch, uid, id, `["Old"]`)
	require.NoError(err)
}
func TestEmailTagsIMAPMembershipIdentityAndSave(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, id := tagFixture(t, "imap", "INBOX|7")
	_, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.Error(err, "legacy identity must sync first")
	tagMembership(t, st, source.ID, id, "INBOX", 77, 7)
	tagMembership(t, st, source.ID, id, "Archive", 88, 9)
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	assert.Equal("INBOX", target.Mailbox)
	assert.Equal(uint32(77), target.UIDValidity)
	alternate, err := st.EmailTagTargetContext(t.Context(), id, "Archive")
	require.NoError(err)
	assert.Equal(uint32(9), alternate.UID)
	_, err = st.EmailTagTargetContext(t.Context(), id, "Missing")
	require.Error(err)
	result := &emailtags.MessageTagResult{Provider: "imap", Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID, Flags: []string{"Next", "\\Seen", "Unrelated"}, Tags: []string{"Next", "Unrelated"}, Verified: true}
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, result))
	var flags string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox=?`), source.ID, "INBOX").Scan(&flags))
	var saved []string
	require.NoError(json.Unmarshal([]byte(flags), &saved))
	assert.ElementsMatch(result.Flags, saved)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox=?`), source.ID, "Archive").Scan(&flags))
	assert.Equal(`["Old"]`, flags)
	// A mapping retired or moved during a remote operation must not be updated.
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "Archive|9", id)
	require.NoError(err)
	require.Error(st.SaveEmailTagsContext(t.Context(), target, result))
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "INBOX|7", id)
	require.NoError(err)
	_, err = st.DB().Exec(st.Rebind(`UPDATE imap_folder_state SET uidvalidity=? WHERE source_id=? AND mailbox=?`), 78, source.ID, "INBOX")
	require.NoError(err)
	require.Error(st.SaveEmailTagsContext(t.Context(), target, result), "retired epoch cannot save")
	_, err = st.EmailTagTargetContext(t.Context(), id, "INBOX")
	require.Error(err, "explicit mailbox cannot select a retired epoch")
	surviving, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err, "sole current copy survives the original epoch")
	assert.Equal("Archive", surviving.Mailbox)
	assert.Equal(uint32(88), surviving.UIDValidity)
	assert.Equal(uint32(9), surviving.UID)
	result.Mailbox, result.UIDValidity, result.UID = surviving.Mailbox, surviving.UIDValidity, surviving.UID
	require.NoError(st.SaveEmailTagsContext(t.Context(), surviving, result))
	tagMembership(t, st, source.ID, id, "Work", 99, 10)
	_, err = st.EmailTagTargetContext(t.Context(), id, "")
	require.Error(err, "multiple surviving copies require a mailbox")
	selected, err := st.EmailTagTargetContext(t.Context(), id, "Work")
	require.NoError(err)
	assert.Equal("Work", selected.Mailbox)
}

func TestEmailTagsIMAPMailboxSelection(t *testing.T) {
	for _, tc := range []struct {
		name, sourceKey, mailbox string
		uids                     []uint32
		wantUID                  uint32
	}{
		{"original before alias", "INBOX|7", "INBOX", []uint32{7, 9}, 7},
		{"original after alias", "INBOX|9", "INBOX", []uint32{7, 9}, 9},
		{"original without mailbox", "INBOX|7", "", []uint32{7, 9}, 7},
		{"ambiguous requested mailbox", "Archive|3", "INBOX", []uint32{7, 9}, 0},
		{"unique requested mailbox", "Archive|3", "INBOX", []uint32{7}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, source, id := tagFixture(t, "imap", tc.sourceKey)
			inbox := store.IMAPMailboxDelta{
				Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 10},
			}
			for _, uid := range tc.uids {
				inbox.Memberships = append(inbox.Memberships, store.IMAPMembershipObservation{UID: uid, CanonicalSourceMessageID: tc.sourceKey})
			}
			require.NoError(st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{
				inbox,
				{Mailbox: "Archive", State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 88, UIDNext: 4},
					Memberships: []store.IMAPMembershipObservation{{UID: 3, CanonicalSourceMessageID: tc.sourceKey}}},
			}))
			target, err := st.EmailTagTargetContext(t.Context(), id, tc.mailbox)
			if tc.wantUID == 0 {
				var failure *emailtags.MessageTagError
				require.ErrorAs(err, &failure)
				assert.Equal("stale_identity", failure.Code)
				return
			}
			require.NoError(err)
			assert.Equal("INBOX", target.Mailbox)
			assert.Equal(uint32(77), target.UIDValidity)
			assert.Equal(tc.wantUID, target.UID)
		})
	}
}

func TestEmailTagsIMAPDraftReadbackRefreshesReceivedSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "imap", attrSink)
	f.confirm(attrSink)
	id := f.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", sourceMsgKey: "INBOX|7"})
	delta := store.IMAPMailboxDelta{
		Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 8},
		Memberships: []store.IMAPMembershipObservation{{UID: 7, SourceMessageID: "INBOX|7", Flags: []string{"Old"}}},
	}
	require.NoError(f.st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)
	target, err := f.st.EmailTagTargetContext(t.Context(), id, "INBOX")
	require.NoError(err)
	result := &emailtags.MessageTagResult{
		Provider: "imap", Mailbox: "INBOX", UIDValidity: 77, UID: 7,
		Flags: []string{"Next", "\\Draft"}, Tags: []string{"Next"}, Verified: true,
	}
	require.NoError(f.st.SaveEmailTagsContext(t.Context(), target, result))
	_, path := attribution(t, f.st, id)
	assert.Equal("sent", path.String)
	var draftAuthored bool
	require.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT draft_authored FROM messages WHERE id = ?`), id).Scan(&draftAuthored))
	assert.True(draftAuthored)
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), id)

	delta.Reset = true
	delta.Memberships[0].Flags = []string{"Next", "\\Draft"}
	require.NoError(f.st.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{delta}))
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), id)
}

func TestEmailTagsGmailSnapshotGuardAndRollback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _, id := tagFixture(t, "gmail", "gmail-1")
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	_, err = st.EmailTagTargetContext(t.Context(), id, "INBOX")
	require.Error(err)
	result := &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"INBOX", "UNREAD", "Label_new", "Label_other"}, AvailableTags: []emailtags.MessageTag{{ID: "Label_new", Name: "Next"}, {ID: "Label_other", Name: "Other"}}, Verified: true}
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, result))
	msg, err := st.GetMessage(id)
	require.NoError(err)
	assert.ElementsMatch([]string{"INBOX", "UNREAD", "Next", "Other"}, msg.Labels)
	result.Tags = []string{"Label_new"}
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, result))
	msg, err = st.GetMessage(id)
	require.NoError(err)
	assert.Equal([]string{"Next"}, msg.Labels)
	// Unknown provider labels cannot silently remove the known local labels.
	result.Tags = []string{"Label_unknown"}
	require.Error(st.SaveEmailTagsContext(t.Context(), target, result))
	msg, err = st.GetMessage(id)
	require.NoError(err)
	assert.Equal([]string{"Next"}, msg.Labels)
}

func TestEmailTagsGmailRefreshesReceivedSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "gmail", attrSink)
	f.confirm(attrSink)
	labels, err := f.st.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{
		"INBOX": {Name: "INBOX", Type: "system"},
		"SENT":  {Name: "SENT", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	require.NoError(err)
	id := f.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", labels: []int64{labels["INBOX"]}})
	target, err := f.st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)

	result := &emailtags.MessageTagResult{Provider: "gmail", Tags: []string{"SENT"}, Verified: true}
	require.NoError(f.st.SaveEmailTagsContext(t.Context(), target, result))
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), id)
	_, path := attribution(t, f.st, id)
	assert.Equal("sent", path.String)

	result.Tags = []string{"INBOX"}
	require.NoError(f.st.SaveEmailTagsContext(t.Context(), target, result))
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)
}

func TestMicrosoftMailLabelsRefreshReceivedSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newAttrFixture(t, "msmail", attrSink)
	f.confirm(attrSink)
	folders := map[string]store.LabelInfo{
		"inbox": {Name: "Inbox", Type: "system"},
		"sent":  {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	}
	labels, err := f.st.EnsureMicrosoftMailFoldersContext(t.Context(), f.source.ID, folders)
	require.NoError(err)
	id := f.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", labels: []int64{labels["inbox"]}})
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)

	sent := labels["sent"]
	changed, err := f.st.ReconcileMicrosoftMailLabelsContext(t.Context(), id, &sent, nil)
	require.NoError(err)
	assert.True(changed)
	assert.NotContains(searchIDs(t, f.st, "received:"+attrSink), id)
	_, path := attribution(t, f.st, id)
	assert.Equal("sent", path.String)

	folders["sent"] = store.LabelInfo{Name: "Archive", Type: "user"}
	_, err = f.st.EnsureMicrosoftMailFoldersContext(t.Context(), f.source.ID, folders)
	require.NoError(err)
	assert.Contains(searchIDs(t, f.st, "received:"+attrSink), id)
}
