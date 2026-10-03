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
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET is_read = ? WHERE id = ?`), false, id)
	require.NoError(err)
	result := &emailtags.Result{Provider: "imap", Mailbox: target.Mailbox, UIDValidity: target.UIDValidity, UID: target.UID, Flags: []string{"Next", "\\Seen", "Unrelated"}, Tags: []string{"Next", "Unrelated"}, Verified: true}
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, result))
	var flags string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox=?`), source.ID, "INBOX").Scan(&flags))
	var saved []string
	require.NoError(json.Unmarshal([]byte(flags), &saved))
	assert.ElementsMatch(result.Flags, saved)
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT flags FROM imap_message_memberships WHERE source_id=? AND mailbox=?`), source.ID, "Archive").Scan(&flags))
	assert.Equal(`["Old"]`, flags)
	var read bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_read FROM messages WHERE id=?`), id).Scan(&read))
	assert.False(read)
	// A mapping retired or moved during a remote operation must not be updated.
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET source_message_id=? WHERE id=?`), "Archive|9", id)
	require.NoError(err)
	require.Error(st.SaveEmailTagsContext(t.Context(), target, result))
}
func TestEmailTagsGmailSnapshotGuardAndRollback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, id := tagFixture(t, "gmail", "gmail-1")
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	_, err = st.EmailTagTargetContext(t.Context(), id, "INBOX")
	require.Error(err)
	result := &emailtags.Result{Provider: "gmail", Tags: []string{"INBOX", "UNREAD", "Label_new", "Label_other"}, AvailableTags: []emailtags.Tag{{ID: "Label_new", Name: "Next"}, {ID: "Label_other", Name: "Other"}}, Verified: true}
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
	forged := target
	forged.SourceID = source.ID + 100
	require.Error(st.SaveEmailTagsContext(t.Context(), forged, result))
	result.Verified = false
	require.Error(st.SaveEmailTagsContext(t.Context(), target, result))
}

func TestEmailTagsMicrosoftCategoriesPreserveFolder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, id := tagFixture(t, "msmail", "immutable-1")
	folder, err := st.EnsureLabel(source.ID, "folder-1", "Inbox", "system")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(id, []int64{folder}, true)
	require.NoError(err)
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	result := &emailtags.Result{Provider: "msmail", Tags: []string{"Next", "Other"}, Verified: true}
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, result))
	msg, err := st.GetMessage(id)
	require.NoError(err)
	assert.ElementsMatch([]string{"Inbox", "Category: Next", "Category: Other"}, msg.Labels)
	result.Tags = []string{}
	require.NoError(st.SaveEmailTagsContext(t.Context(), target, result))
	msg, err = st.GetMessage(id)
	require.NoError(err)
	assert.Equal([]string{"Inbox"}, msg.Labels)
	_, err = st.EmailTagTargetContext(t.Context(), id, "INBOX")
	require.Error(err)
}

func TestEmailTagsMicrosoftCategoryNameCollisionPreservesFolder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, source, id := tagFixture(t, "msmail", "immutable-1")
	folder, err := st.EnsureLabel(source.ID, "folder-1", "Category: Next", "system")
	require.NoError(err)
	_, err = st.ReconcileMessageLabels(id, []int64{folder}, true)
	require.NoError(err)
	target, err := st.EmailTagTargetContext(t.Context(), id, "")
	require.NoError(err)
	result := &emailtags.Result{Provider: "msmail", Tags: []string{"Next"}, Verified: true}
	require.Error(st.SaveEmailTagsContext(t.Context(), target, result))
	msg, err := st.GetMessage(id)
	require.NoError(err)
	assert.Equal([]string{"Category: Next"}, msg.Labels)
	var providerID, kind string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT source_label_id,label_type FROM labels WHERE id=?`), folder).Scan(&providerID, &kind))
	assert.Equal("folder-1", providerID)
	assert.Equal("system", kind)
}
