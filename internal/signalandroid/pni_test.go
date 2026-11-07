package signalandroid

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestReimportRecycledPhoneKeepsSendersSeparate(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `DELETE FROM message WHERE _id <> 100`)
	st, attachments := testutil.NewTestStore(t), t.TempDir()
	runImport(t, st, dir, attachments)
	original, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.AliceACI)
	require.NoError(err)
	require.NotZero(original)

	const newACI = "0f0f0f0f-0000-4000-8000-000000000008"
	changeFixture(t, dir,
		`UPDATE recipient SET aci = '`+newACI+`' WHERE _id = 2`,
		`UPDATE message SET date_sent = 1700000500000, date_received = 1700000500001 WHERE _id = 100`)
	runImport(t, st, dir, attachments)

	replacement, hasPhone, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, newACI)
	require.NoError(err)
	require.NotZero(replacement)
	assert.NotEqual(original, replacement)
	assert.False(hasPhone)
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM messages`))
	for _, tc := range []struct {
		message string
		sender  int64
	}{
		{"signal:aci:" + signalfixture.AliceACI + ":peer:" + ms(signalfixture.TAlice1), original},
		{"signal:aci:" + newACI + ":peer:1700000500000", replacement},
	} {
		var sender int64
		require.NoError(st.DB().QueryRow(st.Rebind(`SELECT sender_id FROM messages WHERE source_message_id = ?`), tc.message).Scan(&sender))
		assert.Equal(tc.sender, sender)
	}
	owner, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.AliceACI)
	require.NoError(err)
	assert.Equal(original, owner)
}

func TestImportDirectAuthorWithoutACI(t *testing.T) {
	for _, tc := range []struct {
		name, change, chatID string
	}{
		{"PNI only", `UPDATE recipient SET e164 = NULL WHERE _id = 7`, "signal:pni:0f0f0f0f-0000-4000-8000-000000000005"},
		{"phone only", `UPDATE recipient SET pni = NULL WHERE _id = 7`, "signal:tel:+15555550104"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			signalfixture.WritePNIOnly(t, dir, signalfixture.Current)
			changeFixture(t, dir, tc.change,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
				 VALUES (599, 1700000390000, 1700000390001, 14, 7, 1, `+ms(signalfixture.TypeIncoming)+`, 'hello owner')`,
				`UPDATE message SET quote_id = 1700000390000, quote_author = 7 WHERE _id = 600`)
			st, attachments := testutil.NewTestStore(t), t.TempDir()
			summary := runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesSkipped)
			assert.Equal(int64(2), summary.MessagesAdded)
			assert.Equal(int64(1), summary.RepliesLinked)
			before := importedMessages(t, st)
			assert.Equal(map[string]importedMessage{
				tc.chatID + ":peer:1700000390000": {
					conversation: tc.chatID, body: "hello owner",
				},
				tc.chatID + ":self:1700000400000": {
					conversation: tc.chatID, body: "hello Dana", fromMe: true,
					replyTo: tc.chatID + ":peer:1700000390000",
				},
			}, before)
			// A later ACI must preserve the direct reply's archived parent ID.
			changeFixture(t, dir, `UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`)
			summary = runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(int64(2), summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
		})
	}
}

