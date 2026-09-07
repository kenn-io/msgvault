package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestSetBeeperAttachmentClassificationsReconcilesStaleRowsAndOccurrences(t *testing.T) {
	f := storetest.New(t)
	require := require.New(t)
	assert := assert.New(t)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`UPDATE sources SET source_type = 'beeper' WHERE id = ?`), f.Source.ID)
	require.NoError(err)
	messageID := f.CreateMessage("beeper-message")
	firstHash := strings.Repeat("a", 64)
	secondHash := strings.Repeat("b", 64)
	thirdHash := strings.Repeat("c", 64)
	for _, attachment := range []store.AttachmentWrite{
		{Filename: "voice.ogg", MIMEType: "audio/ogg", StoragePath: "aa/" + firstHash, ContentHash: firstHash,
			Size: 10, SourceAttachmentID: "beeper:voice", MediaType: "voice_note", State: attachmentpolicy.StateStored,
			Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics},
		{Filename: "old.ogg", MIMEType: "audio/ogg", StoragePath: "bb/" + secondHash, ContentHash: secondHash,
			Size: 10, SourceAttachmentID: "beeper:removed", MediaType: "audio", State: attachmentpolicy.StateStored,
			Role: store.AttachmentRoleSticker, RoleSource: store.AttachmentRoleSourceProviderExplicit},
		{Filename: "other.ogg", MIMEType: "audio/ogg", StoragePath: "cc/" + thirdHash, ContentHash: thirdHash, Metadata: `{"shared_url":"https://other.example"}`,
			Size: 10, SourceAttachmentID: "slack:other", MediaType: "audio", State: attachmentpolicy.StateStored,
			Role: store.AttachmentRolePreview, RoleSource: store.AttachmentRoleSourceProviderExplicit},
	} {
		require.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, attachment))
	}
	reconciler, err := documentindex.NewReconciler(f.Store, documentindex.ReconcilerConfig{
		AttachmentPageSize: 10, ChangePageSize: 10,
	})
	require.NoError(err)
	_, err = reconciler.Reconcile(t.Context())
	require.NoError(err)

	var beforeRevision, afterRevision int64
	require.NoError(f.Store.DB().QueryRow(`SELECT revision FROM document_index_state WHERE singleton = 1`).Scan(&beforeRevision))
	var beforeDerivedRevision int64
	require.NoError(f.Store.DB().QueryRow(`
		SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'), 0)`).Scan(&beforeDerivedRevision))
	changed, err := f.Store.SetBeeperAttachmentClassifications(messageID, map[string]store.BeeperAttachmentClassification{
		"beeper:voice": {Metadata: `{"source_transcript":{"provider":"beeper","text":"hello"}}`, IsPreview: true},
	})
	require.NoError(err)
	assert.EqualValues(2, changed)
	require.NoError(f.Store.DB().QueryRow(`SELECT revision FROM document_index_state WHERE singleton = 1`).Scan(&afterRevision))
	assert.Equal(beforeRevision, afterRevision, "attachment classification leaves occurrence reconciliation to its journal owner")
	var afterDerivedRevision int64
	require.NoError(f.Store.DB().QueryRow(`
		SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'`).Scan(&afterDerivedRevision))
	assert.Equal(beforeDerivedRevision+1, afterDerivedRevision)

	var metadata, role, roleSource string
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COALESCE(CAST(attachment_metadata AS TEXT), ''), attachment_role, role_source
		FROM attachments WHERE source_attachment_id = ?`), "beeper:voice").Scan(&metadata, &role, &roleSource))
	assert.JSONEq(`{"source_transcript":{"provider":"beeper","text":"hello"}}`, metadata)
	assert.Equal("preview", role)
	assert.Equal("importer_semantics", roleSource)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COALESCE(CAST(attachment_metadata AS TEXT), ''), attachment_role, role_source
		FROM attachments WHERE source_attachment_id = ?`), "beeper:removed").Scan(&metadata, &role, &roleSource))
	assert.Empty(metadata)
	assert.Equal("unknown", role)
	assert.Equal("unknown", roleSource)
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COALESCE(CAST(attachment_metadata AS TEXT), ''), attachment_role, role_source
		FROM attachments WHERE source_attachment_id = ?`), "slack:other").Scan(&metadata, &role, &roleSource))
	assert.JSONEq(`{"shared_url":"https://other.example"}`, metadata)
	assert.Equal("preview", role)
	assert.Equal("provider_explicit", roleSource)
	result, err := reconciler.Reconcile(t.Context())
	require.NoError(err)
	assert.Positive(result.ChangesConsumed)
	var occurrenceCount int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COUNT(*) FROM document_occurrences WHERE message_id = ?`), messageID).Scan(&occurrenceCount))
	assert.Zero(occurrenceCount, "the existing reconciler removes the now-ineligible preview occurrence")
	require.NoError(f.Store.DB().QueryRow(`SELECT revision FROM document_index_state WHERE singleton = 1`).Scan(&afterRevision))
	assert.Greater(afterRevision, beforeRevision)
	reconciledRevision := afterRevision

	changed, err = f.Store.SetBeeperAttachmentClassifications(messageID, map[string]store.BeeperAttachmentClassification{
		"beeper:voice": {Metadata: `{"source_transcript":{"provider":"beeper","text":"hello"}}`, IsPreview: true},
	})
	require.NoError(err)
	assert.Zero(changed)
	require.NoError(f.Store.DB().QueryRow(`SELECT revision FROM document_index_state WHERE singleton = 1`).Scan(&afterRevision))
	assert.Equal(reconciledRevision, afterRevision)
	require.NoError(f.Store.DB().QueryRow(`
		SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'`).Scan(&afterDerivedRevision))
	assert.Equal(beforeDerivedRevision+1, afterDerivedRevision)
}

