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

	emersionimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	msgmime "go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/query"
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

func TestDraftForwardConversationKeyUsesProviderReceipt(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := newDraftReplyFixture(t)
	parent, err := fixture.store.GetMessageContext(t.Context(), fixture.parentID)
	requirements.NoError(err)
	target := draftReplyTarget{source: fixture.source, parent: parent, parentSource: fixture.source, forward: true}
	build := func(messageID string) imaplib.ReplyDraft {
		draft, buildErr := imaplib.BuildForward(imaplib.ForwardOptions{
			From: "alice@example.test", To: []string{"recipient@example.test"}, Subject: "Question", Body: "forward",
		}, time.Now(), messageID)
		requirements.NoError(buildErr)
		return draft
	}
	receipt1 := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 4, UID: 1}
	receipt2 := store.IMAPDraftReceipt{SourceID: fixture.source.ID, Mailbox: "Drafts", UIDValidity: 4, UID: 2}
	data1 := draftReplyPersistData(target, build("forward-1@example.test"), receipt1, "forward-1@example.test", []int64{1, 2})
	data2 := draftReplyPersistData(target, build("forward-2@example.test"), receipt2, "forward-2@example.test", []int64{1, 2})
	requirements.NotNil(data1.Conversation)
	requirements.NotNil(data2.Conversation)
	assertions.NotEqual(data1.Conversation.SourceConversationID, data2.Conversation.SourceConversationID)
	assertions.Contains(data1.Conversation.SourceConversationID, store.IMAPDraftSourceMessageID(receipt1))
	assertions.Contains(data2.Conversation.SourceConversationID, store.IMAPDraftSourceMessageID(receipt2))
}

func TestDraftForwardRejectsUnsupportedDestinationBeforeParentRead(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		destination string
		from        string
		policy      bool
		want        string
	}{
		{name: "Gmail destination", destination: "gmail", from: "gmail@example.test", policy: true, want: "draft_disabled"},
		{name: "disabled IMAP source", destination: "imap", from: testutil.IMAPTestUsername, policy: false, want: "draft_disabled"},
		{name: "disallowed sender", destination: "imap", from: "other@example.test", policy: true, want: "invalid_from"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			requirements := require.New(t)
			fixture := newDraftReplyFixture(t)
			adapter := fixture.grantedAdapter()
			var sourceID int64
			if scenario.destination == "gmail" {
				source, err := fixture.store.GetOrCreateSource("gmail", "gmail@example.test")
				requirements.NoError(err)
				requirements.NoError(fixture.store.AddAccountIdentity(source.ID, source.Identifier, "manual"))
				sourceID = source.ID
			} else {
				sourceID = fixture.source.ID
				adapter.draftPolicy = []config.IMAPDraftSource{{SourceID: sourceID, Enabled: scenario.policy, Mailbox: "Drafts"}}
			}
			err := adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: []string{
				"draft-forward", strconv.FormatInt(fixture.parentID, 10),
				"--source-id", strconv.FormatInt(sourceID, 10), "--from", scenario.from, "--to", "recipient@example.test",
			}}, nil)
			requirements.Error(err)
			requirements.Equal(scenario.want, err.Error())
		})
	}
}