func TestImportGroupAuthorWithoutACI(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE recipient SET aci = NULL WHERE _id = 3`, `DELETE FROM message WHERE _id <> 200`)
	st := testutil.NewTestStore(t)
	summary := runImport(t, st, dir, t.TempDir())
	assert.Equal(int64(1), summary.MessagesSkipped)
	assert.Empty(importedMessages(t, st))
}

func TestReimportGroupReplyAfterAuthorGainsACI(t *testing.T) {
	for _, tc := range []struct{ name, change, parentID string }{
		{"phone", `UPDATE recipient SET aci = NULL, pni = NULL, e164 = '+15555550102' WHERE _id = 3`, "signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:tel:+15555550102:1700000240000"},
		{"PNI", `UPDATE recipient SET aci = NULL, pni = '0f0f0f0f-0000-4000-8000-000000000005' WHERE _id = 3`, "signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:pni:0f0f0f0f-0000-4000-8000-000000000005:1700000240000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.change, `DELETE FROM message WHERE _id <> 200`)
			st, attachments := testutil.NewTestStore(t), t.TempDir()
			summary := runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesSkipped)
			before := importedMessages(t, st)
			require.Equal(map[string]importedMessage{
				tc.parentID: {conversation: "signal:group:" + signalfixture.GroupB64, body: "Hi @Alice Example, look\n\nBeach"},
			}, before)
			var archivedID int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&archivedID))

			changeFixture(t, dir,
				`UPDATE recipient SET aci = '`+signalfixture.BobACI+`' WHERE _id = 3`,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, quote_id, quote_author)
				 VALUES (202, 1700000241000, 1700000241001, 11, 1, 5, `+ms(signalfixture.TypeOutgoing)+`, 'Group reply', 1700000240000, 3)`)
			summary = runImport(t, st, dir, attachments)

			assert.Equal(int64(1), summary.MessagesAdded)
			assert.Zero(summary.RepliesUnresolved)
			preferredID := "signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:" + signalfixture.BobACI + ":1700000240000"
			assert.Equal(map[string]importedMessage{
				preferredID: before[tc.parentID],
				"signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:self:1700000241000": {
					conversation: "signal:group:" + signalfixture.GroupB64, body: "Group reply", fromMe: true, replyTo: preferredID,
				},
			}, importedMessages(t, st))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE id = ? AND source_message_id = ?`, archivedID, preferredID))

			before = importedMessages(t, st)
			changeFixture(t, dir, `UPDATE recipient SET e164 = '+15555550103' WHERE _id = 3`)
			summary = runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(int64(2), summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE id = ? AND source_message_id = ?`, archivedID, preferredID))
		})
	}
}

