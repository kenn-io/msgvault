package cmd

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftForwardAuthorizationPrecedesContentIO(t *testing.T) {
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	grant := &agentgrant.Grant{
		ID:          "forward-out-of-scope",
		Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate},
		Sources:     []agentgrant.SourceRef{{ID: fixture.source.ID + 1, Type: "imap", Identifier: "other@example.test"}},
	}
	err := adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{
		Args: []string{
			"draft-forward", strconv.FormatInt(fixture.parentID, 10),
			"--source-id", strconv.FormatInt(fixture.source.ID, 10),
			"--from", testutil.IMAPTestUsername, "--to", "to@example.test",
		}, Grant: grant,
	}, nil)
	requirements.Error(err)
	requirements.Equal("not_permitted", err.Error())
	_, ok := errors.AsType[*api.CLIRunCodedError](err)
	requirements.True(ok)
}

func TestDraftForwardHTTPPublishesManagedDraft(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	attachmentContent := []byte("provider copied attachment bytes")
	parentRaw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"Message-ID: <parent@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=source-boundary\r\n\r\n" +
		"--source-boundary\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Parent body\r\n" +
		"--source-boundary\r\n" +
		"Content-Type: text/plain; name=source.txt\r\n" +
		"Content-Disposition: attachment; filename=source.txt\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(attachmentContent) + "\r\n" +
		"--source-boundary--\r\n")
	parsedParent, err := msgmime.Parse(parentRaw)
	requirements.NoError(err)
	requirements.Len(parsedParent.Attachments, 1)
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, parentRaw))
	digest := sha256.Sum256(attachmentContent)
	hash := hex.EncodeToString(digest[:])
	attachmentDir := t.TempDir()
	relativePath := filepath.Join(hash[:2], hash)
	requirements.NoError(os.MkdirAll(filepath.Dir(filepath.Join(attachmentDir, relativePath)), 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(attachmentDir, relativePath), attachmentContent, 0o600))
	role, roleSource := store.AttachmentRoleFromMIME(
		parsedParent.Attachments[0].Disposition, parsedParent.Attachments[0].IsInline, parsedParent.Attachments[0].ContentID,
	)
	requirements.NoError(fixture.store.UpsertAttachmentRecord(t.Context(), fixture.parentID, store.AttachmentWrite{
		Filename: parsedParent.Attachments[0].Filename, MIMEType: parsedParent.Attachments[0].ContentType,
		StoragePath: filepath.ToSlash(relativePath), ContentHash: hash, Size: int64(len(attachmentContent)),
		Role: role, RoleSource: roleSource, SourcePartKey: parsedParent.Attachments[0].PartKey,
		State: attachmentpolicy.StateStored,
	}))
	maintenance, err := newAttachmentMaintenance(fixture.store, attachmentDir, slog.New(slog.DiscardHandler), true)
	requirements.NoError(err)
	t.Cleanup(func() { _ = maintenance.close() })
	adapter := fixture.grantedAdapter()
	adapter.attachmentMaintenance = maintenance
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(),
			Server:  config.ServerConfig{APIKey: "owner-test-key"},
		},
		Store:  adapter,
		Logger: slog.New(slog.DiscardHandler),
	}).Router())
	t.Cleanup(server.Close)

	args := []string{
		"draft-forward", strconv.FormatInt(fixture.parentID, 10),
		"--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "to@example.test",
		"--cc", "copy@example.test", "--bcc", "hidden@example.test",
		"--body", "forward note", "--json",
	}
	body, err := json.Marshal(map[string]any{"args": args})
	requirements.NoError(err)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(body))
	requirements.NoError(err)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "owner-test-key")
	response, err := http.DefaultClient.Do(request)
	requirements.NoError(err)
	defer func() { _ = response.Body.Close() }()
	requirements.Equal(http.StatusOK, response.StatusCode)

	var events []api.CLIRunEvent
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var event api.CLIRunEvent
		requirements.NoError(json.Unmarshal(scanner.Bytes(), &event))
		events = append(events, event)
	}
	requirements.NoError(scanner.Err())
	requirements.Len(events, 2)
	requirements.Equal(cliStreamStdout, events[0].Type)
	requirements.Equal("complete", events[1].Type)

	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal(draftReplyStatusCreated, result.Status)
	assertions.Equal(fixture.source.ID, result.SourceID)
	assertions.Equal("Drafts", result.Mailbox)
	assertions.NotZero(result.UID)
	assertions.NotZero(result.UIDValidity)
	assertions.Equal(int64(1), result.Revision)

	draft, err := fixture.store.GetIMAPDraft(result.DraftID)
	requirements.NoError(err)
	assertions.Equal(result.MessageID, draft.CurrentMessageID)
	assertions.Equal(result.UID, draft.CurrentReceipt.UID)
	message, err := fixture.store.GetMessage(result.MessageID)
	requirements.NoError(err)
	assertions.Equal([]string{"to@example.test"}, message.To)
	assertions.Equal([]string{"copy@example.test"}, message.Cc)
	assertions.Equal([]string{"hidden@example.test"}, message.Bcc)
	assertions.Len(message.Attachments, 1)
	assertions.Equal(hash, message.Attachments[0].ContentHash)
	storedRaw, err := fixture.store.GetMessageRaw(result.MessageID)
	requirements.NoError(err)
	assertions.Contains(string(storedRaw), "X-Msgvault-Forward: 1")
	assertions.Contains(string(storedRaw), "forward note")
	assertions.Contains(string(storedRaw), "Parent body")
	assertions.Contains(string(storedRaw), "Bcc:")
	matches, total, err := fixture.store.SearchMessages("forward", 0, 10)
	requirements.NoError(err)
	assertions.Equal(int64(1), total)
	requirements.Len(matches, 1)
	assertions.Equal(result.MessageID, matches[0].ID)
	assertions.Contains(*fixture.refreshed, fixture.source.Identifier)

	_, fetchedRaw := fetchDraftMailboxMessage(t, fixture.config, draft.CurrentReceipt)
	assertions.Equal(storedRaw, fetchedRaw)
	fetched, err := msgmime.Parse(fetchedRaw)
	requirements.NoError(err)
	requirements.Len(fetched.Attachments, 1)
	assertions.Equal(attachmentContent, fetched.Attachments[0].Content)
	reader, _, err := maintenance.blob.OpenStream(t.Context(), hash)
	requirements.NoError(err)
	localAttachment, err := io.ReadAll(reader)
	requirements.NoError(err)
	requirements.NoError(reader.Close())
	assertions.Equal(attachmentContent, localAttachment)
}

