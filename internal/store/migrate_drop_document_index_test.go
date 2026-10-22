package store_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDropDocumentIndexUpgrade(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	backend := "sqlite"
	if f.Store.IsPostgreSQL() {
		backend = "postgres"
	}
	schema, err := os.ReadFile("testdata/document_index_" + backend + ".sql")
	requirements.NoError(err)
	_, err = f.Store.DB().ExecContext(t.Context(), string(schema))
	requirements.NoError(err)
	for _, statement := range []string{
		`INSERT INTO document_extraction_profiles (id,fingerprint,provider,endpoint,region,model,retention_posture,training_posture,allowed_media_types,policy_json,enabled) VALUES ('profile','fingerprint','mistral','https://ocr.example.test','eu','synthetic','standard','opted-out','[]','{}',TRUE)`,
		`INSERT INTO document_extractions (id,profile_id,canonical_blob_hash,extraction_input_key,state,local_bytes) VALUES ('extraction','profile','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','original','ready',128)`,
		`INSERT INTO document_chunks (extraction_id,chunk_key,ordinal,text,heading_path,first_unit_index,last_unit_index,checksum,char_count) VALUES ('extraction','chunk',0,'synthetic extracted text','[]',0,0,'checksum',24)`,
		`DELETE FROM applied_migrations WHERE name IN ('drop_document_index_v1','person_sweep_change_triggers_v6')`,
		`UPDATE archive_metadata SET value='11' WHERE key='schema_version'`,
	} {
		_, err = f.Store.DB().ExecContext(t.Context(), statement)
		requirements.NoError(err)
	}
	requirements.NoError(f.Store.InitSchemaContext(t.Context()))
	// Mutations must still work after the text and its revision ledger are gone.
	messageID := f.CreateMessage("after-document-upgrade")
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='sms' WHERE id=?`), messageID)
	requirements.NoError(err)
	requirements.NoError(f.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{Filename: "synthetic.txt", SourceAttachmentID: "synthetic:attachment"}))
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM attachments WHERE message_id=?`), messageID)
	requirements.NoError(err)
	destination, err := f.Store.GetOrCreateSource("gmail", "merged@example.test")
	requirements.NoError(err)
	_, err = f.Store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: f.Source.ID, IntoSourceID: destination.ID})
	requirements.NoError(err)
	var movedSource int64
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT source_id FROM messages WHERE id=?`), messageID).Scan(&movedSource))
	checks.Equal(destination.ID, movedSource)
	query := `SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'document_%'`
	if f.Store.IsPostgreSQL() {
		query = `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() AND tablename LIKE 'document_%'`
	}
	rows, err := f.Store.DB().QueryContext(t.Context(), query)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	checks.False(rows.Next(), "upgrade deletes local extracted text and consent tables")
	requirements.NoError(rows.Err())
	requirements.NoError(rows.Close())
	var remaining int
	metadataQuery := `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND (name LIKE '%document_%' OR name='trg_person_sweep_documents_message_update')`
	if f.Store.IsPostgreSQL() {
		metadataQuery = `SELECT COUNT(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND (proname LIKE 'bump_document_index_%' OR proname='msgvault_append_person_sweep_document_changes')`
	}
	requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), metadataQuery).Scan(&remaining))
	checks.Zero(remaining, "document triggers and functions are removed")
	requirements.NoError(f.Store.InitSchemaContext(t.Context()), "upgrade is ledgered and repeatable")
}

func TestHistoricalKataDocumentCitationUnavailable(t *testing.T) {
	f := storetest.New(t)
	ref := kataevidence.Reference{Version: 1, Kind: "document_chunk", ArchiveUID: "synthetic-archive", MessageID: 42, SourceType: "gmail", SourceIdentifier: "archive@example.test", SourceMessageID: "message", AttachmentID: 7, OccurrenceKey: "occurrence", DocumentChunk: &kataevidence.DocumentReference{CanonicalBlobHash: strings.Repeat("a", 64), ExtractionID: "extraction", ManifestChecksum: strings.Repeat("b", 64), ChunkKey: "chunk", ChunkChecksum: strings.Repeat("c", 64), EndRune: 12}}
	_, err := kataevidence.Canonicalize(ref)
	require.NoError(t, err)
	_, err = f.Store.ReadKataEvidenceSource(t.Context(), ref)
	require.ErrorIs(t, err, kataevidence.ErrUnavailable)
	_, err = kataevidence.New(f.Store).Prepare(t.Context(), []kataevidence.Selector{{Kind: "document_chunk", MessageID: 42, AttachmentID: 7, ExtractionID: "extraction", ChunkKey: "chunk", MaxChars: new(12)}})
	require.ErrorIs(t, err, kataevidence.ErrInvalidReference)
}