func TestDraftForwardLeavesArchivedRemoteImageAsExternalLink(t *testing.T) {
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	const imageURL = "https://images.example.test/cat.png"
	raw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>Parent body</p><img src=\"" + imageURL + "\">\r\n")
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, raw))
	key := "remote-image:" + strings.Repeat("a", 64)
	requirements.NoError(fixture.store.UpsertRemoteImageAttachment(t.Context(), fixture.parentID, store.AttachmentWrite{
		Filename: "remote-image.png", MIMEType: "image/png", StoragePath: "missing/cache.png",
		ContentHash: strings.Repeat("b", 64), Size: 42, SourceAttachmentID: key,
		SourcePartKey: key, ContentID: key, MediaType: "image", Role: store.AttachmentRoleInline,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StateStored,
	}))
	adapter := fixture.grantedAdapter()
	var events []api.CLIRunEvent
	err := adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-forward", strconv.FormatInt(fixture.parentID, 10),
		"--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "recipient@example.test", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	requirements.Equal(draftReplyStatusCreated, result.Status)
	draft, err := fixture.store.GetIMAPDraft(result.DraftID)
	requirements.NoError(err)
	_, fetchedRaw := fetchDraftMailboxMessage(t, fixture.config, draft.CurrentReceipt)
	fetched, err := msgmime.Parse(fetchedRaw)
	requirements.NoError(err)
	requirements.Contains(fetched.BodyHTML, imageURL)
	requirements.Empty(fetched.Attachments)
	refs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), draft.CurrentMessageID)
	requirements.NoError(err)
	requirements.Empty(refs)
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
	dataDir := t.TempDir()
	attachmentDir := filepath.Join(dataDir, "attachments")
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
	remoteKey := "remote-image:" + hash
	requirements.NoError(fixture.store.UpsertRemoteImageAttachment(t.Context(), fixture.parentID, store.AttachmentWrite{
		Filename: "cached-image.png", MIMEType: "image/png", StoragePath: filepath.ToSlash(relativePath),
		ContentHash: hash, Size: int64(len(attachmentContent)), SourceAttachmentID: remoteKey,
		SourcePartKey: remoteKey, ContentID: remoteKey, MediaType: "image", Role: store.AttachmentRoleInline,
		RoleSource: store.AttachmentRoleSourceImporterSemantics, State: attachmentpolicy.StateStored,
	}))
	parentRefs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), fixture.parentID)
	requirements.NoError(err)
	requirements.Len(parentRefs, 2)
	maintenance, err := newAttachmentMaintenance(fixture.store, attachmentDir, slog.New(slog.DiscardHandler), true)
	requirements.NoError(err)
	t.Cleanup(func() { _ = maintenance.close() })
	adapter := fixture.grantedAdapter()
	adapter.attachmentMaintenance = maintenance
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{
		Config: &config.Config{
			HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: dataDir},
			Server: config.ServerConfig{APIKey: "owner-test-key"},
		},
		Store: adapter, Engine: query.NewEngine(fixture.store.DB(), fixture.store.IsPostgreSQL()), BlobStore: maintenance.blob,
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
	draftRefs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), result.MessageID)
	requirements.NoError(err)
	requirements.Len(draftRefs, 1)
	assertions.Empty(draftRefs[0].SourceAttachmentID)
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

	getBody, err := json.Marshal(map[string]any{"args": []string{"draft-get", result.DraftID, "--json"}})
	requirements.NoError(err)
	getRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(getBody))
	requirements.NoError(err)
	getRequest.Header.Set("Content-Type", "application/json")
	getRequest.Header.Set("X-Api-Key", "owner-test-key")
	getResponse, err := http.DefaultClient.Do(getRequest)
	requirements.NoError(err)
	defer func() { _ = getResponse.Body.Close() }()
	requirements.Equal(http.StatusOK, getResponse.StatusCode)
	var getEvents []api.CLIRunEvent
	getScanner := bufio.NewScanner(getResponse.Body)
	for getScanner.Scan() {
		var event api.CLIRunEvent
		requirements.NoError(json.Unmarshal(getScanner.Bytes(), &event))
		getEvents = append(getEvents, event)
	}
	requirements.NoError(getScanner.Err())
	requirements.Len(getEvents, 2)
	var localGet draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(getEvents[0].Data), &localGet))
	assertions.Equal(result.DraftID, localGet.DraftID)
	assertions.Equal(string(storedRaw), localGet.RawMIME)

	detailRequest, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/messages/"+strconv.FormatInt(result.MessageID, 10), nil)
	requirements.NoError(err)
	detailRequest.Header.Set("X-Api-Key", "owner-test-key")
	detailResponse, err := http.DefaultClient.Do(detailRequest)
	requirements.NoError(err)
	defer func() { _ = detailResponse.Body.Close() }()
	requirements.Equal(http.StatusOK, detailResponse.StatusCode)
	var detail api.MessageDetail
	requirements.NoError(json.NewDecoder(detailResponse.Body).Decode(&detail))
	requirements.Len(detail.Attachments, 1)
	assertions.Equal(hash, detail.Attachments[0].ContentHash)
	contentRequest, err := http.NewRequest(http.MethodGet,
		server.URL+"/api/v1/attachments/"+hash+"/content", nil)
	requirements.NoError(err)
	contentRequest.Header.Set("X-Api-Key", "owner-test-key")
	contentResponse, err := http.DefaultClient.Do(contentRequest)
	requirements.NoError(err)
	defer func() { _ = contentResponse.Body.Close() }()
	requirements.Equal(http.StatusOK, contentResponse.StatusCode)
	apiAttachment, err := io.ReadAll(contentResponse.Body)
	requirements.NoError(err)
	assertions.Equal(attachmentContent, apiAttachment)

	editBody, err := json.Marshal(map[string]any{"args": []string{
		api.CLIRunDraftEditCommand, result.DraftID, "--revision", "1", "--body", "edited forward note", "--json",
	}})
	requirements.NoError(err)
	editRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/cli/run", bytes.NewReader(editBody))
	requirements.NoError(err)
	editRequest.Header.Set("Content-Type", "application/json")
	editRequest.Header.Set("X-Api-Key", "owner-test-key")
	editResponse, err := http.DefaultClient.Do(editRequest)
	requirements.NoError(err)
	defer func() { _ = editResponse.Body.Close() }()
	requirements.Equal(http.StatusOK, editResponse.StatusCode)
	var editEvents []api.CLIRunEvent
	editScanner := bufio.NewScanner(editResponse.Body)
	for editScanner.Scan() {
		var event api.CLIRunEvent
		requirements.NoError(json.Unmarshal(editScanner.Bytes(), &event))
		editEvents = append(editEvents, event)
	}
	requirements.NoError(editScanner.Err())
	requirements.Len(editEvents, 2)
	var editOutput draftLifecycleOutput
	requirements.NoError(json.Unmarshal([]byte(editEvents[0].Data), &editOutput))
	assertions.Equal(int64(2), editOutput.Revision)
	editedDraft, err := fixture.store.GetIMAPDraftContext(t.Context(), result.DraftID)
	requirements.NoError(err)
	assertions.Equal(int64(2), editedDraft.Revision)
	editedRaw, err := fixture.store.GetMessageRawContext(t.Context(), editedDraft.CurrentMessageID)
	requirements.NoError(err)
	edited, err := msgmime.Parse(editedRaw)
	requirements.NoError(err)
	assertions.Contains(edited.BodyText, "edited forward note")
	requirements.Len(edited.Attachments, 1)
	assertions.Equal(attachmentContent, edited.Attachments[0].Content)
	editedRefs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), editedDraft.CurrentMessageID)
	requirements.NoError(err)
	requirements.Len(editedRefs, 1)
	assertions.Equal(hash, editedRefs[0].ContentHash)
}