func TestDraftForwardRefusesMissingAttachmentBeforeAppend(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	content := []byte("archived attachment")
	raw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=forward-boundary\r\n\r\n" +
		"--forward-boundary\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Parent body\r\n" +
		"--forward-boundary\r\n" +
		"Content-Type: text/plain; name=report.txt\r\n" +
		"Content-Disposition: attachment; filename=report.txt\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(content) + "\r\n" +
		"--forward-boundary--\r\n")
	parsed, err := msgmime.Parse(raw)
	requirements.NoError(err)
	requirements.Len(parsed.Attachments, 1)
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, raw))
	role, roleSource := store.AttachmentRoleFromMIME(
		parsed.Attachments[0].Disposition, parsed.Attachments[0].IsInline, parsed.Attachments[0].ContentID,
	)
	requirements.NoError(fixture.store.UpsertAttachmentRecord(t.Context(), fixture.parentID, store.AttachmentWrite{
		Filename: parsed.Attachments[0].Filename, MIMEType: parsed.Attachments[0].ContentType,
		StoragePath: "missing/" + parsed.Attachments[0].ContentHash, ContentHash: parsed.Attachments[0].ContentHash,
		Size: int64(parsed.Attachments[0].Size), Role: role, RoleSource: roleSource,
		SourcePartKey: parsed.Attachments[0].PartKey, State: attachmentpolicy.StateStored,
	}))

	maintenance, err := newAttachmentMaintenance(fixture.store, t.TempDir(), nil, true)
	requirements.NoError(err)
	t.Cleanup(func() { _ = maintenance.close() })
	adapter := fixture.grantedAdapter()
	adapter.attachmentMaintenance = maintenance
	var events []api.CLIRunEvent
	err = adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-forward", strconv.FormatInt(fixture.parentID, 10),
		"--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "to@example.test", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.Error(err)
	assertions.Equal("attachment_preflight_failed", err.Error())
	var coded *api.CLIRunCodedError
	requirements.True(errors.As(err, &coded))
	assertions.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	assertions.Contains(events[0].Data, "report.txt")
	assertions.Contains(events[0].Data, "unreadable_file")

	var drafts int
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(
		"SELECT COUNT(*) FROM imap_drafts",
	)).Scan(&drafts))
	assertions.Zero(drafts)
}

