package signalandroid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

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
				"signal:pni:0f0f0f0f-0000-4000-8000-000000000005:0f0f0f0f-0000-4000-8000-000000000001:1700000400000": {
					conversation: "signal:pni:0f0f0f0f-0000-4000-8000-000000000005", body: "hello Dana", fromMe: true,
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
