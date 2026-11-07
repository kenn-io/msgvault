package signalandroid

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

// changeFixture applies statements to a fixture database, as a newer backup
// of the same account would differ.
func changeFixture(t *testing.T, dir string, stmts ...string) {
	t.Helper()
	require := require.New(t)
	db, err := sql.Open("sqlite3", filepath.Join(dir, "database.sqlite"))
	require.NoError(err)
	defer func() { require.NoError(db.Close()) }()
	for _, stmt := range stmts {
		_, err := db.ExecContext(t.Context(), stmt)
		require.NoError(err, stmt)
	}
}

func countRows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(query), args...).Scan(&n))
	return n
}

func TestRerunReplacesReactions(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())

	// Alice changes her reaction on the group message and withdraws the one
	// on the edited draft.
	changeFixture(t, dir,
		`UPDATE reaction SET emoji = '`+"\U0001F602"+`' WHERE message_id = 200`,
		`DELETE FROM reaction WHERE message_id = 106`)
	summary := runImport(t, st, dir, t.TempDir())

	assert.Equal(int64(1), summary.ReactionsAdded)
	reactionsOf := func(id string) []string {
		rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
			SELECT r.reaction_value FROM reactions r JOIN messages m ON m.id = r.message_id
			WHERE m.source_message_id = ? ORDER BY r.reaction_value`), id)
		require.NoError(t, err)
		defer func() { require.NoError(t, rows.Close()) }()
		var values []string
		for rows.Next() {
			var v string
			require.NoError(t, rows.Scan(&v))
			values = append(values, v)
		}
		require.NoError(t, rows.Err())
		return values
	}
	assert.Equal([]string{"\U0001F602"}, reactionsOf("signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:"+signalfixture.BobACI+":"+ms(signalfixture.TGroup)))
	assert.Empty(reactionsOf("signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:" + ms(signalfixture.TEditOld)))
}

func TestRerunClearsEmptiedBody(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())

	changeFixture(t, dir, `UPDATE message SET body = NULL WHERE _id = 300`)
	runImport(t, st, dir, t.TempDir())

	got := importedMessages(t, st)
	assert.Empty(got["signal:aci:0f0f0f0f-0000-4000-8000-000000000004:peer:"+ms(signalfixture.TCarol)].body)
	assert.Equal(0, countRows(t, st, `
		SELECT COUNT(*) FROM messages m WHERE m.source_message_id = ? AND m.snippet IS NOT NULL`,
		"signal:aci:0f0f0f0f-0000-4000-8000-000000000004:peer:"+ms(signalfixture.TCarol)))
}

func TestImportVerifiesOwnerFromBackupEvidence(t *testing.T) {
	const dropOutgoing = `DELETE FROM message WHERE (type & 31) IN (21, 22, 23, 24)`
	for _, tc := range []struct {
		name, phone, wantError string
		changes                []string
		added                  int64
		checkNoSource          bool
		layout                 signalfixture.Layout
	}{
		{name: "full backup of another number", phone: "+15555550199", wantError: "--phone does not match"},
		{name: "incoming-only backup of another number", phone: "+15555550199", changes: []string{dropOutgoing}, wantError: "--phone does not match", checkNoSource: true},
		{name: "incoming-only backup with formatted owner number", phone: "+1 555 555 0100", changes: []string{dropOutgoing}, added: 5},
		{name: "migrated incoming-only backup", phone: signalfixture.OwnPhone, layout: signalfixture.V185, changes: []string{dropOutgoing, `UPDATE message SET to_recipient_id = from_recipient_id WHERE thread_id IN (10, 12)`}, added: 5},
		{name: "group-only backup", phone: signalfixture.OwnPhone, changes: []string{dropOutgoing, `DELETE FROM message WHERE thread_id <> 11`}, added: 1},
		{name: "owner number not recorded", phone: signalfixture.OwnPhone, changes: []string{`UPDATE recipient SET e164 = NULL WHERE _id = 1`}, wantError: "cannot verify --phone"},
		{name: "owner account ID not recorded", phone: signalfixture.OwnPhone, changes: []string{`UPDATE recipient SET aci = NULL WHERE _id = 1`}, added: 7},
		{name: "owner absent from empty backup", phone: "+15555550199", changes: []string{`DELETE FROM message`}, wantError: "cannot find your Signal account for --phone", checkNoSource: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := t.TempDir()
			layout := tc.layout
			if layout == (signalfixture.Layout{}) {
				layout = signalfixture.Current
			}
			path := signalfixture.Write(t, dir, layout, 0)
			changeFixture(t, dir, tc.changes...)
			opts := DefaultOptions()
			opts.Phone = tc.phone
			st := testutil.NewTestStore(t)
			summary, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			if tc.wantError != "" {
				require.Error(err)
				assert.Contains(err.Error(), tc.wantError)
			} else {
				require.NoError(err)
				assert.Equal(tc.added, summary.MessagesAdded)
				source, err := st.GetSourceByID(summary.SourceID)
				require.NoError(err)
				assert.Equal(signalfixture.OwnPhone, source.Identifier)
			}
			if tc.checkNoSource {
				assert.Equal(0, countRows(t, st, `SELECT COUNT(*) FROM sources WHERE source_type = ?`, SourceType))
			}
		})
	}
}

func TestImportSendingSkipped(t *testing.T) {
	for _, tc := range []struct {
		name, phone string
		wantError   bool
	}{
		{name: "owner", phone: signalfixture.OwnPhone},
		{name: "peer", phone: signalfixture.AlicePhone, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			path := signalfixture.Write(t, dir, signalfixture.Current, 0)
			// Type 28 still supplies owner evidence and outgoing authorship.
			changeFixture(t, dir, `DELETE FROM message WHERE _id <> 101`,
				`UPDATE message SET type = `+ms(28|pushMessageBit|secureMessageBit))
			opts := DefaultOptions()
			opts.Phone, opts.AttachmentsDir = tc.phone, t.TempDir()
			_, err := NewImporter(st, nil).Import(t.Context(), path, opts)
			if tc.wantError {
				require.ErrorContains(err, "--phone does not match")
				assert.Zero(countRows(t, st, `SELECT COUNT(*) FROM sources WHERE source_type = ?`, SourceType))
				return
			}
			require.NoError(err)
			id := "signal:aci:" + signalfixture.AliceACI + ":self:" + ms(signalfixture.TReply)
			assert.Equal(map[string]importedMessage{id: {
				conversation: "signal:aci:" + signalfixture.AliceACI, body: "Replying", fromMe: true,
			}}, importedMessages(t, st))
		})
	}
}

func TestRerunRecordsUpdatedMessagesOnSyncRun(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	runImport(t, st, dir, t.TempDir())
	changeFixture(t, dir, `UPDATE message SET body = 'Carol again' WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())

	var processed, added, updated int64
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT messages_processed, messages_added, messages_updated FROM sync_runs
		WHERE source_id = ? ORDER BY id DESC LIMIT 1`), summary.SourceID).Scan(&processed, &added, &updated))
	assert.Equal(summary.MessagesProcessed, processed)
	assert.Equal(int64(0), added)
	assert.Equal(summary.MessagesUpdated, updated)
}

func TestSendStateFollowsSignal(t *testing.T) {
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	failed := 24 | pushMessageBit | secureMessageBit
	changeFixture(t, dir, `UPDATE message SET type = `+ms(failed)+` WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())

	isSent := func() bool {
		var sent bool
		require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT is_sent FROM messages WHERE source_message_id = ?`),
			"signal:aci:0f0f0f0f-0000-4000-8000-000000000002:self:"+ms(signalfixture.TReply)).Scan(&sent))
		return sent
	}
	assert.False(isSent(), "a failed send did not leave the phone")

	changeFixture(t, dir, `UPDATE message SET type = `+ms(28|pushMessageBit|secureMessageBit)+` WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())
	assert.False(isSent(), "Signal skipped sending this message")

	changeFixture(t, dir, `UPDATE message SET type = `+ms(signalfixture.TypeOutgoing)+` WHERE _id = 101`)
	runImport(t, st, dir, t.TempDir())
	assert.True(isSent(), "a later backup shows it sent")
}

