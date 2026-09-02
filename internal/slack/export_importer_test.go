package slack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportSlackdumpPersistsSlackSemantics(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	st := testutil.NewTestStore(t)

	summary, err := ImportSlackdump(context.Background(), st, slackdumpFixture, SlackdumpImportOptions{
		Me: "alice@example.com",
	})
	require.NoError(err)
	assert.Equal(5, summary.ConversationsProcessed)
	assert.Equal(6, summary.MessagesProcessed)
	assert.Equal(6, summary.MessagesAdded)
	assert.Zero(summary.MessagesUpdated)

	source, err := st.GetSourceByID(summary.SourceID)
	require.NoError(err)
	assert.Equal(sourceTypeSlackdump, source.SourceType)
	assert.Equal("T_TEST:UALICE", source.Identifier)
	var syncStatus string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT status FROM sync_runs WHERE source_id = ? ORDER BY id DESC LIMIT 1`),
		summary.SourceID).Scan(&syncStatus))
	assert.Equal("completed", syncStatus)

	assertSlackdumpConversation(t, st, "C001", "#general", "channel")
	assertSlackdumpConversation(t, st, "D001", "Bob Test", "direct_chat")
	assertSlackdumpConversation(t, st, "G002", "mpdm-alice--bob-1", "group_chat")

	var members int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM conversation_participants cp
		JOIN conversations c ON c.id = cp.conversation_id
		WHERE c.source_conversation_id = ?`), "C001").Scan(&members))
	assert.Equal(2, members)

	rootSourceID := "C001:1704067200.000001"
	replySourceID := "C001:1704153600.000002"
	root, err := st.InspectMessage(rootSourceID)
	require.NoError(err)
	assert.Equal("first day @Bob Test", root.BodyText)
	assert.Equal(1, root.RecipientCounts["from"])
	assert.Equal(1, root.RecipientCounts["mention"])
	assert.True(root.RawDataExists)

	var rootID int64
	var isFromMe bool
	require.NoError(st.DB().QueryRow(st.Rebind(
		`SELECT id, is_from_me FROM messages WHERE source_id = ? AND source_message_id = ?`),
		summary.SourceID, rootSourceID).Scan(&rootID, &isFromMe))
	assert.True(isFromMe)
	raw, err := st.GetMessageRaw(rootID)
	require.NoError(err)
	assert.JSONEq(
		`{"type":"message","ts":"1704067200.000001","thread_ts":"1704067200.000001","reply_count":1,"user":"UALICE","text":"first day <@UBOB>","files":[{"id":"F_STD","name":"what do these colored bars mean?.txt","mimetype":"text/plain","size":26}],"future_field":{"kept":true}}`,
		string(raw),
	)

	var linked int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM messages child
		JOIN messages parent ON parent.id = child.reply_to_message_id
		WHERE child.source_id = ? AND child.source_message_id = ?
		  AND parent.source_message_id = ?`),
		summary.SourceID, replySourceID, rootSourceID).Scan(&linked))
	assert.Equal(1, linked)

	var reactions int
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT COUNT(*) FROM reactions r
		JOIN messages m ON m.id = r.message_id
		WHERE m.source_id = ? AND m.source_message_id = ? AND r.reaction_value = 'eyes'`),
		summary.SourceID, replySourceID).Scan(&reactions))
	assert.Equal(2, reactions)

	results, total, err := st.SearchMessages("first day", 0, 10)
	require.NoError(err)
	assert.EqualValues(1, total)
	require.Len(results, 1)
	assert.Equal(rootSourceID, results[0].SourceMessageID)
}

