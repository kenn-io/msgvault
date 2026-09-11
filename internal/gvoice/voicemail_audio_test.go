package gvoice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	withAudioHTML = "Test User - Voicemail - 2024-01-02T03_04_05Z.html"
	withAudioMP3  = "Test User - Voicemail - 2024-01-02T03_04_05Z.mp3"
)

type voicemailFixture struct {
	fixture string
	html    string
	audio   string
	bytes   []byte
}

type voicemailAttachment struct {
	filename           sql.NullString
	mimeType           sql.NullString
	storagePath        string
	contentHash        sql.NullString
	size               int64
	mediaType          sql.NullString
	role               sql.NullString
	roleSource         sql.NullString
	sourcePartKey      sql.NullString
	sourceAttachmentID sql.NullString
	state              sql.NullString
	skipReason         sql.NullString
}

func TestImportVoicemailStoresAudio(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("synthetic voicemail audio one")
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   withAudioMP3,
		bytes:   audioData,
	})
	st := testutil.NewTestStore(t)
	attachments := t.TempDir()
	importVoice(t, st, voice, attachments)

	rows := attachmentRows(t, st)
	require.Len(rows, 1)
	assert.Equal(attachmentpolicy.StateStored, attachmentpolicy.DownloadState(rows[0].state.String))
	assert.Equal("audio/mpeg", rows[0].mimeType.String)
	assert.Equal("audio", rows[0].mediaType.String)
	assert.Equal("standalone", rows[0].role.String)
	assert.Equal("importer_semantics", rows[0].roleSource.String)
	assert.Equal("gvoice:voicemail:audio", rows[0].sourcePartKey.String)
	assert.False(rows[0].sourceAttachmentID.Valid)
	hash := sha256.Sum256(audioData)
	contentHash := hex.EncodeToString(hash[:])
	assert.Equal(contentHash, rows[0].contentHash.String)
	assert.Equal(path.Join(contentHash[:2], contentHash), rows[0].storagePath)
	assert.Equal(int64(len(audioData)), rows[0].size)
	assert.True(messageHasAttachments(t, st, sourceMessageID(t, "with-audio.html")))
}

func TestImportVoicemailRetainsBytes(t *testing.T) {
	require := require.New(t)
	audioData := []byte("synthetic voicemail audio bytes")
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   withAudioMP3,
		bytes:   audioData,
	})
	attachments := t.TempDir()
	st := testutil.NewTestStore(t)
	importVoice(t, st, voice, attachments)
	row := attachmentRows(t, st)[0]

	got, err := os.ReadFile(filepath.Join(attachments, filepath.FromSlash(row.storagePath)))
	require.NoError(err)
	require.Equal(audioData, got)
	require.Equal(int64(len(audioData)), row.size)
}

func TestImportVoicemailExactAssociation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	firstBytes := []byte("first voicemail bytes")
	secondBytes := []byte("second voicemail bytes")
	voice := writeVoice(t,
		voicemailFixture{
			fixture: "with-audio.html",
			html:    withAudioHTML,
			audio:   withAudioMP3,
			bytes:   firstBytes,
		},
		voicemailFixture{
			fixture: "leading-space-src.html",
			html:    "Test User - Voicemail - 2024-01-03T03_04_05Z.html",
			audio:   " Test User - Voicemail - 2024-01-03T03_04_05Z.mp3",
			bytes:   secondBytes,
		},
	)
	st := testutil.NewTestStore(t)
	importVoice(t, st, voice, t.TempDir())

	rows := attachmentRows(t, st)
	require.Len(rows, 2)
	firstID := sourceMessageID(t, "with-audio.html")
	secondID := sourceMessageID(t, "leading-space-src.html")
	var firstMessage, secondMessage int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), firstID).Scan(&firstMessage))
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), secondID).Scan(&secondMessage))
	assert.NotEqual(rows[0].contentHash.String, rows[1].contentHash.String)
	assert.Equal(firstMessage, rows[0].messageID)
	assert.Equal(secondMessage, rows[1].messageID)
}

