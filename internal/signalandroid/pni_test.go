package signalandroid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestReimportPNIChatSurvivesNumberChange(t *testing.T) {
	for name, layout := range map[string]signalfixture.Layout{"current": signalfixture.Current, "v185": signalfixture.V185} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			signalfixture.WritePNIOnly(t, dir, layout)
			st := testutil.NewTestStore(t)
			attachments := t.TempDir()
			runImport(t, st, dir, attachments)
			before := importedMessages(t, st)

			aciColumn, phoneColumn := "aci", "e164"
			if name == "v185" {
				aciColumn, phoneColumn = "uuid", "phone"
			}
			changeFixture(t, dir, `UPDATE recipient SET `+aciColumn+` = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`)
			runImport(t, st, dir, attachments)
			changeFixture(t, dir, `UPDATE recipient SET `+phoneColumn+` = '+12025550107', pni = '0f0f0f0f-0000-4000-8000-000000000007' WHERE _id = 7`)
			summary := runImport(t, st, dir, attachments)

			assert.Zero(t, summary.MessagesAdded)
			assert.Equal(t, 1, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
			assert.Equal(t, before, importedMessages(t, st))
		})
	}
}

func TestImportPNIOnlyChat(t *testing.T) {
	for name, layout := range map[string]signalfixture.Layout{"current": signalfixture.Current, "v185": signalfixture.V185} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			signalfixture.WritePNIOnly(t, dir, layout)
			st := testutil.NewTestStore(t)

			summary := runImport(t, st, dir, t.TempDir())

			assert.Zero(t, summary.ChatsSkipped)
			assert.Equal(t, int64(1), summary.MessagesAdded)
			assert.Equal(t, map[string]importedMessage{
				"signal:pni:0f0f0f0f-0000-4000-8000-000000000005:self:1700000400000": {
					conversation: "signal:pni:0f0f0f0f-0000-4000-8000-000000000005", body: "hello Dana", fromMe: true,
				},
			}, importedMessages(t, st))
		})
	}
}

func TestImportDirectAuthorWithoutACI(t *testing.T) {
	for _, tc := range []struct {
		name, author, change string
	}{
		{"PNI only", "7", `UPDATE recipient SET e164 = NULL WHERE _id = 7`},
		{"phone only", "8", `INSERT INTO recipient (_id, e164) VALUES (8, '+15555550104')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			signalfixture.WritePNIOnly(t, dir, signalfixture.Current)
			changeFixture(t, dir, tc.change,
				`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body)
				 VALUES (599, 1700000390000, 1700000390001, 14, `+tc.author+`, 1, `+ms(signalfixture.TypeIncoming)+`, 'hello owner')`,
				`UPDATE message SET quote_id = 1700000390000, quote_author = `+tc.author+` WHERE _id = 600`)
			st, attachments := testutil.NewTestStore(t), t.TempDir()
			summary := runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesSkipped)
			assert.Equal(int64(2), summary.MessagesAdded)
			assert.Equal(int64(1), summary.RepliesLinked)
			before := importedMessages(t, st)
			assert.Equal(map[string]importedMessage{
				"signal:pni:0f0f0f0f-0000-4000-8000-000000000005:peer:1700000390000": {
					conversation: "signal:pni:0f0f0f0f-0000-4000-8000-000000000005", body: "hello owner",
				},
				"signal:pni:0f0f0f0f-0000-4000-8000-000000000005:self:1700000400000": {
					conversation: "signal:pni:0f0f0f0f-0000-4000-8000-000000000005", body: "hello Dana", fromMe: true,
					replyTo: "signal:pni:0f0f0f0f-0000-4000-8000-000000000005:peer:1700000390000",
				},
			}, before)
			changeFixture(t, dir, `UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id IN (7, `+tc.author+`)`)
			summary = runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(int64(2), summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
		})
	}
}

func TestImportGroupAuthorWithoutACI(t *testing.T) {
	for _, tc := range []struct{ name, change, token string }{
		{"phone only", `UPDATE recipient SET aci = NULL, e164 = '+15555550102' WHERE _id = 3`, "tel:+15555550102"},
		{"PNI only", `UPDATE recipient SET aci = NULL, pni = '0f0f0f0f-0000-4000-8000-000000000005' WHERE _id = 3`, "pni:0f0f0f0f-0000-4000-8000-000000000005"},
		{"unknown", `UPDATE recipient SET aci = NULL WHERE _id = 3`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, tc.change, `DELETE FROM message WHERE _id <> 200`)
			st := testutil.NewTestStore(t)
			summary := runImport(t, st, dir, t.TempDir())
			if tc.token == "" {
				assert.Equal(int64(1), summary.MessagesSkipped)
				assert.Empty(importedMessages(t, st))
				return
			}
			assert.Zero(summary.MessagesSkipped)
			assert.Equal(map[string]importedMessage{
				"signal:group:ABEiM0RVZneImaq7zN3u_wARIjNEVWZ3iJmqu8zd7v8:" + tc.token + ":1700000240000": {
					conversation: "signal:group:" + signalfixture.GroupB64, body: "Hi @Alice Example, look\n\nBeach",
				},
			}, importedMessages(t, st))
		})
	}
}

func TestReimportPNIChatGainsACI(t *testing.T) {
	for name, updates := range map[string][]string{
		"same recipient": {`UPDATE recipient SET aci = '0f0f0f0f-0000-4000-8000-000000000006' WHERE _id = 7`},
		"merged into ACI recipient": {
			`INSERT INTO recipient (_id, aci) VALUES (8, '0f0f0f0f-0000-4000-8000-000000000006')`,
			`UPDATE recipient SET pni = (SELECT pni FROM recipient WHERE _id = 7), e164 = (SELECT e164 FROM recipient WHERE _id = 7) WHERE _id = 8`,
			`INSERT INTO thread (_id, recipient_id) VALUES (15, 8)`,
			`UPDATE message SET thread_id = 15, to_recipient_id = 8 WHERE thread_id = 14`,
			`DELETE FROM thread WHERE _id = 14`,
			`DELETE FROM recipient WHERE _id = 7`,
		},
	} {
		t.Run(name, func(t *testing.T) {
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

			changeFixture(t, dir, updates...)
			summary := runImport(t, st, dir, attachments)

			assert.Zero(summary.MessagesAdded)
			assert.Equal(int64(1), summary.MessagesUpdated)
			assert.Equal(before, importedMessages(t, st))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages WHERE id = ?`, messageID))
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM conversations`))
			pid, _, err := st.ParticipantByIdentifierContext(t.Context(), ACIIdentifierType, "0f0f0f0f-0000-4000-8000-000000000006")
			require.NoError(err)
			assert.Equal(participantID, pid)
		})
	}
}