func TestReplaceMessageBeeperAttachmentsAdvancesRevisionForExistingMetadata(t *testing.T) {
	f := storetest.New(t)
	require := require.New(t)
	messageID := f.CreateMessage("beeper-cache-revision")
	ref := func(metadata string) store.AttachmentRef {
		return store.AttachmentRef{
			StoragePath:        "aa/" + strings.Repeat("a", 64),
			SourceAttachmentID: "beeper:voice",
			Metadata:           metadata,
			MediaType:          "voice_note",
			Role:               store.AttachmentRoleStandalone,
			RoleSource:         store.AttachmentRoleSourceImporterSemantics,
		}
	}
	readRevision := func() int64 {
		var revision int64
		require.NoError(f.Store.DB().QueryRow(
			`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'), 0)`).Scan(&revision))
		return revision
	}
	assertRevision := func(want int64) { require.Equal(want, readRevision()) }

	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{
		ref(`{"source_transcript":{"provider":"beeper","text":"old"}}`),
	}))
	assertRevision(1)
	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{
		ref(`{"source_transcript":{"provider":"beeper","text":"old"}}`),
	}))
	assertRevision(1)
	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{
		ref(`{"source_transcript":{"provider":"beeper","text":"new"}}`),
	}))
	assertRevision(2)
	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, nil))
	assertRevision(3)
}

func TestReplaceMessageBeeperAttachmentsAdvancesRevisionForAddedMetadata(t *testing.T) {
	f := storetest.New(t)
	require := require.New(t)
	messageID := f.CreateMessage("beeper-cache-added-attachment")
	ref := func(sourceAttachmentID, storagePath, metadata string) store.AttachmentRef {
		return store.AttachmentRef{
			StoragePath:        storagePath,
			SourceAttachmentID: sourceAttachmentID,
			Metadata:           metadata,
			MediaType:          "voice_note",
			Role:               store.AttachmentRoleStandalone,
			RoleSource:         store.AttachmentRoleSourceImporterSemantics,
		}
	}
	readRevision := func() int64 {
		var revision int64
		require.NoError(f.Store.DB().QueryRow(
			`SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'), 0)`).Scan(&revision))
		return revision
	}
	first := ref("beeper:voice", "aa/"+strings.Repeat("a", 64), `{"source_transcript":{"provider":"beeper","text":"old"}}`)
	second := ref("beeper:photo", "bb/"+strings.Repeat("b", 64), `{"source_transcript":{"provider":"beeper","text":"new"}}`)

	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{first}))
	require.Equal(int64(1), readRevision(), "initial Beeper attachment publication must invalidate exported metadata")
	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{first, second}))
	require.Equal(int64(2), readRevision(), "adding a Beeper attachment must invalidate exported metadata")
	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{first, second}))
	require.Equal(int64(2), readRevision(), "replaying the same attachment set must be a no-op")
}

func TestReplaceMessageBeeperAttachmentsRollsBackRevisionAndMetadata(t *testing.T) {
	f := storetest.New(t)
	require := require.New(t)
	messageID := f.CreateMessage("beeper-cache-rollback")
	ref := store.AttachmentRef{
		StoragePath:        "aa/" + strings.Repeat("a", 64),
		SourceAttachmentID: "beeper:voice",
		Metadata:           `{"source_transcript":{"provider":"beeper","text":"old"}}`,
		MediaType:          "voice_note",
		Role:               store.AttachmentRoleStandalone,
		RoleSource:         store.AttachmentRoleSourceImporterSemantics,
	}
	require.NoError(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{ref}))
	triggerSQL := `
		CREATE TRIGGER fail_derived_revision
		BEFORE UPDATE OF value ON archive_metadata
		WHEN NEW.key = 'derived_data_revision'
		BEGIN SELECT RAISE(ABORT, 'injected revision failure'); END`
	if f.Store.IsPostgreSQL() {
		triggerSQL = `
			CREATE FUNCTION fail_derived_revision() RETURNS trigger AS $$
			BEGIN
				RAISE EXCEPTION 'injected revision failure';
			END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER fail_derived_revision
			BEFORE UPDATE OF value ON archive_metadata
			FOR EACH ROW WHEN (NEW.key = 'derived_data_revision')
			EXECUTE FUNCTION fail_derived_revision()`
	}
	_, err := f.Store.DB().Exec(triggerSQL)
	require.NoError(err)
	ref.Metadata = `{"source_transcript":{"provider":"beeper","text":"new"}}`
	require.Error(f.Store.ReplaceMessageBeeperAttachments(messageID, []store.AttachmentRef{ref}))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`
		SELECT COALESCE(CAST(attachment_metadata AS TEXT), '')
		FROM attachments WHERE source_attachment_id = ?`), "beeper:voice").Scan(&ref.Metadata))
	require.JSONEq(`{"source_transcript":{"provider":"beeper","text":"old"}}`, ref.Metadata)
	var revision int64
	require.NoError(f.Store.DB().QueryRow(`
		SELECT COALESCE((SELECT CAST(value AS INTEGER) FROM archive_metadata WHERE key = 'derived_data_revision'), 0)`).Scan(&revision))
	require.Equal(int64(1), revision)
	dropSQL := `DROP TRIGGER fail_derived_revision`
	if f.Store.IsPostgreSQL() {
		dropSQL = `DROP TRIGGER fail_derived_revision ON archive_metadata; DROP FUNCTION fail_derived_revision()`
	}
	_, err = f.Store.DB().Exec(dropSQL)
	require.NoError(err)
}