func TestDraftForwardLooseAndPackedAttachments(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newAttachmentMaintenanceFixture(t)
	content := []byte("same archived bytes")
	firstHash := fixture.addLoose(content)

	draft, err := imaplib.BuildForward(imaplib.ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Archived",
		Attachments: []imaplib.ForwardAttachment{
			{Filename: "attachment-1.bin", ContentType: "application/octet-stream", Content: content},
		},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	refs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), fixture.messageID)
	requirements.NoError(err)
	adapter := &storeAPIAdapter{attachmentMaintenance: fixture.maintenance}
	read := func() {
		attachments, problems := adapter.readForwardAttachments(t.Context(), draft.Parsed, refs)
		requirements.Empty(problems)
		requirements.Len(attachments, 1)
		assertions.Equal(content, attachments[0].Content)
	}
	read()
	_, err = fixture.maintenance.pack(t.Context(), 0)
	requirements.NoError(err)
	requirements.NotNil(fixture.packedEntry(firstHash))
	read()
}

func TestDraftForwardAttachmentAvailability(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := []byte("archived state")
	draft, err := imaplib.BuildForward(imaplib.ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Archived",
		Attachments: []imaplib.ForwardAttachment{
			{Filename: "pending.bin", ContentType: "application/octet-stream", Content: content},
			{Filename: "skipped.bin", ContentType: "application/octet-stream", Content: content},
			{Filename: "failed.bin", ContentType: "application/octet-stream", Content: content},
		},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	refs := make([]store.AttachmentRef, 0, len(draft.Parsed.Attachments))
	states := []attachmentpolicy.DownloadState{
		attachmentpolicy.StatePending, attachmentpolicy.StateSkipped, attachmentpolicy.StateFailed,
	}
	for i, part := range draft.Parsed.Attachments {
		refs = append(refs, store.AttachmentRef{
			Filename: part.Filename, ContentHash: part.ContentHash, Size: part.Size,
			State: states[i], SkipReason: attachmentpolicy.SkipReason("policy-test"),
		})
	}
	adapter := &storeAPIAdapter{}
	attachments, problems := adapter.readForwardAttachments(t.Context(), draft.Parsed, refs)
	requirements.Empty(attachments)
	requirements.Len(problems, 3)
	assertions.Equal([]string{"attachment_pending", "attachment_skipped", "attachment_failed"}, []string{
		problems[0].Reason, problems[1].Reason, problems[2].Reason,
	})
	assertions.Equal("policy-test", problems[1].Detail)
}

func TestDraftForwardEditRecoveryRetainsAttachments(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture, _ := newDraftRecoveryFixture(t)
	content := []byte("recovered attachment")
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	requirements.NoError(fixture.store.UpsertAttachmentRecord(t.Context(), fixture.draft.CurrentMessageID, store.AttachmentWrite{
		Filename: "recovered.txt", MIMEType: "text/plain", ContentHash: hash, Size: int64(len(content)), ContentID: "recovered@example.com",
		Role: store.AttachmentRoleInline, RoleSource: store.AttachmentRoleSourceMIMEDisposition,
		SourcePartKey: "mime:original", State: attachmentpolicy.StateStored,
	}))
	const note = "distinctive recovered note"
	forward, err := imaplib.BuildForward(imaplib.ForwardOptions{
		From: "alice@example.com", To: []string{"bob@example.com"}, Subject: "Original", Body: note,
		QuotedHeader: "From: Alice <alice@example.com>\r\nSubject: Original",
		QuotedText:   "distinctive recovered quote", QuotedHTML: `<p>distinctive recovered html <img src="cid:recovered@example.com"></p>`,
		Attachments: []imaplib.ForwardAttachment{{
			Filename: "recovered.txt", ContentType: "text/plain", ContentID: "recovered@example.com", IsInline: true, Content: content,
		}},
	}, time.Now(), "forward@example.com")
	requirements.NoError(err)
	_, err = fixture.store.ClaimIMAPDraftContext(t.Context(), fixture.draft.DraftID, fixture.draft.Revision, store.IMAPDraftOperationEdit, forward.Raw)
	requirements.NoError(err)
	replacement := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 2}
	requirements.NoError(fixture.store.RecordIMAPDraftOutcomeContext(t.Context(), fixture.draft.DraftID, fixture.draft.Revision, "append_uidplus", &replacement))
	pending, err := fixture.store.GetIMAPDraftContext(t.Context(), fixture.draft.DraftID)
	requirements.NoError(err)
	published, err := fixture.adapter.publishRecoveredDraftReplacement(t.Context(), pending)
	requirements.NoError(err)
	refs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), published.CurrentMessageID)
	requirements.NoError(err)
	requirements.Len(refs, 1)
	assertions.Equal(hash, refs[0].ContentHash)
	assertions.Equal("recovered.txt", refs[0].Filename)
	assertions.Equal("recovered@example.com", refs[0].ContentID)
	recoveredRaw, err := fixture.store.GetMessageRawContext(t.Context(), published.CurrentMessageID)
	requirements.NoError(err)
	assertions.Equal(forward.Raw, recoveredRaw)
	recovered, err := msgmime.Parse(recoveredRaw)
	requirements.NoError(err)
	assertions.Equal(1, strings.Count(recovered.BodyText, note))
	assertions.Equal(1, strings.Count(recovered.BodyHTML, note))
	assertions.Contains(recovered.BodyText, "distinctive recovered quote")
	assertions.Contains(recovered.BodyHTML, "distinctive recovered html")
	assertions.Equal(1, strings.Count(recovered.BodyHTML, "cid:recovered@example.com"))
}