func TestDraftForwardCreatesDraftWithDistinctAttachmentPartKeys(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	firstContent := []byte("first archived bytes")
	secondContent := []byte("second archived bytes")
	parentRaw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"Message-ID: <parent@example.com>\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=two-files\r\n\r\n" +
		"--two-files\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Parent body\r\n" +
		"--two-files\r\n" +
		"Content-Type: text/plain; name=first.txt\r\n" +
		"Content-Disposition: attachment; filename=first.txt\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(firstContent) + "\r\n" +
		"--two-files\r\n" +
		"Content-Type: application/octet-stream; name=second.bin\r\n" +
		"Content-Disposition: attachment; filename=second.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(secondContent) + "\r\n" +
		"--two-files--\r\n")
	parsedParent, err := msgmime.Parse(parentRaw)
	requirements.NoError(err)
	requirements.Len(parsedParent.Attachments, 2)
	requirements.NotEmpty(parsedParent.Attachments[0].PartKey)
	requirements.NotEmpty(parsedParent.Attachments[1].PartKey)
	requirements.NotEqual(parsedParent.Attachments[0].PartKey, parsedParent.Attachments[1].PartKey)
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, parentRaw))
	attachmentDir := t.TempDir()
	hashes := make([]string, 2)
	contents := [][]byte{firstContent, secondContent}
	for i, content := range contents {
		digest := sha256.Sum256(content)
		hashes[i] = hex.EncodeToString(digest[:])
		relativePath := filepath.Join(hashes[i][:2], hashes[i])
		requirements.NoError(os.MkdirAll(filepath.Dir(filepath.Join(attachmentDir, relativePath)), 0o700))
		requirements.NoError(os.WriteFile(filepath.Join(attachmentDir, relativePath), content, 0o600))
		role, roleSource := store.AttachmentRoleFromMIME(
			parsedParent.Attachments[i].Disposition, parsedParent.Attachments[i].IsInline, parsedParent.Attachments[i].ContentID,
		)
		requirements.NoError(fixture.store.UpsertAttachmentRecord(t.Context(), fixture.parentID, store.AttachmentWrite{
			Filename: parsedParent.Attachments[i].Filename, MIMEType: parsedParent.Attachments[i].ContentType,
			StoragePath: filepath.ToSlash(relativePath), ContentHash: hashes[i], Size: int64(len(content)),
			Role: role, RoleSource: roleSource, SourcePartKey: parsedParent.Attachments[i].PartKey,
			State: attachmentpolicy.StateStored,
		}))
	}
	maintenance, err := newAttachmentMaintenance(fixture.store, attachmentDir, slog.New(slog.DiscardHandler), true)
	requirements.NoError(err)
	t.Cleanup(func() { _ = maintenance.close() })
	adapter := fixture.grantedAdapter()
	adapter.attachmentMaintenance = maintenance
	var events []api.CLIRunEvent
	err = adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-forward", strconv.FormatInt(fixture.parentID, 10),
		"--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "recipient@example.test", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal(draftReplyStatusCreated, result.Status)
	draft, err := fixture.store.GetIMAPDraft(result.DraftID)
	requirements.NoError(err)
	_, fetchedRaw := fetchDraftMailboxMessage(t, fixture.config, draft.CurrentReceipt)
	fetched, err := msgmime.Parse(fetchedRaw)
	requirements.NoError(err)
	requirements.Len(fetched.Attachments, 2)
	assertions.Equal(firstContent, fetched.Attachments[0].Content)
	assertions.Equal(secondContent, fetched.Attachments[1].Content)
	refs, err := fixture.store.MessageAttachmentRefsContext(t.Context(), draft.CurrentMessageID)
	requirements.NoError(err)
	requirements.Len(refs, 2)
	assertions.Equal(hashes[0], refs[0].ContentHash)
	assertions.Equal(hashes[1], refs[1].ContentHash)
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
	requirements.ErrorAs(err, &coded)
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

