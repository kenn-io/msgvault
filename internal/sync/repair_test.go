package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	testemail "go.kenn.io/msgvault/internal/testutil/email"
)

func TestRepairMessageResolvesExactTargetAndSourceScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	sourceA := env.CreateSource(t)
	sourceB, err := env.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	internalA := seedRepairRow(t, env.Store, sourceA.ID, "123", "thread-a", "A")
	internalB := seedRepairRow(t, env.Store, sourceB.ID, "123", "thread-b", "B")

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "123"})
	require.ErrorContains(err, "ambiguous")
	assert.Empty(env.Mock.GetMessageCalls)

	env.Mock.Messages["123"] = repairRaw("123", "thread-a", "repaired", "body", nil)
	result, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{
		Reference: "123",
		SourceID:  sourceA.ID,
	})
	require.NoError(err)
	assert.Equal(internalA, result.InternalID)
	assert.Equal(sourceA.ID, result.SourceID)
	assert.Equal("123", result.SourceMessageID)
	assert.Equal("repaired", result.Subject)
	assert.Equal([]string{"123"}, env.Mock.GetMessageCalls)
	assert.NotEqual(internalB, result.InternalID)
}

func TestRepairMessageLeavesSentAttributionToIdentityDiscovery(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-sent", "thread-a", "old")
	raw := repairRaw("gmail-sent", "thread-a", "repaired", "repaired body", nil)
	raw.LabelIDs = []string{"SENT"}
	env.Mock.Messages["gmail-sent"] = raw

	_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{
		Reference: "gmail-sent",
		SourceID:  source.ID,
	})
	require.NoError(err)
	var sourceIsFromMe, identityIsFromMe bool
	require.NoError(env.Store.DB().QueryRow(`
		SELECT source_is_from_me, identity_is_from_me
		FROM messages WHERE id = ?`, internalID).Scan(&sourceIsFromMe, &identityIsFromMe))
	assert.False(t, sourceIsFromMe,
		"Gmail ingest never writes source-native attribution; repair must match sync")
	assert.False(t, identityIsFromMe,
		"identity attribution comes from confirmed identities, not the SENT label")
}

func TestRepairMessageRejectsAmbiguousNumericInterpretations(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internal := seedRepairRow(t, env.Store, source.ID, "provider-a", "thread-a", "A")
	seedRepairRow(t, env.Store, source.ID, strconv.FormatInt(internal, 10), "thread-b", "B")

	_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{
		Reference: strconv.FormatInt(internal, 10),
		SourceID:  source.ID,
	})
	require.ErrorContains(t, err, "ambiguous")
	assert.Empty(t, env.Mock.GetMessageCalls)
}

func TestRepairMessageRejectsNonGmailAndInvalidSourceScope(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	imapSource, err := env.Store.GetOrCreateSource("imap", "imap@example.com")
	require.NoError(err)
	internal := seedRepairRow(t, env.Store, imapSource.ID, "imap:1", "thread", "subject")

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: strconv.FormatInt(internal, 10)})
	require.ErrorContains(err, "not gmail")
	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "imap:1", SourceID: -1})
	require.ErrorContains(err, "source ID must be positive")
	assert.Empty(t, env.Mock.GetMessageCalls)
}

func TestRepairMessageRejectsAuthenticatedMailboxMismatchBeforeFetch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source, err := env.Store.GetOrCreateSource("gmail", "mailbox-b@example.com")
	require.NoError(err)
	internalID := seedRepairRow(t, env.Store, source.ID, "shared-gmail-id", "thread-b", "mailbox B")
	before := readRepairSnapshot(t, env.Store, internalID)
	env.Mock.Profile.EmailAddress = "mailbox-a@example.com"
	env.Mock.Messages["shared-gmail-id"] = repairRaw(
		"shared-gmail-id", "thread-a", "mailbox A", "wrong mailbox body", nil,
	)

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{
		Reference: "shared-gmail-id",
		SourceID:  source.ID,
	})
	require.ErrorContains(err, "authenticated Gmail account")
	assert.Equal(1, env.Mock.ProfileCalls)
	assert.Empty(env.Mock.GetMessageCalls)
	assert.Zero(env.Mock.LabelsCalls)
	assert.Equal(before, readRepairSnapshot(t, env.Store, internalID))
}

