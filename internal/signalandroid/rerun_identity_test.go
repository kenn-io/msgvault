package signalandroid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestLongTextRevisionIdentityAcrossRestore(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		changes    []string
	}{
		{"restored rows", "Carol here archivedsuffix", []string{
			`UPDATE message SET _id = 900 WHERE _id = 300`,
			`UPDATE attachment SET message_id = 900 WHERE _id = 503`,
		}},
		{"new revision with same preview", "Carol here", []string{
			`UPDATE message SET latest_revision_id = 301 WHERE _id = 300`,
			`INSERT INTO message (_id, date_sent, date_received, thread_id, from_recipient_id, to_recipient_id, type, body, original_message_id) SELECT 301, date_sent + 1, date_received + 1, thread_id, from_recipient_id, to_recipient_id, type, body, 300 FROM message WHERE _id = 300`,
			`UPDATE attachment SET message_id = 301 WHERE _id = 503`,
		}},
		{"changed raw preview", "Carol changed", []string{`UPDATE message SET body = 'Carol changed' WHERE _id = 300`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
			file := filepath.Join(dir, "Attachment_503_-1.bin")
			require.NoError(os.WriteFile(file, []byte("Carol here archivedsuffix"), 0o600))
			storage := t.TempDir()
			runImport(t, st, dir, storage)
			require.NoError(os.Remove(file))
			changeFixture(t, dir, tc.changes...)
			summary := runImport(t, st, dir, storage)
			assert.Zero(summary.Errors)
			assert.Equal(tc.want, importedMessages(t, st)["signal:aci:"+signalfixture.CarolACI+":"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
		})
	}
}

func TestSignalRerunPreservesArchiveLabels(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st, dir := testutil.NewTestStore(t), t.TempDir()
	signalfixture.Write(t, dir, signalfixture.Current, 0)
	storage := t.TempDir()
	first := runImport(t, st, dir, storage)
	labelID, err := st.EnsureLabel(first.SourceID, "kept", "Kept", "user")
	require.NoError(err)
	var messageID int64
	require.NoError(st.DB().QueryRow(`SELECT id FROM messages ORDER BY id LIMIT 1`).Scan(&messageID))
	require.NoError(st.AddMessageLabels(messageID, []int64{labelID}))
	runImport(t, st, dir, storage)
	labels, err := st.MessageLabelIDsContext(t.Context(), messageID)
	require.NoError(err)
	assert.Equal([]int64{labelID}, labels)
}