func TestReviewEditedLinkPreviewRemoved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change string
		want   int
	}{
		{"removed", `DELETE FROM attachment WHERE _id = 500`, 0},
		{"unchanged", `UPDATE attachment SET message_id = 107 WHERE _id = 500`, 1},
		{"missing source file", `UPDATE attachment SET _id = 503, message_id = 107 WHERE _id = 500`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir,
				`ALTER TABLE message ADD COLUMN link_previews TEXT`,
				`UPDATE attachment SET message_id = 106, caption = NULL WHERE _id = 500`,
				`UPDATE message SET link_previews = '[{"attachmentId":{"rowId":500}}]' WHERE _id = 106`)
			runImport(t, st, dir, storage)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_id FROM attachments`).Scan(&messageID))
			before, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(err)
			require.Len(before, 1)
			require.Equal(store.AttachmentRolePreview, before[0].Role)

			changeFixture(t, dir,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, original_message_id, revision_number)
				 SELECT 107, date_sent + 1000, date_received + 1000, thread_id, from_recipient_id, to_recipient_id, type, 'Edited without preview', 102, 2 FROM message WHERE _id = 106`,
				`UPDATE message SET latest_revision_id = 107 WHERE _id IN (102, 106)`,
				tc.change)
			if tc.name == "unchanged" {
				changeFixture(t, dir, `UPDATE message SET link_previews = '[{"attachmentId":{"rowId":500}}]' WHERE _id = 107`)
			}
			runImport(t, st, dir, storage)

			edited := importedMessages(t, st)["signal:aci:"+signalfixture.AliceACI+":self:"+ms(signalfixture.TEditOld)]
			assert.Equal("Edited without preview", edited.body)
			assert.True(edited.edited)
			after, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(err)
			assert.Len(after, tc.want)
			if tc.want == 1 {
				assert.Equal(before, after)
			}
			var count int
			var hasAttachments bool
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT attachment_count, has_attachments FROM messages WHERE id = ?`), messageID).Scan(&count, &hasAttachments))
			assert.Equal(tc.want, count)
			assert.Equal(tc.want > 0, hasAttachments)
		})
	}
}

func TestImportAttachmentReferenceRoles(t *testing.T) {
	for _, tc := range []struct {
		name, column, reference string
		role                    store.AttachmentRole
	}{
		{"link preview", "link_previews", `[{"url":"https://example.com/","attachmentId":{"rowId":500}}]`, store.AttachmentRolePreview},
		{"contact avatar", "shared_contacts", `[{"avatar":{"attachmentId":{"rowId":500},"isProfile":false}}]`, store.AttachmentRoleAvatar},
		{"unrelated preview", "link_previews", `[{"attachmentId":{"rowId":999}}]`, store.AttachmentRoleStandalone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `ALTER TABLE message ADD COLUMN `+tc.column+` TEXT`,
				`UPDATE message SET `+tc.column+` = '`+tc.reference+`' WHERE _id = 200`)
			runImport(t, st, dir, storage)
			var messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT message_id FROM attachments`).Scan(&messageID))
			refs, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(err)
			require.Len(refs, 1)
			assert.Equal(tc.role, refs[0].Role)

			if tc.role != store.AttachmentRoleStandalone {
				// A later backup drops the preview or contact reference.
				changeFixture(t, dir, `UPDATE message SET `+tc.column+` = NULL WHERE _id = 200`)
				runImport(t, st, dir, storage)
				refs, err = st.MessageMIMEAttachmentsContext(t.Context(), messageID)
				require.NoError(err)
				require.Len(refs, 1)
				assert.Equal(store.AttachmentRoleStandalone, refs[0].Role)
			}
		})
	}
}

func TestRerunRefreshesAttachmentMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, change, filename, mime, media string
		width, height                       int64
	}{
		{"filename", `file_name = 'photo.jpg'`, "photo.jpg", "image/jpeg", "image", 640, 480},
		{"MIME type", `content_type = 'image/png'`, "", "image/png", "image", 640, 480},
		{"width", `width = 1280`, "", "image/jpeg", "image", 1280, 480},
		{"height", `height = 960`, "", "image/jpeg", "image", 640, 960},
		{"media type", `sticker_pack_id = 'example-pack'`, "", "image/jpeg", "sticker", 640, 480},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir, storage := testutil.NewTestStore(t), t.TempDir(), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			runImport(t, st, dir, storage)
			var attachmentID, messageID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id, message_id FROM attachments`).Scan(&attachmentID, &messageID))
			before, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(err)
			require.Len(before, 1)

			changeFixture(t, dir, `UPDATE attachment SET `+tc.change+` WHERE _id = 500`)
			summary := runImport(t, st, dir, storage)
			assert.Zero(summary.AttachmentsStored)
			after, err := st.MessageMIMEAttachmentsContext(t.Context(), messageID)
			require.NoError(err)
			require.Len(after, 1)
			assert.Equal(tc.filename, after[0].Filename)
			assert.Equal(tc.mime, after[0].MimeType)
			assert.Equal(tc.media, after[0].MediaType)
			assert.Equal(tc.width, after[0].Width)
			assert.Equal(tc.height, after[0].Height)
			assert.Equal(before[0].SourcePartKey, after[0].SourcePartKey)
			assert.Equal(before[0].ContentHash, after[0].ContentHash)
			var updatedID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM attachments`).Scan(&updatedID))
			assert.Equal(attachmentID, updatedID)
			if tc.media == "sticker" {
				assert.Equal(store.AttachmentRoleSticker, after[0].Role)
				assert.Equal(store.AttachmentRoleSourceProviderExplicit, after[0].RoleSource)
			}

			summary = runImport(t, st, dir, storage)
			assert.Zero(summary.AttachmentsStored)
		})
	}
}

