package store_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDropDocumentMessageTriggersUpgrade(t *testing.T) {
	requirements := require.New(t)
	checks := assert.New(t)
	f := storetest.New(t)
	ctx := t.Context()
	exec := func(statement string, args ...any) {
		t.Helper()
		_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(statement), args...)
		requirements.NoError(err)
	}

	// An archive the retired pipeline wrote: one attachment with extracted text.
	messageID := f.CreateMessage("before-document-upgrade")
	requirements.NoError(f.Store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{Filename: "synthetic.pdf", SourceAttachmentID: "synthetic:attachment"}))
	var attachmentID int64
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT id FROM attachments WHERE message_id=?`), messageID).Scan(&attachmentID))
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	exec(`INSERT INTO document_extraction_profiles (id,fingerprint,provider,endpoint,region,model,retention_posture,training_posture,allowed_media_types,policy_json,enabled) VALUES ('profile','fingerprint','mistral','https://ocr.example.test','eu','synthetic','standard','opted-out','[]','{}',TRUE)`)
	exec(`INSERT INTO document_extractions (id,profile_id,canonical_blob_hash,extraction_input_key,state,local_bytes) VALUES ('extraction','profile',?,'original','ready',128)`, hash)
	exec(`INSERT INTO document_chunks (extraction_id,chunk_key,ordinal,text,heading_path,first_unit_index,last_unit_index,checksum,char_count) VALUES ('extraction','chunk',0,'synthetic extracted text','[]',0,0,'checksum',24)`)
	exec(`INSERT INTO document_occurrences (occurrence_key,attachment_id,message_id,source_id,canonical_blob_hash,attachment_role,role_source) VALUES ('occurrence',?,?,?,?,'attachment','source')`, attachmentID, messageID, f.Source.ID, hash)

	// The previous release's person-sweep document hook and ledger.
	if f.Store.IsPostgreSQL() {
		schema, err := os.ReadFile("testdata/person_sweep_document_triggers_postgres.sql")
		requirements.NoError(err)
		exec(string(schema))
	} else {
		exec(`CREATE TRIGGER trg_person_sweep_documents_message_update AFTER UPDATE ON messages BEGIN SELECT 1; END`)
	}
	exec(`DELETE FROM applied_migrations WHERE name IN ('drop_document_message_triggers_v1','person_sweep_change_triggers_v6')`)
	for _, key := range []string{"document-index/v1", "visual-index/v1"} {
		consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(ctx, key)
		requirements.NoError(err)
		requirements.NoError(f.Store.CompleteAttachmentChangeReconciliation(ctx, key, consumer.BaselineSequence))
	}

	requirements.NoError(f.Store.InitSchemaContext(ctx))

	_, err := f.Store.GetAttachmentChangeConsumer(ctx, "document-index/v1")
	requirements.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
	exec(`UPDATE messages SET message_type='sms' WHERE id=?`, messageID)
	exec(`DELETE FROM attachments WHERE message_id=?`, messageID)
	destination, err := f.Store.GetOrCreateSource("gmail", "merged@example.test")
	requirements.NoError(err)
	_, err = f.Store.MergeSourcesContext(ctx, store.MergeSourcesRequest{FromSourceID: f.Source.ID, IntoSourceID: destination.ID})
	requirements.NoError(err)
	var movedSource int64
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT source_id FROM messages WHERE id=?`), messageID).Scan(&movedSource))
	checks.Equal(destination.ID, movedSource)
	changes, err := f.Store.ListAttachmentChanges(ctx, "visual-index/v1", 100)
	requirements.NoError(err)
	requirements.NotEmpty(changes)
	requirements.NoError(f.Store.AdvanceAttachmentChangeConsumer(ctx, "visual-index/v1", changes[len(changes)-1].Sequence))
	var pending int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM attachment_change_log`).Scan(&pending))
	checks.Zero(pending, "remaining consumers can prune changes after upgrade")

	// The next open re-applies the schema; the dropped hook must stay dropped.
	requirements.NoError(f.Store.InitSchemaContext(ctx))
	hookQuery := `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='trg_person_sweep_documents_message_update'`
	if f.Store.IsPostgreSQL() {
		hookQuery = `SELECT COUNT(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND proname='msgvault_append_person_sweep_document_changes'`
	}
	var hooks int
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, hookQuery).Scan(&hooks))
	checks.Zero(hooks, "the person-sweep document hook is removed")
	_, err = f.Store.GetAttachmentChangeConsumer(ctx, "document-index/v1")
	requirements.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
	var text string
	requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT text FROM document_chunks WHERE chunk_key='chunk'`).Scan(&text))
	checks.Equal("synthetic extracted text", text, "extracted text stays in the archive")
}
