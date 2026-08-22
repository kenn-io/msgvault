package store_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestVisualCandidateCatalogUsesMessageScopedOwners(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	firstMessage := f.CreateMessage("visual-candidate-first")
	secondMessage := f.CreateMessage("visual-candidate-second")
	hash := strings.Repeat("a1", 32)
	writeVisualAttachment(t, f, firstMessage, "part:2", hash, hash[:2]+"/"+hash,
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics)
	writeVisualAttachment(t, f, firstMessage, "part:1", hash, hash[:2]+"/"+hash,
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceMIMEDisposition)
	writeVisualAttachment(t, f, secondMessage, "part:1", hash, hash[:2]+"/"+hash,
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceProviderExplicit)

	page, err := f.Store.ListVisualCandidates(t.Context(), store.VisualCandidateFilter{})
	require.NoError(err)
	require.Len(page.Candidates, 2)
	assert.Equal(firstMessage, page.Candidates[0].Owner.MessageID)
	assert.Equal(int64(2), page.Candidates[0].OccurrenceCount)
	assert.Equal(secondMessage, page.Candidates[1].Owner.MessageID)
	assert.Equal(int64(1), page.Candidates[1].OccurrenceCount)
	assert.Equal(hash, page.Candidates[0].Owner.BlobHash)
	assert.Equal(store.VisualOriginalMediaInputKey, page.Candidates[0].Owner.MediaInputKey)
	assert.Less(page.Candidates[0].RepresentativeAttachmentID,
		page.Candidates[1].RepresentativeAttachmentID)
	assert.Equal(int64(3), page.Counts.StandaloneOccurrences)
}

func TestVisualCandidateCatalogFailsClosedAndRecoversTrustedCASHash(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("visual-candidate-policy")
	hash := strings.Repeat("b2", 32)
	writeVisualAttachment(t, f, messageID, "trusted", "", hash[:2]+"/"+hash,
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics)
	writeVisualAttachment(t, f, messageID, "unknown-role", hash, hash[:2]+"/"+hash,
		store.AttachmentRoleUnknown, store.AttachmentRoleSourceUnknown)
	writeVisualAttachment(t, f, messageID, "legacy-source", hash, hash[:2]+"/"+hash,
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceLegacyAPI)
	writeVisualAttachment(t, f, messageID, "inline", hash, hash[:2]+"/"+hash,
		store.AttachmentRoleInline, store.AttachmentRoleSourceMIMEDisposition)
	writeVisualAttachment(t, f, messageID, "missing", "", "provider:pending:1",
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceProviderExplicit)

	page, err := f.Store.ListVisualCandidates(t.Context(), store.VisualCandidateFilter{
		MessageIDs: []int64{messageID},
	})
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(hash, page.Candidates[0].Owner.BlobHash)
	assert.Equal(int64(2), page.Counts.UnknownRoleOccurrences)
	assert.Equal(int64(1), page.Counts.IneligibleOccurrences)
	assert.Equal(int64(2), page.Counts.StandaloneOccurrences)
	assert.Equal(int64(1), page.Counts.UnavailableOccurrences)
}

func TestVisualCandidateCatalogPagesWholeMessagesAndAppliesScope(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	first := f.CreateMessage("visual-page-first")
	second := f.CreateMessage("visual-page-second")
	third := f.CreateMessage("visual-page-third")
	require.NoError(updateVisualMessageType(f, first, "mms"))
	require.NoError(updateVisualMessageType(f, second, "MMS"))
	require.NoError(updateVisualMessageType(f, third, "email"))
	for index, messageID := range []int64{first, second, third} {
		hash := strings.Repeat(string(rune('c'+index)), 64)
		writeVisualAttachment(t, f, messageID, "part:1", hash, hash[:2]+"/"+hash,
			store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics)
	}

	page, err := f.Store.ListVisualCandidates(t.Context(), store.VisualCandidateFilter{
		LimitMessages: 1, MessageTypes: []string{" mMs "},
	})
	require.NoError(err)
	require.Len(page.Candidates, 1)
	assert.Equal(first, page.Candidates[0].Owner.MessageID)
	assert.True(page.HasMore)

	next, err := f.Store.ListVisualCandidates(t.Context(), store.VisualCandidateFilter{
		AfterMessageID: page.NextAfterMessageID, LimitMessages: 1, MessageTypes: []string{"mms"},
	})
	require.NoError(err)
	require.Len(next.Candidates, 1)
	assert.Equal(second, next.Candidates[0].Owner.MessageID)
	assert.False(next.HasMore)
}