func TestDraftForwardRefusesCorruptStoredAttachmentBeforeAppend(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	content := []byte("archived attachment")
	corruptContent := bytes.Repeat([]byte("x"), len(content))
	raw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=corrupt-boundary\r\n\r\n" +
		"--corrupt-boundary\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Parent body\r\n" +
		"--corrupt-boundary\r\n" +
		"Content-Type: text/plain; name=report.txt\r\n" +
		"Content-Disposition: attachment; filename=report.txt\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(content) + "\r\n" +
		"--corrupt-boundary--\r\n")
	parsed, err := msgmime.Parse(raw)
	requirements.NoError(err)
	requirements.Len(parsed.Attachments, 1)
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, raw))
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	attachmentDir := t.TempDir()
	relativePath := filepath.Join(hash[:2], hash)
	requirements.NoError(os.MkdirAll(filepath.Dir(filepath.Join(attachmentDir, relativePath)), 0o700))
	requirements.NoError(os.WriteFile(filepath.Join(attachmentDir, relativePath), corruptContent, 0o600))
	role, roleSource := store.AttachmentRoleFromMIME(
		parsed.Attachments[0].Disposition, parsed.Attachments[0].IsInline, parsed.Attachments[0].ContentID,
	)
	requirements.NoError(fixture.store.UpsertAttachmentRecord(t.Context(), fixture.parentID, store.AttachmentWrite{
		Filename: parsed.Attachments[0].Filename, MIMEType: parsed.Attachments[0].ContentType,
		StoragePath: filepath.ToSlash(relativePath), ContentHash: hash, Size: int64(len(content)),
		Role: role, RoleSource: roleSource, SourcePartKey: parsed.Attachments[0].PartKey,
		State: attachmentpolicy.StateStored,
	}))
	maintenance, err := newAttachmentMaintenance(fixture.store, attachmentDir, nil, true)
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
	requirements.Len(events, 1)
	assertions.Equal(cliStreamStderr, events[0].Type)
	var output draftForwardPreflightOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &output))
	requirements.Len(output.Problems, 1)
	assertions.Equal("report.txt", output.Problems[0].Filename)
	assertions.Equal("unreadable_file", output.Problems[0].Reason)
	assertions.NotEmpty(output.Problems[0].Detail)
	var drafts int
	requirements.NoError(fixture.store.DB().QueryRow(fixture.store.Rebind(
		"SELECT COUNT(*) FROM imap_drafts",
	)).Scan(&drafts))
	assertions.Zero(drafts)
	client, err := imapclient.DialInsecure(fixture.config.Addr(), nil)
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	requirements.NoError(client.Login(testutil.IMAPTestUsername, testutil.IMAPTestPassword).Wait())
	status, err := client.Status("Drafts", &emersionimap.StatusOptions{NumMessages: true}).Wait()
	requirements.NoError(err)
	requirements.NotNil(status.NumMessages)
	assertions.Zero(*status.NumMessages)
}

