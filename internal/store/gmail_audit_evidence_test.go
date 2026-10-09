package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGmailAuditEvidenceExcludesPreservedMergeAttachments(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	history, err := st.GetOrCreateSource("gmail", "history@example.test")
	require.NoError(err)
	live, err := st.GetOrCreateSource("gmail", "live@example.test")
	require.NoError(err)

	recipients := []string{"recipient@example.test"}
	survivorID := emailMergeFixtureMessage(t, st, live.ID, "live-message", recipients, true)
	historyID := emailMergeFixtureMessage(t, st, history.ID, "history-message", recipients, true)
	const rfc822MessageID = "<shared@example.test>"
	for _, messageID := range []int64{survivorID, historyID} {
		_, err := st.DB().Exec(st.Rebind(
			`UPDATE messages SET rfc822_message_id = ? WHERE id = ?`), rfc822MessageID, messageID)
		require.NoError(err)
	}

	hash := strings.Repeat("a", 64)
	partKey := "mime:attachment:1"
	for _, messageID := range []int64{survivorID, historyID} {
		require.NoError(st.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
			Filename: "example.txt", MIMEType: "text/plain", StoragePath: hash[:2] + "/" + hash,
			ContentHash: hash, Size: 12, SourcePartKey: partKey,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
		}))
	}

	merged, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{
		FromSourceID: history.ID, IntoSourceID: live.ID,
	})
	require.NoError(err)
	require.Equal(int64(1), merged.DuplicatesHidden)
	require.Equal(int64(1), merged.AttachmentsCopied)

	var preservedCopies int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*)
		FROM attachments a
		JOIN source_merge_preserved_attachments marker ON marker.attachment_id = a.id
		WHERE a.message_id = ?
	`), survivorID).Scan(&preservedCopies))
	require.Equal(1, preservedCopies)

	var evidence []store.GmailAuditEvidence
	pageSize, err := st.StreamGmailAuditEvidencePageContext(
		t.Context(), live.ID, 0, 1, func(item store.GmailAuditEvidence) error {
			evidence = append(evidence, item)
			return nil
		},
	)
	require.NoError(err)
	assert.Equal(1, pageSize)
	require.Len(evidence, 1)
	assert.Equal(survivorID, evidence[0].ID)
	require.Len(evidence[0].Attachments, 1)
	assert.Equal(sql.NullString{String: hash, Valid: true}, evidence[0].Attachments[0].ContentHash)
	assert.Equal(sql.NullString{String: partKey, Valid: true}, evidence[0].Attachments[0].SourcePartKey)
}