func TestImportVoicemailNoAudioReferenceWritesGap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("same stem must remain unread")
	htmlName := "Test User - Voicemail - 2024-01-04T03_04_05Z.html"
	stemName := "Test User - Voicemail - 2024-01-04T03_04_05Z.mp3"
	voice := writeVoice(t, voicemailFixture{
		fixture: "no-audio.html",
		html:    htmlName,
		audio:   stemName,
		bytes:   audioData,
	})
	st := testutil.NewTestStore(t)
	attachments := t.TempDir()
	importVoice(t, st, voice, attachments)

	rows := attachmentRows(t, st)
	require.Len(rows, 1)
	assert.Equal("failed", rows[0].state.String)
	assert.Equal("fetch_failure", rows[0].skipReason.String)
	assert.Empty(rows[0].filename.String)
	assert.Empty(rows[0].mimeType.String)
	assert.Empty(rows[0].storagePath)
	assert.Empty(rows[0].contentHash.String)
	assert.Zero(rows[0].size)
	assert.True(messageHasAttachments(t, st, sourceMessageID(t, "no-audio.html")))
	hash := sha256.Sum256(audioData)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE content_hash = ?`), hex.EncodeToString(hash[:])).Scan(&count))
	assert.Zero(count)
	entries, err := os.ReadDir(attachments)
	require.NoError(err)
	assert.Empty(entries)
}

func TestImportVoicemailMissingFileWritesFailedRow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	name := "missing-recording.mp3"
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   "",
		bytes:   nil,
	})
	data, err := os.ReadFile(filepath.Join("testdata", "voicemail", "with-audio.html"))
	require.NoError(err)
	data = bytes.Replace(data, []byte(withAudioMP3), []byte(name), 1)
	require.NoError(os.WriteFile(filepath.Join(voice, "Calls", withAudioHTML), data, 0o600))
	st := testutil.NewTestStore(t)
	importVoice(t, st, voice, t.TempDir())

	rows := attachmentRows(t, st)
	require.Len(rows, 1)
	assert.Equal("failed", rows[0].state.String)
	assert.Equal("fetch_failure", rows[0].skipReason.String)
	assert.Equal(name, rows[0].filename.String)
	assert.Equal("audio/mpeg", rows[0].mimeType.String)
	assert.Empty(rows[0].storagePath)
}

func TestImportVoicemailRejectsUnsupportedFormat(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	name := "recording.m4a"
	audioData := []byte("unsupported format must not be read")
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   "",
	})
	data, err := os.ReadFile(filepath.Join("testdata", "voicemail", "with-audio.html"))
	require.NoError(err)
	data = bytes.Replace(data, []byte(withAudioMP3), []byte(name), 1)
	require.NoError(os.WriteFile(filepath.Join(voice, "Calls", withAudioHTML), data, 0o600))
	require.NoError(os.WriteFile(filepath.Join(voice, "Calls", name), audioData, 0o600))
	st := testutil.NewTestStore(t)
	attachments := t.TempDir()
	importVoice(t, st, voice, attachments)

	rows := attachmentRows(t, st)
	require.Len(rows, 1)
	assert.Equal(name, rows[0].filename.String)
	assert.Empty(rows[0].mimeType.String)
	assert.Equal("failed", rows[0].state.String)
	assert.Equal("fetch_failure", rows[0].skipReason.String)
	entries, err := os.ReadDir(attachments)
	require.NoError(err)
	assert.Empty(entries)
}

func TestImportVoicemailRejectsEscapingSrc(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("must not be read")
	htmlName := "Test User - Voicemail - 2024-01-08T03_04_05Z.html"
	voice := writeVoice(t, voicemailFixture{
		fixture: "escaping-src.html",
		html:    htmlName,
	})
	escapePath := filepath.Join(voice, "escape.mp3")
	require.NoError(os.WriteFile(escapePath, audioData, 0o600))
	st := testutil.NewTestStore(t)
	attachments := t.TempDir()
	importVoice(t, st, voice, attachments)

	rows := attachmentRows(t, st)
	require.Len(rows, 1)
	assert.Equal("../escape.mp3", rows[0].filename.String)
	assert.Equal("failed", rows[0].state.String)
	assert.Equal("fetch_failure", rows[0].skipReason.String)
	hash := sha256.Sum256(audioData)
	var count int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM attachments WHERE content_hash = ?`), hex.EncodeToString(hash[:])).Scan(&count))
	assert.Zero(count)
	entries, err := os.ReadDir(attachments)
	require.NoError(err)
	assert.Empty(entries)
}

func TestImportVoicemailLeadingSpaceSrc(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("leading space filename bytes")
	voice := writeVoice(t, voicemailFixture{
		fixture: "leading-space-src.html",
		html:    "Test User - Voicemail - 2024-01-03T03_04_05Z.html",
		audio:   " Test User - Voicemail - 2024-01-03T03_04_05Z.mp3",
		bytes:   audioData,
	})
	st := testutil.NewTestStore(t)
	attachments := t.TempDir()
	importVoice(t, st, voice, attachments)
	row := attachmentRows(t, st)[0]
	assert.Equal(" Test User - Voicemail - 2024-01-03T03_04_05Z.mp3", row.filename.String)
	assert.Equal("stored", row.state.String)
	assert.NotEmpty(row.contentHash.String)
	require.Equal(audioData, readStoredAttachment(t, attachments, row))
}