func TestDraftForwardReportsLegacyAttachmentStateAndTextPreflight(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	emptyContent := []byte{}
	skippedContent := []byte("skipped bytes")
	emptyDigest := sha256.Sum256(emptyContent)
	skippedDigest := sha256.Sum256(skippedContent)
	emptyHash := hex.EncodeToString(emptyDigest[:])
	skippedHash := hex.EncodeToString(skippedDigest[:])
	raw := []byte("From: Sender <sender@example.com>\r\n" +
		"To: " + testutil.IMAPTestUsername + "\r\n" +
		"Subject: Question\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=state-boundary\r\n\r\n" +
		"--state-boundary\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Parent body\r\n" +
		"--state-boundary\r\n" +
		"Content-Type: application/octet-stream; name=empty.bin\r\n" +
		"Content-Disposition: attachment; filename=empty.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n\r\n" +
		"--state-boundary\r\n" +
		"Content-Type: application/octet-stream; name=skipped.bin\r\n" +
		"Content-Disposition: attachment; filename=skipped.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString(skippedContent) + "\r\n" +
		"--state-boundary--\r\n")
	parsed, err := msgmime.Parse(raw)
	requirements.NoError(err)
	requirements.Len(parsed.Attachments, 2)
	requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, raw))
	attachmentDir := t.TempDir()
	for _, hash := range []string{emptyHash} {
		relative := filepath.Join(hash[:2], hash)
		requirements.NoError(os.MkdirAll(filepath.Dir(filepath.Join(attachmentDir, relative)), 0o700))
		requirements.NoError(os.WriteFile(filepath.Join(attachmentDir, relative), emptyContent, 0o600))
	}
	for i, item := range []struct {
		hash string
		size int64
	}{
		{hash: emptyHash, size: 0},
		{hash: skippedHash, size: int64(len(skippedContent))},
	} {
		role, roleSource := store.AttachmentRoleFromMIME(
			parsed.Attachments[i].Disposition, parsed.Attachments[i].IsInline, parsed.Attachments[i].ContentID,
		)
		write := store.AttachmentWrite{
			Filename: parsed.Attachments[i].Filename, MIMEType: parsed.Attachments[i].ContentType,
			StoragePath: filepath.ToSlash(filepath.Join(item.hash[:2], item.hash)), ContentHash: item.hash,
			Size: item.size, Role: role, RoleSource: roleSource, SourcePartKey: parsed.Attachments[i].PartKey,
		}
		if i == 1 {
			write.SkipReason = attachmentpolicy.SkipReason("policy-test")
		}
		requirements.NoError(fixture.store.UpsertAttachmentRecord(t.Context(), fixture.parentID, write))
	}
	maintenance, err := newAttachmentMaintenance(fixture.store, attachmentDir, nil, true)
	requirements.NoError(err)
	t.Cleanup(func() { _ = maintenance.close() })
	adapter := fixture.grantedAdapter()
	adapter.attachmentMaintenance = maintenance
	args := []string{
		"draft-forward", strconv.FormatInt(fixture.parentID, 10), "--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername, "--to", "recipient@example.test",
	}
	var textEvents []api.CLIRunEvent
	err = adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: args}, func(event api.CLIRunEvent) error {
		textEvents = append(textEvents, event)
		return nil
	})
	requirements.Error(err)
	assertions.Equal("attachment_preflight_failed", err.Error())
	requirements.Len(textEvents, 1)
	assertions.Equal(cliStreamStderr, textEvents[0].Type)
	assertions.Contains(textEvents[0].Data, "draft-forward refused before APPEND")
	assertions.Contains(textEvents[0].Data, "skipped.bin")
	assertions.NotContains(textEvents[0].Data, "empty.bin")
	var jsonEvents []api.CLIRunEvent
	err = adapter.runCLIForwardDraft(t.Context(), api.CLIRunRequest{Args: append(args, "--json")}, func(event api.CLIRunEvent) error {
		jsonEvents = append(jsonEvents, event)
		return nil
	})
	requirements.Error(err)
	requirements.Len(jsonEvents, 1)
	var output draftForwardPreflightOutput
	requirements.NoError(json.Unmarshal([]byte(jsonEvents[0].Data), &output))
	requirements.Len(output.Problems, 1)
	assertions.Equal("skipped.bin", output.Problems[0].Filename)
	assertions.Equal("attachment_skipped", output.Problems[0].Reason)
	assertions.Equal("policy-test", output.Problems[0].Detail)
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
	requirements.NotEmpty(draft.DraftID)
	_, err = fixture.maintenance.pack(t.Context(), 0)
	requirements.NoError(err)
	_, err = fixture.store.DB().Exec(fixture.store.Rebind("DELETE FROM messages WHERE id = ?"), fixture.messageID)
	requirements.NoError(err)
	_, err = fixture.maintenance.repack(t.Context(), 0)
	requirements.NoError(err)
	reader, _, err := fixture.maintenance.blob.OpenStream(t.Context(), hash)
	requirements.NoError(err)
	got, err := io.ReadAll(reader)
	requirements.NoError(err)
	requirements.NoError(reader.Close())
	assertions.Equal(content, got)
}
