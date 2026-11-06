package store_test

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDropDocumentIndexUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interrupted bool
	}{
		{name: "interrupted=false"},
		{name: "interrupted=true", interrupted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.interrupted && !store.IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
				t.Skip("PostgreSQL trigger function dependency")
			}
			requirements := require.New(t)
			checks := assert.New(t)
			f := storetest.New(t)
			source, err := f.Store.GetOrCreateSource("slack", "document-upgrade")
			requirements.NoError(err)
			f.Source = source
			f.ConvID, err = f.Store.EnsureConversation(source.ID, "document-upgrade", "Document upgrade")
			requirements.NoError(err)
			ctx := t.Context()
			exec := func(statement string, args ...any) {
				t.Helper()
				_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(statement), args...)
				requirements.NoError(err)
			}

			// An archive the retired pipeline wrote: one attachment with extracted text.
			fixture := "testdata/retired_document_tables_sqlite.sql"
			if f.Store.IsPostgreSQL() {
				fixture = "testdata/retired_document_tables_postgres.sql"
			}
			schema, err := os.ReadFile(fixture)
			requirements.NoError(err)
			exec(string(schema))
			messageID := f.CreateMessage("before-document-upgrade")
			requirements.NoError(f.Store.UpsertAttachmentRecord(ctx, messageID, store.AttachmentWrite{Filename: "synthetic.pdf", SourceAttachmentID: "synthetic:attachment"}))
			var attachmentID int64
			requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT id FROM attachments WHERE message_id=?`), messageID).Scan(&attachmentID))
			hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			exec(`INSERT INTO document_extraction_profiles (id,fingerprint,provider,endpoint,region,model,retention_posture,training_posture,allowed_media_types,policy_json,enabled) VALUES ('profile','fingerprint','mistral','https://ocr.example.test','eu','synthetic','standard','opted-out','[]','{}',TRUE)`)
			exec(`INSERT INTO document_extractions (id,profile_id,canonical_blob_hash,extraction_input_key,state,local_bytes) VALUES ('extraction','profile',?,'original','ready',128)`, hash)
			exec(`INSERT INTO document_chunks (extraction_id,chunk_key,ordinal,text,heading_path,first_unit_index,last_unit_index,checksum,char_count) VALUES ('extraction','chunk',0,'synthetic extracted text','[]',0,0,'checksum',24)`)
			exec(`INSERT INTO document_occurrences (occurrence_key,attachment_id,message_id,source_id,canonical_blob_hash,attachment_role,role_source) VALUES ('occurrence',?,?,?,?,'attachment','source')`, attachmentID, messageID, f.Source.ID, hash)
			exec(`INSERT INTO document_vector_generations (fingerprint,target_extraction_profile_id,embedding_profile,model,dimension,state) VALUES ('vectors','profile','synthetic','synthetic',4,'active')`)
			exec(`INSERT INTO persons (vcard_uid) VALUES ('document-upgrade')`)
			var personID int64
			requirements.NoError(f.Store.DB().QueryRowContext(ctx, `SELECT id FROM persons WHERE vcard_uid='document-upgrade'`).Scan(&personID))
			participantID := f.EnsureParticipant("sender@example.test", "Sender", "example.test")
			exec(`INSERT INTO person_participants (person_id,participant_id) VALUES (?,?)`, personID, participantID)
			_, err = f.Store.SetPersonTrackingContext(ctx, personID, true)
			requirements.NoError(err)
			exec(`UPDATE messages SET sender_id=? WHERE id=?`, participantID, messageID)
			// The previous release's person-sweep document hook and ledger.
			if f.Store.IsPostgreSQL() {
				hook, err := os.ReadFile("testdata/person_sweep_document_triggers_postgres.sql")
				requirements.NoError(err)
				exec(string(hook))
			} else {
				hook, err := os.ReadFile("testdata/person_sweep_document_triggers_sqlite.sql")
				requirements.NoError(err)
				exec(string(hook))
			}
			exec(`DELETE FROM applied_migrations WHERE name IN ('drop_document_index_v1','person_sweep_change_triggers_v6')`)
			consumer, _, err := f.Store.RegisterAttachmentChangeConsumer(ctx, "document-index/v1")
			requirements.NoError(err)
			requirements.NoError(f.Store.CompleteAttachmentChangeReconciliation(ctx, "document-index/v1", consumer.BaselineSequence))

			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			if tc.interrupted {
				exec(`
		UPDATE archive_metadata SET value='31' WHERE key='schema_version';
		CREATE FUNCTION interrupt_document_upgrade() RETURNS trigger AS $$
		BEGIN
			IF NEW.name = 'drop_document_index_v1' THEN RAISE EXCEPTION 'interrupt after document trigger removal'; END IF;
			RETURN NEW;
		END $$ LANGUAGE plpgsql;
		CREATE TRIGGER interrupt_document_upgrade BEFORE INSERT ON applied_migrations
		FOR EACH ROW EXECUTE FUNCTION interrupt_document_upgrade();`)
				exec(`UPDATE messages SET deleted_at=CURRENT_TIMESTAMP WHERE id=?`, messageID)
				err = f.Store.InitSchemaContext(ctx)
				requirements.ErrorContains(err, "interrupt after document trigger removal")
				exec(`UPDATE messages SET deleted_at=NULL, snippet=? WHERE id=?`, "after", messageID)
				var snippet string
				requirements.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT snippet FROM messages WHERE id=?`), messageID).Scan(&snippet))
				checks.Equal("after", snippet, "message writes survive interruption between migrations")
				exec(`DROP TRIGGER interrupt_document_upgrade ON applied_migrations; DROP FUNCTION interrupt_document_upgrade()`)
				logs.Reset()
			}
			requirements.NoError(f.Store.InitSchemaContext(ctx))
			checks.Contains(logs.String(), `"msg":"deleted retired document rows during schema migration"`)

			// The next open re-applies the schema; the dropped hook must stay dropped.
			requirements.NoError(f.Store.InitSchemaContext(ctx))
			checks.Equal(1, strings.Count(logs.String(), "deleted retired document rows during schema migration"))
			hookQuery := `SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='trg_person_sweep_documents_message_update'`
			if f.Store.IsPostgreSQL() {
				hookQuery = `SELECT COUNT(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND proname='msgvault_append_person_sweep_document_changes'`
			}
			var hooks int
			requirements.NoError(f.Store.DB().QueryRowContext(ctx, hookQuery).Scan(&hooks))
			checks.Zero(hooks, "the person-sweep document hook is removed")
			_, err = f.Store.GetAttachmentChangeConsumer(ctx, "document-index/v1")
			requirements.ErrorIs(err, store.ErrAttachmentChangeConsumerMissing)
			count := func(query string) int {
				t.Helper()
				var n int
				requirements.NoError(f.Store.DB().QueryRowContext(ctx, query).Scan(&n))
				return n
			}
			retired := `SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'document%' OR name LIKE 'trg_document%' OR sql LIKE '%document_key%'`
			if f.Store.IsPostgreSQL() {
				retired = `SELECT (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name LIKE 'document%')
			+ (SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=current_schema() AND column_name LIKE '%document_key')
			+ (SELECT COUNT(*) FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND proname LIKE '%document%')`
			}
			checks.Zero(count(retired), "the upgrade drops the retired tables, triggers, functions, and cursor columns")
		})
	}
}