func TestImportVoicemailDeterministicReimport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("reimport bytes")
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   withAudioMP3,
		bytes:   audioData,
	})
	attachments := t.TempDir()
	st := testutil.NewTestStore(t)
	client := newVoiceClient(t, voice, attachments)
	_, err := clientImport(t, client, st)
	require.NoError(err)
	first := attachmentRows(t, st)[0]
	_, err = clientImport(t, client, st)
	require.NoError(err)
	rows := attachmentRows(t, st)
	require.Len(rows, 1)
	assert.Equal(first.contentHash, rows[0].contentHash)
	assert.Equal("gvoice:voicemail:audio", rows[0].sourcePartKey.String)
}

func TestImportVoicemailAttachmentRevision(t *testing.T) {
	for _, tt := range []struct {
		name              string
		preimport         bool
		withAudio         bool
		wantRevisionDelta int64
	}{
		// New messages advance the revision only for their voicemail label.
		{name: "new message", wantRevisionDelta: 1},
		{name: "existing message without attachment", preimport: true, wantRevisionDelta: 1},
		{name: "unchanged attachment", preimport: true, withAudio: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			voice := writeVoice(t, voicemailFixture{
				fixture: "with-audio.html", html: withAudioHTML,
				audio: withAudioMP3, bytes: []byte("voicemail bytes"),
			})
			st := testutil.NewTestStore(t)
			attachments := t.TempDir()
			if tt.preimport {
				initialDir := ""
				if tt.withAudio {
					initialDir = attachments
				}
				importVoice(t, st, voice, initialDir)
			}

			before, err := st.DerivedDataRevision()
			require.NoError(err)
			importVoice(t, st, voice, attachments)
			after, err := st.DerivedDataRevision()
			require.NoError(err)
			assert.Equal(tt.wantRevisionDelta, after-before)
			rows := attachmentRows(t, st)
			require.Len(rows, 1)
			assert.Equal("stored", rows[0].state.String)
		})
	}
}

