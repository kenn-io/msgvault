package slack

import (
	"os"
	"path/filepath"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSlackdumpNotifyingUnselectedOmittedLegacyMetadataIsReady(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	root, mediaPath := syntheticSlackdumpMedia(t)
	require.NoError(os.Remove(mediaPath))
	st := testutil.NewTestStore(t)
	configureSlackdumpNotifications(t, st)
	first, err := ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net"})
	require.NoError(err)
	assert.Equal(1, first.AttachmentsMissing)
	require.NoError(os.WriteFile(filepath.Join(root, "example-channel/2024-01-01.json"), []byte(`[{"type":"message","ts":"1704067200.000001","user":"U_PEER","text":"Synthetic offline media"}]`), 0o600))
	second, err := ImportSlackdump(t.Context(), st, root, SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true})
	require.NoError(err, "omitted native metadata must respect this run's explicit attachment opt-out")
	assert.Equal(1, second.AttachmentsSkipped)
	var state, reason string
	require.NoError(st.DB().QueryRow(`SELECT attachment_state,attachment_skip_reason FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE'`).Scan(&state, &reason))
	assert.Equal(string(attachmentpolicy.StateSkipped), state)
	assert.Equal(string(attachmentpolicy.SkipPolicyScope), reason)
	var n int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log`).Scan(&n))
	assert.Zero(n, "silent legacy adoption does not become a new import")
}