func TestVisualCandidateCatalogReflectsMovesRolesReplacementAndDeletion(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	first := f.CreateMessage("visual-lifecycle-first")
	second := f.CreateMessage("visual-lifecycle-second")
	oldHash := strings.Repeat("d4", 32)
	newHash := strings.Repeat("e5", 32)
	writeVisualAttachment(t, f, first, "stable-part", oldHash, oldHash[:2]+"/"+oldHash,
		store.AttachmentRoleStandalone, store.AttachmentRoleSourceImporterSemantics)

	assertVisualOwners(t, f, []store.VisualOwner{{
		MessageID: first, BlobHash: oldHash, MediaInputKey: store.VisualOriginalMediaInputKey,
	}})
	require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), first, store.AttachmentWrite{
		Filename: "replacement.png", MIMEType: "image/png", StoragePath: newHash[:2] + "/" + newHash,
		ContentHash: newHash, Size: 20, Role: store.AttachmentRoleStandalone,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "stable-part",
	}))
	assertVisualOwners(t, f, []store.VisualOwner{{
		MessageID: first, BlobHash: newHash, MediaInputKey: store.VisualOriginalMediaInputKey,
	}})

	var attachmentID int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT id FROM attachments WHERE message_id = ?`), first).Scan(&attachmentID))
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET message_id = ? WHERE id = ?`), second, attachmentID)
	require.NoError(err)
	assertVisualOwners(t, f, []store.VisualOwner{{
		MessageID: second, BlobHash: newHash, MediaInputKey: store.VisualOriginalMediaInputKey,
	}})

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE attachments SET attachment_role = 'inline' WHERE id = ?`), attachmentID)
	require.NoError(err)
	assertVisualOwners(t, f, nil)

	_, err = f.Store.DB().Exec(f.Store.Rebind(`DELETE FROM attachments WHERE id = ?`), attachmentID)
	require.NoError(err)
	assertVisualOwners(t, f, nil)
}

func TestVisualMessageContextUsesDirectBodyFallbackAndLivePredicate(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	messageID := f.CreateMessage("visual-context")
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET subject = ?, message_type = ? WHERE id = ?`),
		"Synthetic subject", "mms", messageID)
	require.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`INSERT INTO message_bodies (message_id, body_html) VALUES (?, ?)`),
		messageID, `<p>Synthetic <strong>body</strong></p>`)
	require.NoError(err)

	context, err := f.Store.GetVisualMessageContext(t.Context(), messageID)
	require.NoError(err)
	assert.Equal("Synthetic subject", context.Subject)
	assert.Equal("Synthetic body", context.Body)
	assert.Equal("mms", context.MessageType)

	_, err = f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`), messageID)
	require.NoError(err)
	_, err = f.Store.GetVisualMessageContext(t.Context(), messageID)
	assert.ErrorIs(err, sql.ErrNoRows)
}

func writeVisualAttachment(
	t *testing.T,
	f *storetest.Fixture,
	messageID int64,
	partKey, hash, storagePath string,
	role store.AttachmentRole,
	roleSource store.AttachmentRoleSource,
) {
	t.Helper()
	require.NoError(t, f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: "synthetic.png", MIMEType: "image/png", StoragePath: storagePath,
		ContentHash: hash, Size: 10, Role: role, RoleSource: roleSource, SourcePartKey: partKey,
	}))
}

func updateVisualMessageType(f *storetest.Fixture, messageID int64, messageType string) error {
	_, err := f.Store.DB().Exec(f.Store.Rebind(
		`UPDATE messages SET message_type = ? WHERE id = ?`), messageType, messageID)
	return err
}

func assertVisualOwners(t *testing.T, f *storetest.Fixture, want []store.VisualOwner) {
	t.Helper()
	page, err := f.Store.ListVisualCandidates(t.Context(), store.VisualCandidateFilter{})
	require.NoError(t, err)
	got := make([]store.VisualOwner, len(page.Candidates))
	for i := range page.Candidates {
		got[i] = page.Candidates[i].Owner
	}
	if want == nil {
		assert.Empty(t, got)
		return
	}
	assert.Equal(t, want, got)
}