func TestRepairMessageRejectsUndescribedProviderLabelWithoutErasingLabels(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	labelID, err := env.Store.EnsureLabel(source.ID, "OLD", "Old label", "user")
	require.NoError(err)
	_, err = env.Store.DB().Exec(`INSERT INTO message_labels (message_id, label_id) VALUES (?, ?)`, internalID, labelID)
	require.NoError(err)
	before := readRepairSnapshot(t, env.Store, internalID)
	raw := repairRaw("gmail-a", "thread-a", "new", "new body", nil)
	raw.LabelIDs = []string{"UNDESCRIBED"}
	env.Mock.Messages["gmail-a"] = raw
	env.Mock.Labels = []*gmail.Label{{ID: "INBOX", Name: "Inbox", Type: "system"}}

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-a", SourceID: source.ID})
	require.ErrorContains(err, "label \"UNDESCRIBED\" is missing from provider catalog")
	assert.Equal(t, before, readRepairSnapshot(t, env.Store, internalID))
}

func TestRepairMessageProviderMIMESourcePartCollisionRollsBack(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	raw := repairRaw("gmail-a", "thread-a", "new", "new body", []byte("MIME bytes"))
	parsed, err := mime.Parse(raw.Raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 1)
	require.NotEmpty(parsed.Attachments[0].ContentHash)
	require.NoError(env.Store.UpsertAttachmentRecord(t.Context(), internalID, store.AttachmentWrite{
		Filename: "provider.bin", MIMEType: "application/octet-stream",
		StoragePath: "provider.bin", ContentHash: "provider-hash", Size: 8,
		SourceAttachmentID: "provider:1", SourcePartKey: parsed.Attachments[0].PartKey,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit,
	}))
	require.NoError(env.Store.RecomputeMessageAttachmentStats(internalID))
	before := readRepairSnapshot(t, env.Store, internalID)
	env.Mock.Messages["gmail-a"] = raw
	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = attachmentsDir })

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-a", SourceID: source.ID})
	require.ErrorContains(err, "provider-owned attachment source-part collision")
	assert.Equal(t, before, readRepairSnapshot(t, env.Store, internalID))
	assert.NoFileExists(t, filepath.Join(
		attachmentsDir,
		parsed.Attachments[0].ContentHash[:2],
		parsed.Attachments[0].ContentHash,
	), "failed repair must remove its newly published attachment blob")
}

func TestRepairMessageStoreFailurePreservesPreexistingMIMEBlob(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	raw := repairRaw("gmail-a", "thread-a", "new", "new body", []byte("MIME bytes"))
	parsed, err := mime.Parse(raw.Raw)
	require.NoError(err)
	require.Len(parsed.Attachments, 1)
	require.NoError(env.Store.UpsertAttachmentRecord(t.Context(), internalID, store.AttachmentWrite{
		Filename: "provider.bin", MIMEType: "application/octet-stream",
		StoragePath: "provider.bin", ContentHash: "provider-hash", Size: 8,
		SourceAttachmentID: "provider:1", SourcePartKey: parsed.Attachments[0].PartKey,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceProviderExplicit,
	}))
	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	receipt, err := export.StoreAttachmentFileDurable(attachmentsDir, &parsed.Attachments[0])
	require.NoError(err)
	require.True(receipt.Created)
	blobPath := filepath.Join(attachmentsDir, filepath.FromSlash(receipt.StoragePath))
	require.FileExists(blobPath)
	env.Mock.Messages["gmail-a"] = raw
	env.SetOptions(t, func(options *Options) { options.AttachmentsDir = attachmentsDir })

	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-a", SourceID: source.ID})
	require.ErrorContains(err, "provider-owned attachment source-part collision")
	assert.FileExists(t, blobPath, "failed repair must preserve a pre-existing deduplicated blob")
}

type cancelingLabelsAPI struct {
	*gmail.MockAPI

	cancel context.CancelFunc
}

func (a *cancelingLabelsAPI) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	labels, err := a.MockAPI.ListLabels(ctx)
	a.cancel()
	return labels, err
}