func TestImportVoicemailRollsBackAudioWhenRevisionFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html", html: withAudioHTML,
		audio: withAudioMP3, bytes: []byte("original audio"),
	})
	st := testutil.NewSQLiteTestStore(t)
	attachments := t.TempDir()
	importVoice(t, st, voice, attachments)
	before := attachmentRows(t, st)[0]
	revision, err := st.DerivedDataRevision()
	require.NoError(err)
	updated := []byte("replacement audio")
	require.NoError(os.WriteFile(filepath.Join(voice, "Calls", withAudioMP3), updated, 0o600))
	// Reject just the revision update to exercise the atomic write contract.
	_, err = st.DB().Exec(`CREATE TRIGGER reject_audio_revision
		BEFORE UPDATE ON archive_metadata WHEN NEW.key = 'derived_data_revision'
		BEGIN SELECT RAISE(ABORT, 'revision write rejected'); END`)
	require.NoError(err)
	importVoice(t, st, voice, attachments)
	assert.Equal(before, attachmentRows(t, st)[0])
	messageID := messageIDBySource(t, st, sourceMessageID(t, "with-audio.html"))
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT has_attachments, attachment_count FROM messages WHERE id = ?
	`), messageID).Scan(&hasAttachments, &attachmentCount))
	assert.True(hasAttachments)
	assert.Equal(1, attachmentCount)
	unchangedRevision, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Equal(revision, unchangedRevision)

	_, err = st.DB().Exec(`DROP TRIGGER reject_audio_revision`)
	require.NoError(err)
	importVoice(t, st, voice, attachments)
	assert.Equal(updated, readStoredAttachment(t, attachments, attachmentRows(t, st)[0]))
	updatedRevision, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Greater(updatedRevision, revision)
}

func TestImportVoicemailRollsBackAudioWhenStatsFail(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html", html: withAudioHTML,
		audio: withAudioMP3, bytes: []byte("voicemail bytes"),
	})
	st := testutil.NewSQLiteTestStore(t)
	attachments := t.TempDir()
	_, err := st.DB().Exec(`CREATE TRIGGER reject_audio_stats
		BEFORE UPDATE ON messages WHEN NEW.attachment_count > 0
		BEGIN SELECT RAISE(ABORT, 'stats write rejected'); END`)
	require.NoError(err)
	importVoice(t, st, voice, attachments)
	assert.Empty(attachmentRows(t, st))
	assert.False(messageHasAttachments(t, st, sourceMessageID(t, "with-audio.html")))
	before, err := st.DerivedDataRevision()
	require.NoError(err)

	_, err = st.DB().Exec(`DROP TRIGGER reject_audio_stats`)
	require.NoError(err)
	importVoice(t, st, voice, attachments)
	assert.Len(attachmentRows(t, st), 1)
	assert.True(messageHasAttachments(t, st, sourceMessageID(t, "with-audio.html")))
	after, err := st.DerivedDataRevision()
	require.NoError(err)
	assert.Greater(after, before)
}

func TestImportNonVoicemailCallIgnoresAudio(t *testing.T) {
	require := require.New(t)
	voice := writeVoice(t, voicemailFixture{
		fixture: "received-with-audio.html",
		html:    "Test User - Received - 2024-01-09T03_04_05Z.html",
		audio:   "received.mp3",
		bytes:   []byte("received audio"),
	})
	st := testutil.NewTestStore(t)
	importVoice(t, st, voice, t.TempDir())
	rows := attachmentRows(t, st)
	require.Empty(rows)
}

func TestImportVoicemailWithoutAttachmentsDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("retained without configured root")
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   withAudioMP3,
		bytes:   audioData,
	})
	st := testutil.NewTestStore(t)
	attachments := t.TempDir()
	firstClient := newVoiceClient(t, voice, attachments)
	_, err := clientImport(t, firstClient, st)
	require.NoError(err)
	before := attachmentRows(t, st)[0]
	secondClient := newVoiceClient(t, voice, "")
	_, err = clientImport(t, secondClient, st)
	require.NoError(err)
	after := attachmentRows(t, st)[0]
	assert.Equal(before.storagePath, after.storagePath)
	assert.Equal(before.contentHash, after.contentHash)
	assert.Equal(before.size, after.size)
	assert.True(messageHasAttachments(t, st, sourceMessageID(t, "with-audio.html")))
}

func TestImportVoicemailPreservesMessageFields(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   withAudioMP3,
		bytes:   []byte("preserve message fields"),
	})
	raw, err := os.ReadFile(filepath.Join(voice, "Calls", withAudioHTML))
	require.NoError(err)
	st := testutil.NewTestStore(t)
	importVoice(t, st, voice, t.TempDir())
	messageID := messageIDBySource(t, st, sourceMessageID(t, "with-audio.html"))
	var messageType string
	var snippet sql.NullString
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT message_type, snippet FROM messages WHERE id = ?`), messageID).Scan(&messageType, &snippet))
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(err)
	gotRaw, err := st.GetMessageRaw(messageID)
	require.NoError(err)
	assert.Equal("google_voice_voicemail", messageType)
	assert.Equal("Voicemail from Test User (1m 23s)", snippet.String)
	assert.Equal("Voicemail from Test User (1m 23s)", body)
	assert.Equal(raw, gotRaw)

	var labelCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM message_labels ml
		JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ? AND l.name = ?
	`), messageID, "voicemail").Scan(&labelCount))
	assert.Equal(1, labelCount)
}

func TestImportVoicemailPreservesStoredOccurrenceOnFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	audioData := []byte("stored occurrence survives replay")
	voice := writeVoice(t, voicemailFixture{
		fixture: "with-audio.html",
		html:    withAudioHTML,
		audio:   withAudioMP3,
		bytes:   audioData,
	})
	attachments := t.TempDir()
	st := testutil.NewTestStore(t)
	firstClient := newVoiceClient(t, voice, attachments)
	_, err := clientImport(t, firstClient, st)
	require.NoError(err)
	before := attachmentRows(t, st)[0]
	require.NoError(os.Remove(filepath.Join(voice, "Calls", withAudioMP3)))
	secondClient := newVoiceClient(t, voice, attachments)
	_, err = clientImport(t, secondClient, st)
	require.NoError(err)
	after := attachmentRows(t, st)[0]
	assert.Equal("stored", after.state.String)
	assert.Equal(before.storagePath, after.storagePath)
	assert.Equal(before.contentHash, after.contentHash)
	assert.Equal(before.size, after.size)
	assert.True(messageHasAttachments(t, st, sourceMessageID(t, "with-audio.html")))
}

func TestImportVoicemailRespectsDateFilterAndLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	voice := writeVoice(t,
		voicemailFixture{fixture: "with-audio.html", html: withAudioHTML, audio: withAudioMP3, bytes: []byte("old")},
		voicemailFixture{fixture: "leading-space-src.html", html: "Test User - Voicemail - 2024-01-03T03_04_05Z.html", audio: " Test User - Voicemail - 2024-01-03T03_04_05Z.mp3", bytes: []byte("new")},
	)
	st := testutil.NewTestStore(t)
	client := newVoiceClient(t, voice, t.TempDir(), WithAfterDate(time.Date(2024, 1, 3, 0, 0, 0, 0, time.FixedZone("EST", -5*60*60))), WithLimit(1))
	summary, err := clientImport(t, client, st)
	require.NoError(err)
	assert.Equal(1, summary.MessagesImported)
	require.Len(attachmentRows(t, st), 1)
	assert.Equal(sourceMessageID(t, "leading-space-src.html"), onlyMessageSourceID(t, st))
}

func writeVoice(t *testing.T, fixtures ...voicemailFixture) string {
	t.Helper()
	require := require.New(t)
	voice := t.TempDir()
	calls := filepath.Join(voice, "Calls")
	require.NoError(os.MkdirAll(calls, 0o700))
	phones, err := os.ReadFile(filepath.Join("testdata", "voicemail", "phones.vcf"))
	require.NoError(err)
	require.NoError(os.WriteFile(filepath.Join(voice, "Phones.vcf"), phones, 0o600))
	for _, fixture := range fixtures {
		data, err := os.ReadFile(filepath.Join("testdata", "voicemail", fixture.fixture))
		require.NoError(err)
		require.NoError(os.WriteFile(filepath.Join(calls, fixture.html), data, 0o600))
		if fixture.audio != "" {
			require.NoError(os.WriteFile(filepath.Join(calls, fixture.audio), fixture.bytes, 0o600))
		}
	}
	return voice
}

func newVoiceClient(t *testing.T, voice, attachments string, opts ...ClientOption) *Client {
	t.Helper()
	clientOpts := append([]ClientOption{}, opts...)
	if attachments != "" {
		clientOpts = append(clientOpts, WithAttachmentsDir(attachments))
	}
	client, err := NewClient(voice, clientOpts...)
	require.NoError(t, err)
	return client
}

func clientImport(t *testing.T, client *Client, st *store.Store) (*ImportSummary, error) {
	t.Helper()
	source, err := st.GetOrCreateSource("google_voice", client.Identifier())
	require.NoError(t, err)
	return client.Import(context.Background(), st, source.ID)
}

func importVoice(t *testing.T, st *store.Store, voice, attachments string) *ImportSummary {
	t.Helper()
	summary, err := clientImport(t, newVoiceClient(t, voice, attachments), st)
	require.NoError(t, err)
	return summary
}

func attachmentRows(t *testing.T, st *store.Store) []voicemailAttachmentWithMessage {
	t.Helper()
	rows, err := st.DB().Query(st.Rebind(`
		SELECT message_id, filename, mime_type, storage_path, content_hash, size,
		       media_type, attachment_role, role_source, source_part_key,
		       source_attachment_id, attachment_state, attachment_skip_reason
		FROM attachments
		WHERE source_part_key = ?
		ORDER BY message_id
	`), voicemailAudioPartKey)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var result []voicemailAttachmentWithMessage
	for rows.Next() {
		var row voicemailAttachmentWithMessage
		require.NoError(t, rows.Scan(
			&row.messageID, &row.filename, &row.mimeType, &row.storagePath,
			&row.contentHash, &row.size, &row.mediaType, &row.role,
			&row.roleSource, &row.sourcePartKey, &row.sourceAttachmentID,
			&row.state, &row.skipReason,
		))
		result = append(result, row)
	}
	require.NoError(t, rows.Err())
	return result
}

type voicemailAttachmentWithMessage struct {
	voicemailAttachment

	messageID int64
}

func sourceMessageID(t *testing.T, fixture string) string {
	t.Helper()
	record := parseVoicemailFixture(t, fixture)
	return computeMessageID(record.CallType.String(), record.Phone, record.Timestamp.Format(time.RFC3339Nano))
}

func messageIDBySource(t *testing.T, st *store.Store, sourceMessageID string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&id))
	return id
}

func messageHasAttachments(t *testing.T, st *store.Store, sourceMessageID string) bool {
	t.Helper()
	var has bool
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT has_attachments FROM messages WHERE source_message_id = ?`), sourceMessageID).Scan(&has))
	return has
}

func onlyMessageSourceID(t *testing.T, st *store.Store) string {
	t.Helper()
	var sourceMessageID string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`SELECT source_message_id FROM messages ORDER BY id LIMIT 1`)).Scan(&sourceMessageID))
	return sourceMessageID
}

func readStoredAttachment(t *testing.T, attachments string, row voicemailAttachmentWithMessage) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(attachments, filepath.FromSlash(row.storagePath)))
	require.NoError(t, err)
	return data
}
