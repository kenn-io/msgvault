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

func TestUnreadableSignalLongTextKeepsMessage(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "first import", true: "rerun"}[archived], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, dir := testutil.NewTestStore(t), t.TempDir()
			signalfixture.Write(t, dir, signalfixture.Current, 0)
			changeFixture(t, dir, `INSERT INTO attachment (_id, message_id, content_type) VALUES (503, 300, 'text/x-signal-plain')`)
			file := filepath.Join(dir, "Attachment_503_-1.bin")
			require.NoError(os.WriteFile(file, []byte("Carol here archivedsuffix"), 0o600))
			storage, want := t.TempDir(), "Carol here"
			if archived {
				runImport(t, st, dir, storage)
				want = "Carol here archivedsuffix"
			}
			require.NoError(os.Chmod(file, 0o000))
			t.Cleanup(func() { require.NoError(os.Chmod(file, 0o600)) })
			_, err := os.ReadFile(file)
			require.ErrorIs(err, os.ErrPermission)
			summary := runImport(t, st, dir, storage)
			assert.Equal(int64(1), summary.Errors)
			assert.Equal(int64(2), summary.AttachmentsMissing)
			assert.Equal(int64(6), summary.MessagesAdded+summary.MessagesUpdated)
			assert.Equal(want, importedMessages(t, st)["signal:aci:"+signalfixture.CarolACI+":"+signalfixture.CarolACI+":"+ms(signalfixture.TCarol)].body)
		})
	}
}