func TestRerunAttachmentOrderKeepsOccurrences(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	runImport(t, st, dir, storage)
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, file_name, display_order) VALUES (503, 200, 'image/png', 'other.png', -1)`)
	require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_503_-1.bin"), []byte("other photo"), 0o600))
	summary := runImport(t, st, dir, storage)
	assert.Equal(int64(1), summary.AttachmentsStored)
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE filename = 'other.png'`))
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	changeFixture(t, dir, `UPDATE attachment SET display_order = 1 WHERE _id = 503`)
	summary = runImport(t, st, dir, storage)
	assert.Zero(summary.AttachmentsStored)
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	// Identical files in two positions still represent two occurrences.
	changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type, display_order) VALUES (504, 200, 'image/jpeg', -2)`)
	require.NoError(os.WriteFile(filepath.Join(dir, "Attachment_504_-1.bin"), []byte("photo bytes"), 0o600))
	summary = runImport(t, st, dir, storage)
	// Both occurrences reuse the photo's stored bytes.
	assert.Zero(summary.AttachmentsStored)
	assert.Equal(3, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM attachments WHERE content_hash = '2a6927613ddc903379f3e02145f22074ccf048c7753cfa1ed8ee26c746682997'`))
	summary = runImport(t, st, dir, storage)
	assert.Zero(summary.AttachmentsStored)
	assert.Equal(3, countRows(t, st, `SELECT COUNT(*) FROM attachments`))
}

func TestUnresolvedRepliesCountOnlyThisRun(t *testing.T) {
	st := testutil.NewTestStore(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE message SET quote_id = 1700000290000, quote_author = 4 WHERE _id = 300`)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(t, int64(1), summary.RepliesUnresolved)
	changeFixture(t, dir, `INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
		VALUES (301, 1700000290000, 1700000290001, 12, 4, 1, `+ms(signalfixture.TypeIncoming)+`, 'Earlier message')`)
	summary = runImport(t, st, dir, t.TempDir())
	assert.Zero(t, summary.RepliesUnresolved)
	assert.Equal(t, "signal:aci:"+signalfixture.CarolACI+":peer:1700000290000", importedMessages(t, st)["signal:aci:"+signalfixture.CarolACI+":peer:"+ms(signalfixture.TCarol)].replyTo)
}

func TestSignalRerunPreservesArchiveLabels(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	first := runImport(t, st, dir, storage)
	labelID, err := st.EnsureLabel(t.Context(), first.SourceID, "kept", "Kept", "user")
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages ORDER BY id LIMIT 1`).Scan(&messageID))
	require.NoError(st.AddMessageLabels(t.Context(), messageID, []int64{labelID}))
	runImport(t, st, dir, storage)
	labels, err := st.MessageLabelIDsContext(t.Context(), messageID)
	require.NoError(err)
	assert.Equal([]int64{labelID}, labels)
}
