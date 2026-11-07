package signalandroid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestReimportRecycledPhoneKeepsUnnamedFormerOwner(t *testing.T) {
	for _, tc := range []struct{ name, formerName, replacementName string }{
		{"named owner", "Alice Example", "Alice Example"},
		{"unnamed owner", "", "Replacement Example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			first, second := t.TempDir(), t.TempDir()
			signalfixture.Write(t, first, signalfixture.Current, 0)
			changeFixture(t, first, `DELETE FROM message WHERE _id <> 100`)
			if tc.formerName == "" {
				changeFixture(t, first, `UPDATE recipient SET system_joined_name = NULL, profile_joined_name = NULL WHERE _id = 2`)
			}
			st, attachments := testutil.NewTestStore(t), t.TempDir()
			runImport(t, st, first, attachments)
			original, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.AliceACI)
			require.NoError(err)
			require.NotZero(original)
			var name string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT COALESCE(display_name, '') FROM participants WHERE id = ?`), original).Scan(&name))
			require.Equal(tc.formerName, name)

			const newACI = "0f0f0f0f-0000-4000-8000-000000000008"
			signalfixture.Write(t, second, signalfixture.Current, 0)
			changeFixture(t, second,
				`DELETE FROM message WHERE _id <> 100`,
				`UPDATE recipient SET aci = '`+newACI+`', system_joined_name = '`+tc.replacementName+`', profile_joined_name = NULL WHERE _id = 2`,
				`UPDATE message SET date_sent = 1700000500000, date_received = 1700000500001 WHERE _id = 100`)
			runImport(t, st, second, attachments)

			var phoneOwner int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id, COALESCE(display_name, '') FROM participants WHERE phone_number = ?`), signalfixture.AlicePhone).Scan(&phoneOwner, &name))
			assert.Equal(original, phoneOwner)
			assert.Equal(tc.formerName, name, "the former owner keeps the name recorded before the number was recycled")
			replacement, hasPhone, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, newACI)
			require.NoError(err)
			require.NotZero(replacement)
			assert.NotEqual(original, replacement)
			assert.False(hasPhone)
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT display_name FROM participants WHERE id = ?`), replacement).Scan(&name))
			assert.Equal(tc.replacementName, name)
			for _, tc := range []struct {
				message string
				sender  int64
			}{
				{"signal:aci:" + signalfixture.AliceACI + ":peer:" + ms(signalfixture.TAlice1), original},
				{"signal:aci:" + newACI + ":peer:1700000500000", replacement},
			} {
				var sender int64
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT sender_id FROM messages WHERE source_message_id = ?`), tc.message).Scan(&sender))
				assert.Equal(tc.sender, sender)
			}
			assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM messages`))
			owner, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, signalfixture.AliceACI)
			require.NoError(err)
			assert.Equal(original, owner)
		})
	}
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
			require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT id FROM messages`).Scan(&archivedID))

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
		name, messageKey             string
		setup, beforeUpdate, updates []string
		messagesUpdated              int64
	}{
		{name: "number changes after the ACI is learned", messagesUpdated: 1, messageKey: "signal:pni:" + signalfixture.DanaPNI + ":self:1700000400000",
			beforeUpdate: []string{`UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`},
			updates:      []string{`UPDATE recipient SET e164 = '+12025550107', pni = '0f0f0f0f-0000-4000-8000-000000000007' WHERE _id = 7`}},
		{name: "phone-only number changes after the ACI is learned", messageKey: "signal:tel:" + signalfixture.DanaPhone + ":peer:1700000410000",
			setup: []string{`UPDATE recipient SET pni = NULL WHERE _id = 7`,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body) VALUES (601, 1700000410000, 1700000410001, 14, 7, 1, ` + ms(signalfixture.TypeIncoming) + `, 'hello owner')`},
			beforeUpdate: []string{`UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`},
			updates:      []string{`UPDATE recipient SET e164 = '+12025550107', pni = '0f0f0f0f-0000-4000-8000-000000000007' WHERE _id = 7`}, messagesUpdated: 2},
		{name: "merged into ACI recipient", messagesUpdated: 1, messageKey: "signal:pni:" + signalfixture.DanaPNI + ":self:1700000400000", updates: []string{
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
			changeFixture(t, dir, tc.setup...)
			st := testutil.NewTestStore(t)
			attachments := t.TempDir()
			runImport(t, st, dir, attachments)
			before := importedMessages(t, st)
			var messageID, participantID int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), tc.messageKey).Scan(&messageID))
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT id FROM participants WHERE phone_number = ?`), signalfixture.DanaPhone).Scan(&participantID))

			if len(tc.beforeUpdate) > 0 {
				changeFixture(t, dir, tc.beforeUpdate...)
				runImport(t, st, dir, attachments)
			}
			changeFixture(t, dir, tc.updates...)
			summary := runImport(t, st, dir, attachments)

			assert.Zero(summary.MessagesAdded)
			assert.Equal(tc.messagesUpdated, summary.MessagesUpdated)
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

func TestRecycledPhoneStartsNewChat(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	dir := t.TempDir()
	signalfixture.WritePNIOnly(t, dir, signalfixture.Current)
	changeFixture(t, dir, `UPDATE recipient SET pni = NULL WHERE _id = 7`)
	st, attachments := testutil.NewTestStore(t), t.TempDir()
	runImport(t, st, dir, attachments)
	oldChat := "signal:tel:" + signalfixture.DanaPhone

	changeFixture(t, dir, `UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`)
	runImport(t, st, dir, attachments)

	const recycledACI = "0f0f0f0f-0000-4000-8000-000000000008"
	changeFixture(t, dir,
		`UPDATE recipient SET e164 = '+12025550107' WHERE _id = 7`,
		`INSERT INTO recipient (_id, aci, e164) VALUES (8, '`+recycledACI+`', '`+signalfixture.DanaPhone+`')`,
		`INSERT INTO thread (_id, recipient_id) VALUES (15, 8)`,
		`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
		 VALUES (700, 1700000500000, 1700000500001, 15, 8, 1, `+ms(signalfixture.TypeIncoming)+`, 'hello from the new owner')`)
	runImport(t, st, dir, attachments)

	messages := importedMessages(t, st)
	newChat := "signal:aci:" + recycledACI
	assert.Equal(importedMessage{conversation: newChat, body: "hello from the new owner"}, messages[newChat+":peer:1700000500000"])
	assert.Equal(oldChat, messages[oldChat+":self:1700000400000"].conversation)
	assert.Equal(2, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
	require.Len(messages, 2)
}
