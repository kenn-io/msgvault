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

func TestEmptyLongTextKeepsMessage(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "first import", true: "rerun"}[archived], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewSQLiteTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
			file, storage := filepath.Join(dir, "Attachment_503_-1.bin"), t.TempDir()
			want, search := "Carol here", "Carol"
			if archived {
				require.NoError(os.WriteFile(file, []byte("Carol here archivedsuffix"), 0o600))
				runImport(t, st, dir, storage)
				want, search = "Carol here archivedsuffix", "archivedsuffix"
			}
			require.NoError(os.WriteFile(file, nil, 0o600))
			summary := runImport(t, st, dir, storage)
			assert.Equal(int64(2), summary.AttachmentsMissing)
			id := "signal:aci:" + signalfixture.CarolACI + ":" + signalfixture.CarolACI + ":" + ms(signalfixture.TCarol)
			assert.Equal(want, importedMessages(t, st)[id].body)
			assert.Equal(1, countRows(t, st, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH ?`, search))
		})
	}
}
