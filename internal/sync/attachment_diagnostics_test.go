package sync

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttachmentReplayRemoteImageErrorContext(t *testing.T) {
	for _, present := range []bool{true, false} {
		t.Run(map[bool]string{true: "archive", false: "read"}[present], func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			e := attachmentPersistenceEnv(t)
			source := e.CreateSource(t)
			if present {
				seedRepairRow(t, e.Store, source.ID, "pending-image", "thread", "subject")
			}
			service := New(nil, e.Store, &Options{AttachmentsDir: t.TempDir()})
			service.pendingAttachmentRepairs = map[string]materializationWork{"pending-image": archiveRemoteImages}
			err := service.repairPendingAttachments(t.Context(), source.ID, "pending-image")
			require.ErrorIs(err, errRemoteImageArchival)
			assert.Equal(1, strings.Count(err.Error(), "archive remote images:"), "both archive and message-read failures need exactly one prefix")
		})
	}
}
