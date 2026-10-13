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

func TestImportSlackdumpNotifyingMediaPolicySkipsAreReady(t *testing.T) {
	for _, policy := range []string{"unselected", "scope", "size", "actual_size"} {
		t.Run(policy, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			root, mediaPath := syntheticSlackdumpMedia(t)
			st := testutil.NewTestStore(t)
			configureSlackdumpNotifications(t, st)
			opts := SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true, AttachmentsDir: t.TempDir()}
			reason := attachmentpolicy.SkipPolicyScope
			switch policy {
			case "unselected":
				opts.AttachmentsDir = ""
				require.NoError(os.Remove(mediaPath))
			case "scope":
				opts.MediaPolicy.Scope = attachmentpolicy.ScopeNone
				require.NoError(os.Remove(mediaPath))
			case "size":
				opts.MaxMediaBytes = 1
				reason = attachmentpolicy.SkipSizeCap
			case "actual_size":
				opts.MaxMediaBytes = 15
				require.NoError(os.WriteFile(mediaPath, []byte("synthetic bytes above the declared file size"), 0o600))
				reason = attachmentpolicy.SkipSizeCap
			}
			installSlackdumpMediaAbort(t, st, "mcp_event_log", `NOT EXISTS (SELECT 1 FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE' AND attachment_state='skipped' AND attachment_skip_reason<>'')`)
			summary, err := ImportSlackdump(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Equal(1, summary.AttachmentsSkipped)
			assert.Zero(summary.AttachmentsMissing)
			var state, gotReason string
			require.NoError(st.DB().QueryRow(`SELECT attachment_state,attachment_skip_reason FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE'`).Scan(&state, &gotReason))
			assert.Equal(string(attachmentpolicy.StateSkipped), state)
			assert.Equal(string(reason), gotReason)
		})
	}
}

func TestImportSlackdumpNotifyingRetainsNativeStoredAndLegacyReferences(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		name := "present"
		if omitted {
			name = "omitted"
		}
		t.Run(name, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			root, mediaPath := syntheticSlackdumpMedia(t)
			st := testutil.NewTestStore(t)
			configureSlackdumpNotifications(t, st)
			storage := t.TempDir()
			opts := SlackdumpImportOptions{Me: "owner@example.net", NotifyImports: true, AttachmentsDir: storage}
			first, err := ImportSlackdump(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Equal(1, first.AttachmentsDownloaded)
			var originalPath, originalHash string
			require.NoError(st.DB().QueryRow(`SELECT storage_path,content_hash FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE'`).Scan(&originalPath, &originalHash))
			_, err = st.DB().Exec(`UPDATE attachments SET attachment_state='' WHERE source_attachment_id='slack:F_EXAMPLE'`)
			require.NoError(err)
			require.NoError(os.Remove(mediaPath))
			if omitted {
				require.NoError(os.WriteFile(filepath.Join(root, "example-channel/2024-01-01.json"), []byte(`[{"type":"message","ts":"1704067200.000001","user":"U_PEER","text":"Synthetic offline media"}]`), 0o600))
			}
			second, err := ImportSlackdump(t.Context(), st, root, opts)
			require.NoError(err)
			assert.Zero(second.AttachmentsDownloaded)
			assert.Zero(second.AttachmentsMissing)
			var path, hash, state string
			require.NoError(st.DB().QueryRow(`SELECT storage_path,content_hash,attachment_state FROM attachments WHERE source_attachment_id='slack:F_EXAMPLE'`).Scan(&path, &hash, &state))
			assert.Equal(originalPath, path)
			assert.Equal(originalHash, hash)
			assert.Equal(string(attachmentpolicy.StateStored), state)
			var n int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&n))
			assert.Equal(1, n, "a fresh native run does not replay an existing occurrence")
		})
	}
}
