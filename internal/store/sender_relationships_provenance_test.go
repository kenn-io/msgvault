package store_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/email"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSenderRelationshipsRawImportOwnershipSurvivesSourceMerge(t *testing.T) {
	for _, provider := range []string{"gmail", "imap"} {
		t.Run(provider, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			root := t.TempDir()
			mailbox := filepath.Join(root, "Archive.mailbox")
			requirements.NoError(os.Mkdir(mailbox, 0o700))
			raw := email.NewMessage().From("history@example.com").To("target@example.com").
				Header("Message-ID", "<raw-relationship@example.com>").
				Subject("Synthetic import").Body("Hello").Bytes()
			requirements.NoError(os.WriteFile(filepath.Join(mailbox, "message.eml"), raw, 0o600))
			summary, err := importer.ImportEMLDir(t.Context(), st, root, importer.EMLImportOptions{
				SourceType: provider, Identifier: "history@example.com", NoResume: true,
				AttachmentsDir: filepath.Join(root, "attachments"),
			})
			requirements.NoError(err)
			requirements.Equal(int64(1), summary.MessagesAdded)
			// Reproduce archives written before raw-import ownership was separated:
			// an address match could persist as native ownership on an email row.
			_, err = st.DB().Exec(`UPDATE messages SET source_is_from_me=TRUE, is_from_me=TRUE`)
			requirements.NoError(err)
			got, err := st.SenderRelationships(t.Context(), []string{"target@example.com"}, nil)
			requirements.NoError(err)
			want := store.SenderRelationshipEvidence{Relationship: "none", SeenInArchive: true}
			assertions.Equal(want, got["target@example.com"])
			from := summary.SourceID
			for _, identifier := range []string{"live@example.com", "final@example.com"} {
				into, err := st.GetOrCreateSource(provider, identifier)
				requirements.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
				requirements.NoError(err)
				got, err = st.SenderRelationships(t.Context(), []string{"target@example.com"}, []int64{into.ID})
				requirements.NoError(err)
				assertions.Equal(want, got["target@example.com"], "archive-ID rewrites must retain raw-import distrust")
				from = into.ID
			}
		})
	}
}

func TestCopySubsetPreservesMicrosoftSentEnvelopeSnapshot(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("CopySubset operates on SQLite archive files")
	}
	f := storetest.New(t)
	ctx := t.Context()

	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(
		`UPDATE sources SET source_type='msmail' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	messageID := senderRelationshipMessage(t, f, true, "to", "visible@example.test")
	bccID := f.EnsureParticipant("private@example.test", "Private Recipient", "example.test")
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`INSERT INTO message_recipients(
		message_id, participant_id, recipient_type, email_address
	) VALUES (?, ?, 'bcc', ?)`), messageID, bccID, "private@example.test")
	requirements.NoError(err)
	folders, err := f.Store.EnsureMicrosoftMailFoldersContext(ctx, f.Source.ID, map[string]store.LabelInfo{
		"sent": {Name: "Sent", Type: "system", SystemRole: store.LabelSystemRoleSent},
	})
	requirements.NoError(err)
	sentLabel := folders["sent"]
	_, err = f.Store.ReconcileMicrosoftMailLabelsContext(ctx, messageID, &sentLabel, nil)
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(
		`DELETE FROM message_recipients WHERE message_id=? AND recipient_type='bcc'`), messageID)
	requirements.NoError(err)

	destinationDir := filepath.Join(t.TempDir(), "subset")
	_, err = store.CopySubset(store.DBPathForTest(f.Store), destinationDir, 1, false)
	requirements.NoError(err)
	destination, err := store.Open(filepath.Join(destinationDir, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(destination.Close()) })

	got, err := destination.SenderRelationships(ctx, []string{"private@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}, got["private@example.test"])
}

func TestCopySubsetBackfillsMicrosoftSentEnvelopeFromSchema20Source(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	if store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("CopySubset operates on SQLite archive files")
	}
	f := storetest.New(t)
	ctx := t.Context()
	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(
		`UPDATE sources SET source_type='msmail' WHERE id=?`), f.Source.ID)
	requirements.NoError(err)
	messageID := senderRelationshipMessage(t, f, true, "to", "target@example.test")
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(
		`UPDATE messages SET source_message_id=?,source_is_from_me=TRUE,metadata=? WHERE id=?`),
		"eml-"+strings.Repeat("a", 64), `{"msmail_sent_items_observed":true}`, messageID)
	requirements.NoError(err)
	for _, statement := range []string{
		`DROP TABLE msmail_sent_envelope_recipients`,
		`DELETE FROM applied_migrations WHERE name='msmail_sent_envelope_recipients_v1'`,
		`UPDATE archive_metadata SET value='20' WHERE key='schema_version'`,
	} {
		_, err = f.Store.DB().ExecContext(ctx, statement)
		requirements.NoError(err)
	}

	destinationDir := filepath.Join(t.TempDir(), "schema20-subset")
	_, err = store.CopySubset(store.DBPathForTest(f.Store), destinationDir, 1, false)
	requirements.NoError(err)
	destination, err := store.Open(filepath.Join(destinationDir, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { requirements.NoError(destination.Close()) })

	var applied bool
	requirements.NoError(destination.DB().QueryRowContext(ctx, destination.Rebind(`
		SELECT EXISTS(SELECT 1 FROM applied_migrations WHERE name=?)
	`), "msmail_sent_envelope_recipients_v1").Scan(&applied))
	assertions.True(applied, "the copied destination must complete the Sent envelope backfill after copying")
	var copiedRecipient bool
	requirements.NoError(destination.DB().QueryRowContext(ctx, destination.Rebind(`
		SELECT EXISTS(SELECT 1 FROM msmail_sent_envelope_recipients WHERE message_id=? AND email_address=?)
	`), messageID, "target@example.test").Scan(&copiedRecipient))
	assertions.True(copiedRecipient, "the copied destination must contain the backfilled Sent recipient")
	got, err := destination.SenderRelationships(ctx, []string{"target@example.test"}, nil)
	requirements.NoError(err)
	assertions.Equal(store.SenderRelationshipEvidence{Relationship: "known", SeenInArchive: true}, got["target@example.test"])
}
