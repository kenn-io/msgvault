package gvoice

import (
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestImportGvoiceSelectedAudioExistsBeforeJournalAppend(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	st := testutil.NewTestStore(t)
	configureSyntheticVoiceImports(t, st, true)
	condition := `NEW.family='msgvault.import_message_archived' AND NOT EXISTS(SELECT 1 FROM attachments WHERE message_id=NEW.message_id AND source_part_key='gvoice:voicemail:audio' AND attachment_state='stored' AND storage_path<>'' AND content_hash<>'')`
	var err error
	if st.IsPostgreSQL() {
		_, err = st.DB().Exec(`CREATE FUNCTION synthetic_voice_reference_guard() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF ` + condition + ` THEN RAISE EXCEPTION 'synthetic selected audio missing at journal append'; END IF; RETURN NEW; END $$`)
		require.NoError(err)
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_voice_reference_guard BEFORE INSERT ON mcp_event_log FOR EACH ROW EXECUTE FUNCTION synthetic_voice_reference_guard()`)
	} else {
		_, err = st.DB().Exec(`CREATE TRIGGER synthetic_voice_reference_guard BEFORE INSERT ON mcp_event_log WHEN ` + condition + ` BEGIN SELECT RAISE(ABORT,'synthetic selected audio missing at journal append'); END`)
	}
	require.NoError(err)
	voice := writeVoice(t, voicemailFixture{fixture: "with-audio.html", html: withAudioHTML, audio: withAudioMP3, bytes: []byte("Synthetic voice ready audio")})
	attachmentRoot := t.TempDir()
	client, err := NewClient(voice, WithAttachmentsDir(attachmentRoot), WithNotifyImports(true))
	require.NoError(err)
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(err)
	summary, err := client.Import(t.Context(), st, source.ID)
	require.NoError(err)
	require.Equal(1, summary.MessagesImported)
	assert.Zero(summary.Skipped)
	var path string
	require.NoError(st.DB().QueryRow(`SELECT storage_path FROM attachments`).Scan(&path))
	content, err := os.ReadFile(filepath.Join(attachmentRoot, path))
	require.NoError(err)
	assert.Equal([]byte("Synthetic voice ready audio"), content)
	var events int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
	assert.Equal(1, events)
}

func TestImportGvoiceUnreadySelectedAudioRollsBackAndCanRetry(t *testing.T) {
	for _, failure := range []string{"missing-file", "sql"} {
		t.Run(failure, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			st := testutil.NewTestStore(t)
			configureSyntheticVoiceImports(t, st, true)
			voice := writeVoice(t, voicemailFixture{fixture: "with-audio.html", html: withAudioHTML, audio: withAudioMP3, bytes: []byte("Synthetic retry audio")})
			if failure == "missing-file" {
				require.NoError(os.Remove(filepath.Join(voice, "Calls", withAudioMP3)))
			} else {
				var err error
				if st.IsPostgreSQL() {
					_, err = st.DB().Exec(`CREATE FUNCTION synthetic_voice_attachment_abort() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic required audio failure'; END $$`)
					require.NoError(err)
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_voice_attachment_abort BEFORE INSERT ON attachments FOR EACH ROW EXECUTE FUNCTION synthetic_voice_attachment_abort()`)
				} else {
					_, err = st.DB().Exec(`CREATE TRIGGER synthetic_voice_attachment_abort BEFORE INSERT ON attachments BEGIN SELECT RAISE(ABORT,'synthetic required audio failure'); END`)
				}
				require.NoError(err)
			}
			client, err := NewClient(voice, WithAttachmentsDir(t.TempDir()), WithNotifyImports(true))
			require.NoError(err)
			source, err := st.GetOrCreateSource("google_voice", client.Identifier())
			require.NoError(err)
			summary, err := client.Import(t.Context(), st, source.ID)
			require.NoError(err, "native per-entry failure continues")
			assert.Zero(summary.MessagesImported)
			assert.Equal(1, summary.Skipped)
			for _, table := range []string{"participants", "participant_identifiers", "conversations", "conversation_participants", "messages", "message_bodies", "message_raw", "message_recipients", "attachments", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				assert.Zero(count, "unready selected audio leaked "+table)
			}
			if failure == "missing-file" {
				require.NoError(os.WriteFile(filepath.Join(voice, "Calls", withAudioMP3), []byte("Synthetic retry audio"), 0o600))
			} else {
				query := `DROP TRIGGER synthetic_voice_attachment_abort`
				if st.IsPostgreSQL() {
					query += ` ON attachments`
				}
				_, err = st.DB().Exec(query)
				require.NoError(err)
			}
			retry, err := client.Import(t.Context(), st, source.ID)
			require.NoError(err)
			assert.Equal(1, retry.MessagesImported)
			var events int
			require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM mcp_event_log WHERE family='msgvault.import_message_archived'`).Scan(&events))
			assert.Equal(1, events)
		})
	}
}