func TestImportSlackdumpRequiresUniqueExportIdentity(t *testing.T) {
	tests := []struct {
		name    string
		me      string
		users   string
		wantErr string
	}{
		{name: "missing", wantErr: "identity is required"},
		{name: "unknown", me: "nobody@example.com", wantErr: "not found"},
		{
			name: "ambiguous email",
			me:   "shared@example.com",
			users: `[
				{"id":"U_ONE","team_id":"T_TEST","profile":{"email":"shared@example.com"}},
				{"id":"U_TWO","team_id":"T_TEST","profile":{"email":"SHARED@example.com"}}
			]`,
			wantErr: "matches multiple users",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := copySlackdumpFixture(t)
			if tt.users != "" {
				require.NoError(t, os.WriteFile(filepath.Join(root, "users.json"), []byte(tt.users), 0o644))
			}

			_, err := ImportSlackdump(context.Background(), testutil.NewTestStore(t), root, SlackdumpImportOptions{Me: tt.me})
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestImportSlackdumpRecordsFailedSync(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := copySlackdumpFixture(t)
	require.NoError(os.WriteFile(
		filepath.Join(root, "general", "2024-01-02.json"),
		[]byte(`[{"type":"message"`),
		0o644,
	))
	st := testutil.NewTestStore(t)

	_, err := ImportSlackdump(context.Background(), st, root, SlackdumpImportOptions{Me: "UALICE"})
	require.Error(err)
	require.ErrorContains(err, filepath.ToSlash(filepath.Join("general", "2024-01-02.json")))

	var status, failure string
	require.NoError(st.DB().QueryRow(`
		SELECT sr.status, sr.error_message
		FROM sync_runs sr
		JOIN sources s ON s.id = sr.source_id
		WHERE s.source_type = 'slackdump' AND s.identifier = 'T_TEST:UALICE'
		ORDER BY sr.id DESC LIMIT 1`).Scan(&status, &failure))
	assert.Equal("failed", status)
	assert.Contains(failure, "2024-01-02.json")
}

func TestImportSlackdumpLimitDoesNotReadPastCap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := copySlackdumpFixture(t)
	require.NoError(os.WriteFile(
		filepath.Join(root, "general", "2024-01-02.json"),
		[]byte(`[{"type":"message"`),
		0o644,
	))
	st := testutil.NewTestStore(t)

	summary, err := ImportSlackdump(context.Background(), st, root, SlackdumpImportOptions{
		Me:    "UALICE",
		Limit: 1,
	})
	require.NoError(err)
	assert.Equal(5, summary.MessagesProcessed)

	rootMessage, err := st.InspectMessage("C001:1704067200.000001")
	require.NoError(err)
	assert.Equal("first day @Bob Test", rootMessage.BodyText)
	_, err = st.InspectMessage("C001:1704153600.000002")
	require.Error(err)
}

func TestImportSlackdumpMarksMissingConversationRosterUnknown(t *testing.T) {
	require := require.New(t)

	root := copySlackdumpFixture(t)
	require.NoError(os.WriteFile(
		filepath.Join(root, "mpims.json"),
		[]byte(`[{"id":"G002","name":"mpdm-alice--bob-1","is_group":true,"is_mpim":true,"is_private":true,"is_member":true}]`),
		0o644,
	))
	st := testutil.NewTestStore(t)

	_, err := ImportSlackdump(context.Background(), st, root, SlackdumpImportOptions{Me: "UALICE"})
	require.NoError(err)

	var metadata string
	require.NoError(st.DB().QueryRow(`
		SELECT COALESCE(CAST(metadata AS TEXT), '') FROM conversations
		WHERE source_conversation_id = 'G002'`).Scan(&metadata))
	assert.JSONEq(t, `{"member_count_unknown":true}`, metadata)
}

func TestImportSlackdumpUnknownRosterFailsParticipantPolicyClosed(t *testing.T) {
	tests := []struct {
		name    string
		catalog string
	}{
		{
			name:    "missing members field",
			catalog: `[{"id":"C001","name":"general","is_channel":true,"is_member":true}]`,
		},
		{
			name:    "empty members list",
			catalog: `[{"id":"C001","name":"general","is_channel":true,"is_member":true,"members":[]}]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			root := copySlackdumpFixture(t)
			require.NoError(os.WriteFile(filepath.Join(root, "channels.json"), []byte(tt.catalog), 0o644))
			st := testutil.NewTestStore(t)

			summary, err := ImportSlackdump(context.Background(), st, root, SlackdumpImportOptions{
				Me:             "UALICE",
				AttachmentsDir: t.TempDir(),
				MediaPolicy: attachmentpolicy.Policy{
					Scope: attachmentpolicy.ScopeAll, MaxParticipants: 1, MaxBytes: 100 << 20,
				},
			})
			require.NoError(err)
			assert.Positive(summary.AttachmentsSkipped)

			var state, reason string
			require.NoError(st.DB().QueryRow(`
				SELECT attachment_state, attachment_skip_reason FROM attachments
				WHERE source_attachment_id = 'slack:F_STD'`).Scan(&state, &reason))
			assert.Equal(string(attachmentpolicy.StateSkipped), state)
			assert.Equal(string(attachmentpolicy.SkipParticipantThreshold), reason)
		})
	}
}

func TestImportSlackdumpUsesCatalogMemberCountWithoutRoster(t *testing.T) {
	require := require.New(t)

	root := copySlackdumpFixture(t)
	require.NoError(os.WriteFile(
		filepath.Join(root, "channels.json"),
		[]byte(`[{"id":"C001","name":"general","is_channel":true,"num_members":7}]`),
		0o644,
	))
	st := testutil.NewTestStore(t)

	_, err := ImportSlackdump(context.Background(), st, root, SlackdumpImportOptions{Me: "UALICE"})
	require.NoError(err)

	var metadata string
	require.NoError(st.DB().QueryRow(`
		SELECT COALESCE(CAST(metadata AS TEXT), '') FROM conversations
		WHERE source_conversation_id = 'C001'`).Scan(&metadata))
	assert.JSONEq(t, `{"member_count":7}`, metadata)
}

func TestImportSlackdumpStoresDirectoryAndZIPAttachments(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
	}{
		{name: "directory", path: func(t *testing.T) string {
			t.Helper()
			return slackdumpFixture
		}},
		{name: "ZIP", path: func(t *testing.T) string {
			t.Helper()
			return zipSlackdumpFixtureReverseOrder(t, slackdumpFixture)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			st := testutil.NewTestStore(t)
			attachmentsDir := t.TempDir()

			summary, err := ImportSlackdump(context.Background(), st, tt.path(t), SlackdumpImportOptions{
				Me:             "UALICE",
				AttachmentsDir: attachmentsDir,
			})
			require.NoError(err)
			assert.Equal(2, summary.AttachmentsDownloaded)
			assert.Equal(1, summary.AttachmentsMissing)
			assert.Zero(summary.AttachmentsPending, "offline files must not leave live API retry debt")

			assertSlackdumpStoredAttachment(t, st, attachmentsDir, "slack:F_STD", "standard attachment bytes\n")
			assertSlackdumpStoredAttachment(t, st, attachmentsDir, "slack:F_MM", "mattermost attachment bytes\n")

			var contentHash, mediaType, state string
			require.NoError(st.DB().QueryRow(`
				SELECT COALESCE(content_hash, ''), COALESCE(media_type, ''), COALESCE(attachment_state, '')
				FROM attachments WHERE source_attachment_id = 'slack:F_MISSING'`).
				Scan(&contentHash, &mediaType, &state))
			assert.Empty(contentHash)
			assert.Equal("link", mediaType)
			assert.Empty(state)
		})
	}
}

func TestImportSlackdumpStoresPresentEmptyAttachment(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := copySlackdumpFixture(t)
	messagePath := filepath.Join(root, "general", "2024-01-01.json")
	content, err := os.ReadFile(messagePath)
	require.NoError(err)
	content = []byte(strings.Replace(
		string(content),
		`"size":26}`,
		`"size":26},{"id":"F_EMPTY","name":"empty.txt","mimetype":"text/plain","size":0}`,
		1,
	))
	require.NoError(os.WriteFile(messagePath, content, 0o644))
	require.NoError(os.WriteFile(
		filepath.Join(root, "general", "attachments", "F_EMPTY-empty.txt"),
		[]byte{},
		0o600,
	))
	attachmentsDir := t.TempDir()
	st := testutil.NewTestStore(t)

	summary, err := ImportSlackdump(context.Background(), st, root, SlackdumpImportOptions{
		Me:             "UALICE",
		AttachmentsDir: attachmentsDir,
	})
	require.NoError(err)
	assert.Equal(3, summary.AttachmentsDownloaded)
	assert.Equal(1, summary.AttachmentsMissing)

	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	var storagePath, contentHash, state string
	var size int
	require.NoError(st.DB().QueryRow(`
		SELECT storage_path, content_hash, attachment_state, size
		FROM attachments WHERE source_attachment_id = 'slack:F_EMPTY'`).
		Scan(&storagePath, &contentHash, &state, &size))
	assert.Equal(filepath.ToSlash(filepath.Join(emptySHA256[:2], emptySHA256)), storagePath)
	assert.Equal(emptySHA256, contentHash)
	assert.Equal("stored", state)
	assert.Zero(size)

	info, err := os.Stat(filepath.Join(attachmentsDir, filepath.FromSlash(storagePath)))
	require.NoError(err)
	assert.Zero(info.Size())
}

func TestImportSlackdumpAppliesMediaPolicyForExportTeam(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := testutil.NewTestStore(t)
	var resolvedTeamID string

	summary, err := ImportSlackdump(context.Background(), st, slackdumpFixture, SlackdumpImportOptions{
		Me:             "UALICE",
		AttachmentsDir: t.TempDir(),
		MediaPolicyForTeam: func(teamID string) attachmentpolicy.Policy {
			resolvedTeamID = teamID
			return attachmentpolicy.Policy{
				Scope:    attachmentpolicy.ScopeNone,
				MaxBytes: 100 << 20,
			}
		},
	})
	require.NoError(err)
	assert.Equal("T_TEST", resolvedTeamID)
	assert.Equal(3, summary.AttachmentsSkipped)
	assert.Zero(summary.AttachmentsMissing, "policy must be evaluated before reading export bytes")
	var skipped int
	require.NoError(st.DB().QueryRow(`
		SELECT COUNT(*) FROM attachments
		WHERE attachment_state = 'skipped' AND attachment_skip_reason = 'policy_scope'`).Scan(&skipped))
	assert.Equal(3, skipped)
	var storagePath string
	require.NoError(st.DB().QueryRow(`
		SELECT storage_path FROM attachments WHERE source_attachment_id = 'slack:F_MM'`).Scan(&storagePath))
	assert.Equal("https://files.slack.com/files-pri/T_TEST-F_MM/quarterly-report", storagePath)
}

func TestSlackdumpMetadataAttachmentUsesCapturedHTTPURLFallbacks(t *testing.T) {
	tests := []struct {
		name string
		file File
		want string
	}{
		{
			name: "permalink",
			file: File{Permalink: "https://example.com/permalink", URLPrivate: "https://example.com/private"},
			want: "https://example.com/permalink",
		},
		{
			name: "private URL",
			file: File{URLPrivate: "https://files.slack.com/private"},
			want: "https://files.slack.com/private",
		},
		{
			name: "private download URL",
			file: File{URLPrivateDownload: "http://files.slack.com/download"},
			want: "http://files.slack.com/download",
		},
		{
			name: "non HTTP URL",
			file: File{Permalink: "javascript:alert(1)", URLPrivate: "file:///tmp/private"},
			want: "slackdump:missing:F_TEST",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := slackdumpMetadataAttachment(&tt.file, "slackdump:missing:F_TEST")
			assert.Equal(t, tt.want, ref.StoragePath)
		})
	}
}

func TestImportSlackdumpRerunUpdatesWithoutDuplicates(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	root := copySlackdumpFixture(t)
	st := testutil.NewTestStore(t)
	attachmentsDir := t.TempDir()
	opts := SlackdumpImportOptions{Me: "UALICE", AttachmentsDir: attachmentsDir}

	first, err := ImportSlackdump(context.Background(), st, root, opts)
	require.NoError(err)
	require.Equal(6, first.MessagesAdded)

	messagePath := filepath.Join(root, "general", "2024-01-02.json")
	content, err := os.ReadFile(messagePath)
	require.NoError(err)
	content = []byte(strings.ReplaceAll(string(content), "second day reply", "updated second day reply"))
	require.NoError(os.WriteFile(messagePath, content, 0o644))
	firstDayPath := filepath.Join(root, "general", "2024-01-01.json")
	firstDay, err := os.ReadFile(firstDayPath)
	require.NoError(err)
	firstDay = []byte(strings.Replace(
		string(firstDay),
		`,"files":[{"id":"F_STD","name":"what do these colored bars mean?.txt","mimetype":"text/plain","size":26}]`,
		"",
		1,
	))
	require.NoError(os.WriteFile(firstDayPath, firstDay, 0o644))

	second, err := ImportSlackdump(context.Background(), st, root, opts)
	require.NoError(err)
	assert.Zero(second.MessagesAdded)
	assert.Equal(6, second.MessagesUpdated)
	assert.Zero(second.AttachmentsDownloaded, "stored content must be reused on rerun")

	var messages, reactions, attachments int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE message_type = 'slack'`).Scan(&messages))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM attachments`).Scan(&attachments))
	assert.Equal(6, messages)
	assert.Equal(2, reactions)
	assert.Equal(3, attachments)
	var hasAttachments bool
	var attachmentCount int
	require.NoError(st.DB().QueryRow(`
		SELECT has_attachments, attachment_count FROM messages
		WHERE source_message_id = 'C001:1704067200.000001'`).
		Scan(&hasAttachments, &attachmentCount))
	assert.True(hasAttachments, "retained attachment rows must remain visible after a file is omitted")
	assert.Equal(1, attachmentCount)
	body, err := st.InspectBodyText("C001:1704153600.000002")
	require.NoError(err)
	assert.Equal("updated second day reply", body)
}

func assertSlackdumpStoredAttachment(
	t *testing.T,
	st *store.Store,
	attachmentsDir string,
	sourceAttachmentID string,
	want string,
) {
	t.Helper()

	var storagePath, contentHash, state string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT storage_path, content_hash, attachment_state
		FROM attachments WHERE source_attachment_id = ?`), sourceAttachmentID).
		Scan(&storagePath, &contentHash, &state))
	assert.NotEmpty(t, contentHash)
	assert.Equal(t, "stored", state)
	content, err := os.ReadFile(filepath.Join(attachmentsDir, filepath.FromSlash(storagePath)))
	require.NoError(t, err)
	assert.Equal(t, []byte(want), content)
}

func assertSlackdumpConversation(t *testing.T, st *store.Store, sourceConversationID, wantTitle, wantType string) {
	t.Helper()

	var title, conversationType string
	require.NoError(t, st.DB().QueryRow(st.Rebind(`
		SELECT title, conversation_type FROM conversations
		WHERE source_conversation_id = ?`), sourceConversationID).Scan(&title, &conversationType))
	assert.Equal(t, wantTitle, title)
	assert.Equal(t, wantType, conversationType)
}
