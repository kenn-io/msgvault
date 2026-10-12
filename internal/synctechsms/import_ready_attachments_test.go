package synctechsms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
)

func syntheticSynctechMMSFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "messages.xml")
	Require.NoError(t, os.WriteFile(path, []byte(`<smses count="1"><mms date="1717214460000" msg_box="1" m_id="synthetic-ready-mms"><parts><part seq="0" ct="text/plain" text="Synthetic ready MMS" /><part seq="1" ct="application/octet-stream" cl="synthetic.bin" data="U3ludGhldGljIGF0dGFjaG1lbnQ=" /></parts><addrs><addr address="+15550000002" type="137" /><addr address="+15550000001" type="151" /></addrs></mms></smses>`), 0o600))
	return path
}

// The journal's database trigger observes the real transaction, rather than a
// test callback after the importer has completed its legacy attachment phase.
func TestImportSynctechMMSSelectedReferencesExistBeforeJournalAppend(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticSynctechImport(t, st, true)
	var err error
	condition := `NEW.family='msgvault.import_message_archived' AND NOT EXISTS(SELECT 1 FROM attachments WHERE message_id=NEW.message_id AND source_part_key='synctech:mms:1' AND storage_path<>'' AND content_hash<>'')`
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_synctech_reference_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF ` + condition + ` THEN RAISE EXCEPTION 'synthetic selected reference missing at journal append'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_reference_guard BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_synctech_reference_guard()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_reference_guard BEFORE INSERT ON mcp_event_log WHEN ` + condition + ` BEGIN SELECT RAISE(ABORT,'synthetic selected reference missing at journal append'); END`)
	}
	require.NoError(err)
	dir := t.TempDir()
	summary, err := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, IncludeAttachments: true, AttachmentsDir: dir, NotifyImports: true}).ImportPath(syntheticSynctechMMSFile(t))
	require.NoError(err)
	require.Len(summary.MessageIDs, 1)
	assert.Equal(1, summary.AttachmentsImported)
	var relativePath, role, roleSource string
	require.NoError(st.DB().QueryRow(`SELECT storage_path,attachment_role,role_source FROM attachments`).Scan(&relativePath, &role, &roleSource))
	assert.Equal("standalone", role)
	assert.Equal("importer_semantics", roleSource)
	content, err := os.ReadFile(filepath.Join(dir, relativePath))
	require.NoError(err)
	assert.Equal([]byte("Synthetic attachment"), content)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestImportSynctechMMSSelectedAttachmentFailureRollsBackNotification(t *testing.T) {
	for _, failure := range []string{"size", "sql"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureSyntheticSynctechImport(t, st, true)
			opts := ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, IncludeAttachments: true, AttachmentsDir: t.TempDir(), NotifyImports: true}
			cause := "exceeds maximum size"
			if failure == "size" {
				opts.MaxAttachmentBytes = 1
			} else {
				cause = "synthetic required attachment failure"
				var err error
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_synctech_attachment_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required attachment failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_attachment_abort BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION synthetic_synctech_attachment_abort()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_synctech_attachment_abort BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic required attachment failure'); END`)
				}
				require.NoError(err)
			}
			summary, err := NewImporter(st, opts).ImportPath(syntheticSynctechMMSFile(t))
			require.ErrorContains(err, cause)
			assert.Empty(summary.MessageIDs, "an unready notifying projection cannot be reported committed")
			require.NotEmpty(summary.ImportRunID)
			for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "attachments", "import_message_decisions", "import_message_identities", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				assert.Zero(count, "unready message leaked "+table)
			}
		})
	}
}

func TestImportSynctechMMSExplicitNoAttachmentsCanNotify(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticSynctechImport(t, st, true)
	summary, err := NewImporter(st, ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, NotifyImports: true}).ImportPath(syntheticSynctechMMSFile(t))
	require.NoError(err)
	assert.Equal(1, summary.MMSImported)
	assert.Zero(summary.AttachmentsImported)
	var hasAttachments bool
	var count, events int
	require.NoError(st.DB().QueryRow(`SELECT has_attachments,attachment_count FROM messages`).Scan(&hasAttachments, &count))
	assert.False(hasAttachments)
	assert.Zero(count)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&count))
	assert.Zero(count)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

// Native backups may omit previously present parts; stored references survive.
func TestImportSynctechMMSReplayRetainsUnmentionedStoredParts(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticSynctechImport(t, st, true)
	path := syntheticSynctechMMSFile(t)
	single, err := os.ReadFile(path)
	require.NoError(err)
	multiple := strings.Replace(string(single), "</parts>", `<part seq="2" ct="application/octet-stream" cl="second.bin" data="U3ludGhldGljIHNlY29uZA==" /></parts>`, 1)
	require.NoError(os.WriteFile(path, []byte(multiple), 0o600))
	opts := ImportOptions{OwnerPhone: "+15550000001", IncludeMMS: true, IncludeAttachments: true, AttachmentsDir: t.TempDir()}
	first, err := NewImporter(st, opts).ImportPath(path)
	require.NoError(err)
	assert.Equal(2, first.AttachmentsImported)
	require.NoError(os.WriteFile(path, single, 0o600))
	opts.NotifyImports = true
	second, err := NewImporter(st, opts).ImportPath(path)
	require.NoError(err)
	assert.Equal(first.MessageIDs, second.MessageIDs)
	var count int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&count))
	assert.Equal(2, count)
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&count))
	assert.Zero(count, "a prior silent occurrence cannot become a notification")
}