func TestRepairMessageChecksCancellationBeforeAttachmentPublication(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	before := readRepairSnapshot(t, env.Store, internalID)
	env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-a", "new", "new body", []byte("attachment"))
	ctx, cancel := context.WithCancel(t.Context())
	client := &cancelingLabelsAPI{MockAPI: env.Mock, cancel: cancel}
	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.Syncer = New(client, env.Store, &Options{AttachmentsDir: attachmentsDir})

	_, err := env.Syncer.RepairMessage(ctx, RepairRequest{Reference: "gmail-a", SourceID: source.ID})
	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(attachmentsDir)
	require.ErrorIs(t, statErr, os.ErrNotExist)
	assert.Equal(t, before, readRepairSnapshot(t, env.Store, internalID))
}

func TestRepairMessagePreStoreFailuresLeaveArchiveUnchanged(t *testing.T) {
	tests := []struct {
		name      string
		configure func(t *testing.T, env *TestEnv, source *store.Source, internalID int64)
		wantError string
	}{
		{
			name: "provider 404",
			configure: func(_ *testing.T, env *TestEnv, _ *store.Source, _ int64) {
				env.Mock.GetMessageError["gmail-a"] = &gmail.NotFoundError{Path: "/messages/gmail-a"}
			},
			wantError: "fetch Gmail message",
		},
		{
			name: "returned ID mismatch",
			configure: func(_ *testing.T, env *TestEnv, _ *store.Source, _ int64) {
				env.Mock.Messages["gmail-a"] = repairRaw("wrong-id", "thread-a", "new", "new", nil)
			},
			wantError: "returned message ID",
		},
		{
			name: "strict MIME parse",
			configure: func(_ *testing.T, env *TestEnv, _ *store.Source, _ int64) {
				env.Mock.Messages["gmail-a"] = &gmail.RawMessage{ID: "gmail-a", Raw: []byte("not a MIME message\x00")}
			},
			wantError: "strictly parse",
		},
		{
			name: "durable attachment publication",
			configure: func(t *testing.T, env *TestEnv, _ *store.Source, _ int64) {
				t.Helper()
				blocked := filepath.Join(env.TmpDir, "not-a-directory")
				require.NoError(t, os.WriteFile(blocked, []byte("file"), 0o600))
				env.Syncer = New(env.Mock, env.Store, &Options{AttachmentsDir: blocked})
				env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-a", "new", "new", []byte("attachment"))
			},
			wantError: "publish attachment",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			source := env.CreateSource(t)
			internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
			siblingID := seedRepairRow(t, env.Store, source.ID, "gmail-b", "thread-b", "sibling")
			before := readRepairSnapshot(t, env.Store, internalID)
			siblingBefore := readRepairSnapshot(t, env.Store, siblingID)
			tc.configure(t, env, source, internalID)

			_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-a", SourceID: source.ID})
			require.ErrorContains(t, err, tc.wantError)
			assert.Equal(t, before, readRepairSnapshot(t, env.Store, internalID))
			assert.Equal(t, siblingBefore, readRepairSnapshot(t, env.Store, siblingID))
		})
	}
}

type identityRaceAPI struct {
	*gmail.MockAPI

	change func() error
}

func (a *identityRaceAPI) ListLabels(ctx context.Context) ([]*gmail.Label, error) {
	if err := a.change(); err != nil {
		return nil, err
	}
	return a.MockAPI.ListLabels(ctx)
}

func TestRepairMessageIdentityRaceLeavesArchiveRowsUnchanged(t *testing.T) {
	assert := assert.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-a", "new", "new body", nil)
	racing := &identityRaceAPI{MockAPI: env.Mock, change: func() error {
		_, err := env.Store.DB().Exec(`UPDATE messages SET source_message_id = 'raced' WHERE id = ?`, internalID)
		return err
	}}
	env.Syncer = New(racing, env.Store, nil)

	_, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: "gmail-a", SourceID: source.ID})
	require.ErrorContains(t, err, "identity guard mismatch")
	snapshot := readRepairSnapshot(t, env.Store, internalID)
	assert.Equal("raced", snapshot.SourceMessageID)
	assert.Equal("old", snapshot.Subject)
	assert.Contains(snapshot.Body, "old body")
}