func TestDraftForwardAttachmentsSurviveParentRemoval(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newAttachmentMaintenanceFixture(t)
	content := []byte("retained draft bytes")
	hash := fixture.addLoose(content)
	source, err := fixture.store.GetOrCreateSource("imap", "retained-draft@example.test")
	requirements.NoError(err)
	conversationID, err := fixture.store.EnsureConversation(source.ID, "retained-draft", "Retained draft")
	requirements.NoError(err)
	receipt := store.IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	draft, err := fixture.store.PersistIMAPDraftContext(t.Context(), receipt, nil, func(_ []int64) *store.MessagePersistData {
		return &store.MessagePersistData{
			Message: &store.Message{
				SourceID: source.ID, SourceMessageID: store.IMAPDraftSourceMessageID(receipt),
				MessageType: store.MessageTypeEmail, ConversationID: conversationID,
			},
			BodyText: sql.NullString{String: "forward", Valid: true},
			RawMIME:  []byte("From: test@example.com\r\nTo: recipient@example.com\r\n\r\nforward\r\n"),
			MIMEAttachmentReplacement: &[]store.AttachmentWrite{{
				Filename: "retained.txt", MIMEType: "text/plain", StoragePath: hash[:2] + "/" + hash,
				ContentHash: hash, Size: int64(len(content)), Role: store.AttachmentRoleStandalone,
				RoleSource: store.AttachmentRoleSourceMIMEDisposition, SourcePartKey: "mime:retained",
				State: attachmentpolicy.StateStored,
			}},
		}
	})
	requirements.NoError(err)
	requirements.NotZero(draft.DraftID)
	_, err = fixture.maintenance.pack(t.Context(), 0)
	requirements.NoError(err)
	_, err = fixture.store.DB().Exec(fixture.store.Rebind("DELETE FROM messages WHERE id = ?"), fixture.messageID)
	requirements.NoError(err)
	reader, _, err := fixture.maintenance.blob.OpenStream(t.Context(), hash)
	requirements.NoError(err)
	got, err := io.ReadAll(reader)
	requirements.NoError(err)
	requirements.NoError(reader.Close())
	assertions.Equal(content, got)
}
