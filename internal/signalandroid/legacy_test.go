package signalandroid

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/signalfixture"
)

func TestImportLegacyGroup(t *testing.T) {
	for name, layout := range map[string]signalfixture.Layout{"current": signalfixture.Current, "v185": signalfixture.V185} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			signalfixture.Write(t, dir, layout, 0)
			changeFixture(t, dir, `DELETE FROM message WHERE thread_id <> 13`)
			st := testutil.NewTestStore(t)
			attachments := t.TempDir()

			summary := runImport(t, st, dir, attachments)

			assert.Equal(int64(1), summary.MessagesAdded)
			before := importedMessages(t, st)
			changeFixture(t, dir, `UPDATE thread SET _id = 113 WHERE _id = 13`, `UPDATE message SET thread_id = 113 WHERE thread_id = 13`)
			summary = runImport(t, st, dir, attachments)
			assert.Zero(summary.MessagesAdded)
			assert.Equal(before, importedMessages(t, st))
		})
	}
}