func TestRepairMessagePreservesConversationParticipants(t *testing.T) {
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	internalID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "old")
	var conversationID int64
	require.NoError(env.Store.DB().QueryRow(
		`SELECT conversation_id FROM messages WHERE id = ?`, internalID,
	).Scan(&conversationID))
	participants, err := env.Store.EnsureParticipantsBatch([]mime.Address{
		{Email: "thread-member@example.com", Domain: "example.com"},
	})
	require.NoError(err)
	require.NoError(env.Store.EnsureConversationParticipant(
		conversationID, participants["thread-member@example.com"], "member",
	))

	env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-a", "repaired", "repaired body", nil)
	_, err = env.Syncer.RepairMessage(t.Context(), RepairRequest{
		Reference: "gmail-a", SourceID: source.ID,
	})
	require.NoError(err)

	var count int
	require.NoError(env.Store.DB().QueryRow(
		`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id = ?`,
		conversationID,
	).Scan(&count))
	assert.Equal(t, 1, count, "repair without a membership snapshot must preserve thread membership")
}

func TestRepairMessageReplacesOnlyTargetSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	env := newTestEnv(t)
	source := env.CreateSource(t)
	targetID := seedRepairRow(t, env.Store, source.ID, "gmail-a", "thread-a", "crossed")
	siblingID := seedRepairRow(t, env.Store, source.ID, "gmail-b", "thread-b", "sibling")
	siblingBefore := readRepairSnapshot(t, env.Store, siblingID)
	require.NoError(env.Store.UpsertMessageRaw(targetID, []byte(siblingBefore.Raw)))
	_, err := env.Store.DB().Exec(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`, siblingBefore.Body, targetID)
	require.NoError(err)
	attachmentsDir := filepath.Join(env.TmpDir, "attachments")
	env.Syncer = New(env.Mock, env.Store, &Options{AttachmentsDir: attachmentsDir})
	env.Mock.Labels = []*gmail.Label{{ID: "INBOX", Name: "Inbox", Type: "system"}}
	env.Mock.Messages["gmail-a"] = repairRaw("gmail-a", "thread-repaired", "repaired", "repaired body", []byte("fresh attachment"))

	result, err := env.Syncer.RepairMessage(t.Context(), RepairRequest{Reference: strconv.FormatInt(targetID, 10)})
	require.NoError(err)
	assert.Equal(targetID, result.InternalID)
	target := readRepairSnapshot(t, env.Store, targetID)
	assert.Equal("repaired", target.Subject)
	assert.Contains(target.Body, "repaired body")
	assert.Equal("gmail-a", target.SourceMessageID)
	assert.Equal(siblingBefore, readRepairSnapshot(t, env.Store, siblingID))
	assert.Equal([]string{"gmail-a"}, env.Mock.GetMessageCalls)

	var attachmentCount int
	require.NoError(env.Store.DB().QueryRow(`SELECT COUNT(*) FROM attachments WHERE message_id = ?`, targetID).Scan(&attachmentCount))
	assert.Equal(1, attachmentCount)
}

type repairSnapshot struct {
	SourceMessageID string
	Subject         string
	Body            string
	Raw             string
	Conversation    string
	Recipients      []string
	Labels          []string
	Attachments     []string
}

func readRepairSnapshot(t *testing.T, st *store.Store, id int64) repairSnapshot {
	t.Helper()
	var got repairSnapshot
	var conversationID int64
	var conversationType, conversationSourceID, conversationTitle string
	require.NoError(t, st.DB().QueryRow(`
		SELECT m.source_message_id, COALESCE(m.subject, ''), COALESCE(mb.body_text, ''),
		       c.id, c.conversation_type, COALESCE(c.source_conversation_id, ''), COALESCE(c.title, '')
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id
		LEFT JOIN message_bodies mb ON mb.message_id = m.id
		WHERE m.id = ?`, id).Scan(
		&got.SourceMessageID, &got.Subject, &got.Body,
		&conversationID, &conversationType, &conversationSourceID, &conversationTitle,
	))
	got.Conversation = fmt.Sprintf("%d|%s|%s|%s", conversationID, conversationType, conversationSourceID, conversationTitle)
	raw, err := st.GetMessageRaw(id)
	require.NoError(t, err)
	got.Raw = string(raw)
	recipients, err := st.DB().Query(`
		SELECT recipient_type, participant_id, COALESCE(display_name, ''), COALESCE(email_address, '')
		FROM message_recipients WHERE message_id = ?
		ORDER BY recipient_type, participant_id, email_address`, id)
	require.NoError(t, err)
	defer func() { _ = recipients.Close() }()
	for recipients.Next() {
		var recipientType, displayName, emailAddress string
		var participantID int64
		require.NoError(t, recipients.Scan(&recipientType, &participantID, &displayName, &emailAddress))
		got.Recipients = append(got.Recipients, fmt.Sprintf("%s|%d|%s|%s", recipientType, participantID, displayName, emailAddress))
	}
	require.NoError(t, recipients.Err())
	labels, err := st.DB().Query(`
		SELECT l.id, COALESCE(l.source_label_id, ''), l.name, COALESCE(l.label_type, ''), COALESCE(l.system_role, '')
		FROM message_labels ml JOIN labels l ON l.id = ml.label_id
		WHERE ml.message_id = ? ORDER BY l.id`, id)
	require.NoError(t, err)
	defer func() { _ = labels.Close() }()
	for labels.Next() {
		var labelID int64
		var sourceLabelID, name, labelType, systemRole string
		require.NoError(t, labels.Scan(&labelID, &sourceLabelID, &name, &labelType, &systemRole))
		got.Labels = append(got.Labels, fmt.Sprintf("%d|%s|%s|%s|%s", labelID, sourceLabelID, name, labelType, systemRole))
	}
	require.NoError(t, labels.Err())
	attachments, err := st.DB().Query(`
		SELECT id, COALESCE(filename, ''), COALESCE(content_hash, ''), storage_path,
		       COALESCE(source_attachment_id, ''), attachment_role, role_source, COALESCE(source_part_key, '')
		FROM attachments WHERE message_id = ? ORDER BY id`, id)
	require.NoError(t, err)
	defer func() { _ = attachments.Close() }()
	for attachments.Next() {
		var attachmentID int64
		var filename, contentHash, storagePath, sourceAttachmentID, role, roleSource, sourcePartKey string
		require.NoError(t, attachments.Scan(
			&attachmentID, &filename, &contentHash, &storagePath, &sourceAttachmentID, &role, &roleSource, &sourcePartKey,
		))
		got.Attachments = append(got.Attachments, fmt.Sprintf(
			"%d|%s|%s|%s|%s|%s|%s|%s",
			attachmentID, filename, contentHash, storagePath, sourceAttachmentID, role, roleSource, sourcePartKey,
		))
	}
	require.NoError(t, attachments.Err())
	return got
}

func seedRepairRow(t *testing.T, st *store.Store, sourceID int64, sourceMessageID, threadID, subject string) int64 {
	t.Helper()
	convID, err := st.EnsureConversation(sourceID, threadID, subject)
	require.NoError(t, err)
	raw := testemail.NewMessage().
		From("sender@example.com").
		To("recipient@example.com").
		Subject(subject).
		Header("Message-ID", "<"+sourceMessageID+"@example.com>").
		Body(subject + " body").Bytes()
	parsed, err := mime.Parse(raw)
	require.NoError(t, err)
	participants, err := st.EnsureParticipantsBatch([]mime.Address{
		{Email: "sender@example.com", Domain: "example.com"},
		{Email: "recipient@example.com", Domain: "example.com"},
	})
	require.NoError(t, err)
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID: convID, SourceID: sourceID, SourceMessageID: sourceMessageID,
			MessageType: store.MessageTypeEmail, RFC822MessageID: sql.NullString{String: "<" + sourceMessageID + "@example.com>", Valid: true},
			Subject: sql.NullString{String: subject, Valid: true}, SenderID: sql.NullInt64{Int64: participants["sender@example.com"], Valid: true},
		},
		BodyText: sql.NullString{String: parsed.GetBodyText(), Valid: parsed.GetBodyText() != ""},
		RawMIME:  raw,
		Recipients: []store.RecipientSet{
			{Type: "from", ParticipantIDs: []int64{participants["sender@example.com"]}, EmailAddresses: []string{"sender@example.com"}},
			{Type: "to", ParticipantIDs: []int64{participants["recipient@example.com"]}, EmailAddresses: []string{"recipient@example.com"}},
		},
	})
	require.NoError(t, err)
	return id
}

func repairRaw(id, threadID, subject, body string, attachment []byte) *gmail.RawMessage {
	builder := testemail.NewMessage().
		From("new-sender@example.com").
		To("new-recipient@example.com").
		Subject(subject).
		Header("Message-ID", "<"+id+"@example.com>").
		Body(body)
	if attachment != nil {
		builder.WithAttachment("fresh.bin", "application/octet-stream", attachment)
	}
	return &gmail.RawMessage{ID: id, ThreadID: threadID, LabelIDs: []string{"INBOX"}, Raw: builder.Bytes()}
}
