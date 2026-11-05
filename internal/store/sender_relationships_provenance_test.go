package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/email"
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