func TestReimportPNIChatGainsACI(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		beforeUpdate, updates []string
	}{
		{name: "number changes after the ACI is learned",
			beforeUpdate: []string{`UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`},
			updates:      []string{`UPDATE recipient SET e164 = '+12025550107', pni = '0f0f0f0f-0000-4000-8000-000000000007' WHERE _id = 7`}},
		{name: "merged into ACI recipient", updates: []string{
			`INSERT INTO recipient (_id, aci) VALUES (8, '0f0f0f0f-0000-4000-8000-000000000006')`,
			`UPDATE recipient SET pni = (SELECT pni FROM recipient WHERE _id = 7), e164 = (SELECT e164 FROM recipient WHERE _id = 7) WHERE _id = 8`,
			`INSERT INTO thread (_id, recipient_id) VALUES (15, 8)`,
			`UPDATE message SET thread_id = 15, to_recipient_id = 8 WHERE thread_id = 14`,
			`DELETE FROM thread WHERE _id = 14`,
			`DELETE FROM recipient WHERE _id = 7`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			dir := t.TempDir()
			signalfixture.WritePNIOnly(t, dir, signalfixture.Current)
			st := testutil.NewTestStore(t)
			attachments := t.TempDir()
			runImport(t, st, dir, attachments)
			before := importedMessages(t, st)
			var messageID, participantID int64
			require.NoError(st.DB().QueryRow(`SELECT id FROM messages`).Scan(&messageID))
			require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM participants WHERE phone_number = ?`), signalfixture.DanaPhone).Scan(&participantID))

			if len(tc.beforeUpdate) > 0 {
				changeFixture(t, dir, tc.beforeUpdate...)
				runImport(t, st, dir, attachments)
			}
			changeFixture(t, dir, tc.updates...)
			summary := runImport(t, st, dir, attachments)

			assert.Zero(summary.MessagesAdded)
			assert.Equal(int64(1), summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
			if len(tc.beforeUpdate) == 0 {
				pid, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, "0f0f0f0f-0000-4000-8000-000000000006")
				require.NoError(err)
				assert.Equal(participantID, pid)
			}
		})
	}
}

func TestReimportPhoneOnlyChatGainsACI(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	signalfixture.WritePNIOnly(t, dir, signalfixture.Current)
	changeFixture(t, dir, `UPDATE recipient SET pni = NULL WHERE _id = 7`,
		`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
		 VALUES (601, 1700000410000, 1700000410001, 14, 7, 1, `+ms(signalfixture.TypeIncoming)+`, 'hello owner')`)
	st, attachments := testutil.NewTestStore(t), t.TempDir()
	runImport(t, st, dir, attachments)
	chatID := "signal:tel:" + signalfixture.DanaPhone
	before := importedMessages(t, st)
	var messageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), chatID+":peer:1700000410000").Scan(&messageID))

	changeFixture(t, dir, `UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`)
	runImport(t, st, dir, attachments)

	changeFixture(t, dir, `UPDATE recipient SET e164 = '+12025550107', pni = '0f0f0f0f-0000-4000-8000-000000000007' WHERE _id = 7`)
	summary := runImport(t, st, dir, attachments)

	assert.Zero(summary.MessagesAdded)
	assert.Equal(int64(2), summary.MessagesUpdated)
	assert.Equal(before, importedMessages(t, st))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
}

func TestReimportPhoneOnlyChatGainsACIAndChangesNumber(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	changeFixture(t, dir, `UPDATE recipient SET aci = NULL, pni = NULL WHERE _id = 2`)
	st, attachments := testutil.NewTestStore(t), t.TempDir()
	first := runImport(t, st, dir, attachments)
	require.Equal(int64(7), first.MessagesAdded)
	before := importedMessages(t, st)
	require.Contains(before, "signal:tel:"+signalfixture.AlicePhone+":peer:"+ms(signalfixture.TAlice1))
	require.Equal(4, countRows(t, st, `SELECT COUNT(*) FROM conversations`))

	// Both changes arrive in one newer backup, with no intervening import.
	changeFixture(t, dir, `UPDATE recipient SET aci = '`+signalfixture.AliceACI+`', e164 = '+12025550107' WHERE _id = 2`)
	summary := runImport(t, st, dir, attachments)

	assert.Zero(summary.MessagesAdded)
	assert.Equal(int64(7), summary.MessagesUpdated)
	assert.Equal(before, importedMessages(t, st))
	assert.Equal(4, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
}

func TestReimportRenumberedChatMatchesIncomingAfterFirstBatch(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	signalfixture.WritePNIOnly(t, dir, signalfixture.Current)
	changeFixture(t, dir, `UPDATE recipient SET pni = NULL WHERE _id = 7`,
		`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
		 VALUES (601, 1700000410000, 1700000410001, 14, 7, 1, `+ms(signalfixture.TypeIncoming)+`, 'hello owner')`)
	st, attachments := testutil.NewTestStore(t), t.TempDir()
	first := runImport(t, st, dir, attachments)
	require.Equal(int64(2), first.MessagesAdded)
	before := importedMessages(t, st)
	var chatID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM conversations`).Scan(&chatID))

	changeFixture(t, dir, `UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006', e164 = '+12025550107' WHERE _id = 7`)
	opts := DefaultOptions()
	opts.Phone = signalfixture.OwnPhone
	opts.AttachmentFilesDir = dir
	opts.AttachmentsDir = attachments
	// Row 600 is outgoing; only the next batch contains the incoming match.
	opts.BatchSize = 1
	summary, err := NewImporter(st, nil).Import(t.Context(), filepath.Join(dir, "database.sqlite"), opts)
	require.NoError(err)

	assert.Zero(summary.MessagesAdded)
	assert.Equal(int64(2), summary.MessagesUpdated)
	assert.Equal(before, importedMessages(t, st))
	assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE conversation_id = ?`, chatID))
}
