package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/docbankmedia"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
	"go.kenn.io/msgvault/internal/testutil/katatest"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestKataInputRejectedBeforeTransport(t *testing.T) {
	largest := `{"selectors":[]}`
	largest += strings.Repeat(" ", kataevidence.MaxRequestBytes-len(largest))
	for _, tc := range []struct {
		input    string
		rejected bool
	}{
		{`{"selectors":[],"unexpected":true}`, true},
		{largest + " ", true},
		{`{} {}`, true},
		{largest, false},
	} {
		command := newKataCmd()
		command.SetArgs([]string{"evidence", "prepare"})
		command.SetIn(strings.NewReader(tc.input))
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		err := command.ExecuteContext(t.Context())
		require.Error(t, err)
		// Without a daemon configured, input that passes the reader fails opening the client.
		assert.Equal(t, tc.rejected, strings.Contains(strings.ToLower(err.Error()), "kata input"), "%q: %v", tc.input[:min(len(tc.input), 40)], err)
	}
}

// The daemon adapter, not a bare Store, must carry evidence and the retry
// marker from the CLI through to Kata.
func TestKataCreateThroughDaemonAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	id := f.CreateMessage("cli-evidence")
	_, err := f.Store.DB().Exec(f.Store.Rebind("INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)"), id, "é界🙂 send the revised budget")
	require.NoError(err)
	kataServer := httptest.NewServer(katatest.New(t).Service.Handler())
	t.Cleanup(kataServer.Close)
	cfg := &config.Config{}
	cfg.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: "example"}
	daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: f.Store}, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)

	output := runKataCommand(ctx, t, fmt.Sprintf(`{"selectors":[{"kind":"message","message_id":%d,"max_chars":1000}]}`, id), "evidence", "prepare")
	var prepared generated.KataEvidencePrepareResponse
	require.NoError(json.Unmarshal(output, &prepared))
	require.Len(prepared.Evidence, 1)
	assert.Equal("é界🙂 send the revised budget", prepared.Evidence[0].Excerpt)

	request, err := json.Marshal(generated.KataIssueCreateRequest{Title: "Send the revised budget", Evidence: []generated.Reference{prepared.Evidence[0].Reference}})
	require.NoError(err)
	var created, retried generated.KataIssueResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "cli-key", "--json"), &created))
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "cli-key", "--json"), &retried))

	// The caller names the issue with its key; the CLI never invents one.
	command := newKataCmd()
	command.SetArgs([]string{"create"})
	command.SetIn(strings.NewReader(string(request)))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.ErrorContains(command.ExecuteContext(ctx), "--idempotency-key is required")
	assert.False(created.Replayed)
	assert.True(retried.Replayed)
	assert.Equal(created.Issue.UID, retried.Issue.UID)
	assert.Equal(created.Issue.QualifiedRef+"\tSend the revised budget (already filed, "+created.Issue.Status+")\n", string(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "cli-key")))
	var citing generated.KataIssueListResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, "", "issues", "--message", strconv.FormatInt(id, 10), "--json"), &citing))
	require.Len(citing.Issues, 1)
	assert.Equal(created.Issue.UID, citing.Issues[0].UID)
	var issueContext generated.KataIssueContextResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, "", "context", created.Issue.QualifiedRef, "--json"), &issueContext))
	assert.Equal(created.Issue.QualifiedRef, issueContext.Issue.QualifiedRef)
	assert.Len(issueContext.Passages, 1)

	// The exported generated client decodes a replay like a create, and the
	// daemon client names the filed issue when a retry changes the request.
	client, err := daemonclient.New(daemonclient.Config{URL: daemon.URL, AllowInsecure: true, HTTPClient: daemon.Client()})
	require.NoError(err)
	generatedClient, err := client.GeneratedClient()
	require.NoError(err)
	replayed, err := generatedClient.CreateKataIssue(ctx, &generated.CreateKataIssueRequestOptions{
		Body: &generated.KataIssueCreateRequest{Title: "Send the revised budget", Evidence: []generated.Reference{prepared.Evidence[0].Reference}}, Header: &generated.CreateKataIssueHeaders{IdempotencyKey: "cli-key"},
	})
	require.NoError(err)
	assert.True(replayed.Replayed)
	_, err = client.CreateKataIssue(ctx, "cli-key", generated.KataIssueCreateRequest{Title: "Send the final budget", Evidence: []generated.Reference{prepared.Evidence[0].Reference}})
	conflict, ok := errors.AsType[*daemonclient.KataIssueConflictError](err)
	require.True(ok, "%v", err)
	assert.Equal(created.Issue.QualifiedRef, conflict.Issue.QualifiedRef)

	// Truncation must not add a non-tabular row to stdout.
	for i := range 10 {
		runKataCommand(ctx, t, string(request), "create", "--idempotency-key", fmt.Sprintf("cli-extra-%d", i), "--json")
	}
	command = newKataCmd()
	command.SetArgs([]string{"issues", "--message", strconv.FormatInt(id, 10)})
	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	require.NoError(command.ExecuteContext(ctx))
	rows := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	assert.Len(rows, 10)
	for _, row := range rows {
		assert.Len(strings.Split(row, "\t"), 3, row)
	}
	assert.Contains(stderr.String(), "More issues cite this source")
}

func TestKataIssuesRequiresMessage(t *testing.T) {
	command := newKataCmd()
	command.SetArgs([]string{"issues"})
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.ErrorContains(t, command.ExecuteContext(t.Context()), `required flag(s) "message" not set`)
}

func runKataCommand(ctx context.Context, t *testing.T, input string, args ...string) []byte {
	t.Helper()
	command := newKataCmd()
	command.SetArgs(args)
	command.SetIn(strings.NewReader(input))
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(io.Discard)
	require.NoError(t, command.ExecuteContext(ctx))
	return output.Bytes()
}

func TestKataCommandsRefuseAnOlderDaemon(t *testing.T) {
	var version atomic.Pointer[string]
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/health" {
			assert.Fail(t, "unexpected request", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"` + *version.Load() + `"}`))
	}))
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)
	for _, args := range [][]string{{"evidence", "prepare"}, {"create", "--idempotency-key", "key-1"}, {"link", "example#abcd"}, {"issues", "--message", "1"}, {"context", "example#abcd"}} {
		// The lookup and the context read need newer daemons than the other commands.
		version.Store(new("3.2.0"))
		switch args[0] {
		case "issues":
			version.Store(new("3.3.0"))
		case "context":
			version.Store(new("3.4.0"))
		}
		command := newKataCmd()
		command.SetArgs(args)
		command.SetIn(strings.NewReader(kataInputFor(args[0])))
		command.SetOut(io.Discard)
		command.SetErr(io.Discard)
		err := command.ExecuteContext(ctx)
		require.ErrorContains(t, err, "too old for Kata issues", args[0])
	}
	// Citing a Docbank transcript needs a daemon that reads them.
	version.Store(new("3.5.0"))
	command := newKataCmd()
	command.SetArgs([]string{"evidence", "prepare"})
	command.SetIn(strings.NewReader(`{"selectors":[{"kind":"docbank_rendition","message_id":1,"attachment_id":2,"max_chars":1000}]}`))
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	require.ErrorContains(t, command.ExecuteContext(ctx), "API schema 3.6.0")
}

func kataInputFor(command string) string {
	switch command {
	case "evidence":
		return `{"selectors":[{"kind":"message","message_id":1,"max_chars":1000}]}`
	case "create":
		return `{"title":"Send the budget","evidence":[]}`
	}
	return `{"evidence":[]}`
}

// A passage names its source from its reference, then its subject or
// filename, and drops IDs that may belong to another archive.
func TestKataContextLabelsPassagesWithoutDisplay(t *testing.T) {
	chunk := generated.Reference{MessageID: 4, AttachmentID: new(int64(9)), DocumentChunk: &generated.DocumentReference{}}
	var out strings.Builder
	require.NoError(t, writeKataIssueContext(&out, generated.KataIssueContextResponse{Passages: []generated.ContextPassage{
		{State: "available", Evidence: generated.Evidence{Reference: generated.Reference{MessageID: 4}, Excerpt: "é界🙂 send the revised budget"}},
		{State: "available", Evidence: generated.Evidence{Reference: generated.Reference{Kind: "docbank_rendition", MessageID: 4, AttachmentID: new(int64(9))}, Display: generated.Display{Filename: new("voice.wav")}}},
		{State: "unreachable", Evidence: generated.Evidence{Reference: generated.Reference{Kind: "docbank_rendition"}}},
		{State: "changed", Evidence: generated.Evidence{Reference: chunk}, SavedQuote: new("Send the budget")},
		{State: "changed", Evidence: generated.Evidence{Reference: generated.Reference{MessageID: 4}, Display: generated.Display{ContainingTitle: new("Budget")}}},
		{State: "unavailable", Evidence: generated.Evidence{Reference: chunk}},
		{State: "unavailable", Evidence: generated.Evidence{Reference: generated.Reference{MessageID: 4}}},
	}}))
	assert.Equal(t, "\t\t\n\navailable\tmessage 4\n  [é界🙂 send the revised budget]\n\navailable\tattachment 9 of message 4: voice.wav\n\nunreachable\tDocbank unreachable; retry later\n\nchanged\tattachment 9 of message 4\n  saved quote: Send the budget\n\nchanged\tmessage 4: Budget\n\nunavailable\tattachment in this or another archive\n\nunavailable\tmessage in this or another archive\n", out.String())
}

// The daemon adapter carries a Docbank transcript citation from prepare
// through the issue and back to the file lookup and context read.
func TestKataDocbankThroughDaemonAdapter(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, _ := storedBeeperVoiceNote(t)
	var messageID, attachmentID int64
	var hash string
	require.NoError(st.DB().QueryRow(`SELECT message_id, id, content_hash FROM attachments`).Scan(&messageID, &attachmentID, &hash))
	const vault, version = "22222222-2222-4222-8222-222222222222", "11111111-1111-4111-8111-111111111111"
	transcript := []rune(strings.Repeat("é", kataevidence.MaxChars) + "Send the revised budget by Friday.")
	docbank := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/versions/" + version:
			_, _ = fmt.Fprintf(w, `{"id":%q,"node_id":7,"blob_hash":%q}`, version, hash)
		case "/api/v1/renditions/select":
			for name, value := range map[string]string{"Content-Version": version, "Rendition-Attachment": strings.Repeat("b", 64), "Rendition-Build": strings.Repeat("c", 64), "Blob-Hash": strings.Repeat("d", 64)} {
				w.Header().Set("X-Docbank-"+name, value)
			}
		case "/api/v1/evidence/windows":
			var request docbankmedia.EvidenceWindowRequest
			if !assert.NoError(json.UnmarshalRead(r.Body, &request, json.RejectUnknownMembers(true))) {
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			if request.Offset > len(transcript) {
				http.Error(w, "offset past end", http.StatusRequestedRangeNotSatisfiable)
				return
			}
			text := transcript[request.Offset:min(len(transcript), request.Offset+request.MaxChars)]
			window := docbankmedia.EvidenceWindow{VaultUID: request.VaultUID, NodeID: request.NodeID, ContentVersionID: request.ContentVersionID, ContentSHA256: request.ContentSHA256,
				RenditionAttachmentID: request.RenditionAttachmentID, BuildID: request.BuildID, RenditionSHA256: request.RenditionSHA256,
				Text: string(text), ActualStart: request.Offset, ActualEnd: request.Offset + len(text), EOF: request.Offset+len(text) == len(transcript)}
			assert.NoError(json.MarshalWrite(w, window))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(docbank.Close)
	archiveUID, err := st.ArchiveUIDContext(t.Context())
	require.NoError(err)
	mapping := store.BeeperMediaMapping{DestinationKey: docbankmedia.DestinationKey(docbank.URL, archiveUID), OccurrenceRef: "msgvault:voice1", Revision: "revision",
		SourceType: "beeper", SourceIdentifier: "signal", SourceConversationID: "default-thread", SourceMessageID: "voice1",
		SourceAttachmentID: "beeper:mxc://beeper.local/voice1", SourcePartKey: "beeper:mxc://beeper.local/voice1", MessageID: messageID, AttachmentID: attachmentID,
		SourceSHA256: hash, ByteLength: 1, RawHash: strings.Repeat("e", 64), OccurrenceJSON: `{}`, Filename: "voice.wav", MIMEType: "audio/wav",
		ProcessingKey: "processing", ProcessingProvider: "beeper", ProcessingProfile: "supplied-transcript"}
	require.NoError(st.ReconcileBeeperMediaMapping(t.Context(), mapping))
	prepared, err := st.PrepareBeeperMediaOperation(t.Context(), store.BeeperMediaOperation{Kind: store.BeeperMediaOperationRetain, DestinationKey: mapping.DestinationKey, OccurrenceRef: mapping.OccurrenceRef, Revision: mapping.Revision})
	require.NoError(err)
	applied, err := st.FinishBeeperMediaOperation(t.Context(), prepared, store.BeeperMediaResult{VaultUID: vault, DocbankSourceID: "source", SourceVersionID: "source-version",
		ContentVersionID: version, DocbankOccurrenceID: "occurrence", CoverageState: "transcribed"})
	require.NoError(err)
	require.True(applied)

	kataServer := httptest.NewServer(katatest.New(t).Service.Handler())
	t.Cleanup(kataServer.Close)
	cfg := &config.Config{}
	cfg.Integrations.Kata = config.TaskIntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: "example"}
	cfg.Integrations.Docbank = config.DocbankIntegrationConfig{Enabled: true, URL: docbank.URL, APIKey: "synthetic-key"}
	daemon := httptest.NewServer(api.NewServer(cfg, &storeAPIAdapter{store: st}, nil, slog.New(slog.DiscardHandler)).Router())
	t.Cleanup(daemon.Close)
	ctx := configureRemoteDaemonForTest(t, daemon.URL)

	selector := fmt.Sprintf(`{"selectors":[{"kind":"docbank_rendition","message_id":%d,"attachment_id":%d,"start_rune":%d,"max_chars":1000}]}`, messageID, attachmentID, kataevidence.MaxChars)
	var window generated.KataEvidencePrepareResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, selector, "evidence", "prepare"), &window))
	require.Len(window.Evidence, 1)
	assert.Equal("Send the revised budget by Friday.", window.Evidence[0].Excerpt)
	assert.Nil(window.Evidence[0].NextRune, "the transcript ends with this window")

	request, err := json.Marshal(generated.KataIssueCreateRequest{Title: "Send the revised budget", Evidence: []generated.Reference{window.Evidence[0].Reference}})
	require.NoError(err)
	var created generated.KataIssueResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, string(request), "create", "--idempotency-key", "docbank-key", "--json"), &created))
	var citing generated.KataIssueListResponse
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, "", "issues", "--message", strconv.FormatInt(messageID, 10), "--attachment", strconv.FormatInt(attachmentID, 10), "--json"), &citing))
	require.Len(citing.Issues, 1, "the file lookup finds an issue citing only its Docbank transcript")
	assert.Equal(created.Issue.UID, citing.Issues[0].UID)
	assert.Contains(string(runKataCommand(ctx, t, "", "context", created.Issue.QualifiedRef)), "[Send the revised budget by Friday.]\n")

	// A file cited as extracted text and as a Docbank transcript is found by both, once
	// each, oldest first.
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO document_occurrences(occurrence_key,attachment_id,message_id,source_id,canonical_blob_hash,attachment_role,role_source)
		SELECT 'doc-occurrence',a.id,a.message_id,m.source_id,a.content_hash,a.attachment_role,'test' FROM attachments a JOIN messages m ON m.id=a.message_id WHERE a.id=?`), attachmentID)
	require.NoError(err)
	sources := []kataevidence.Reference{
		{ArchiveUID: archiveUID, SourceType: "beeper", SourceIdentifier: "signal", SourceMessageID: "voice1", OccurrenceKey: "doc-occurrence"},
		{ArchiveUID: archiveUID, SourceType: "beeper", SourceIdentifier: "signal", SourceMessageID: "voice1", OccurrenceKey: mapping.OccurrenceRef},
	}
	fileKey := func(ref kataevidence.Reference) string {
		keys := kataevidence.SourceKeys(ref)
		return keys[len(keys)-1]
	}
	kata, err := taskclient.ConnectKata(t.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: kataServer.URL, APIKey: katatest.Token, DefaultProject: "example"})
	require.NoError(err)
	chunkOnly, err := kata.CreateTask(t.Context(), "example", "chunk-only", taskclient.KataCreate{Title: "Cites the extracted text", Metadata: map[string]any{fileKey(sources[0]): true}})
	require.NoError(err)
	both, err := kata.CreateTask(t.Context(), "example", "both", taskclient.KataCreate{Title: "Cites both", Metadata: map[string]any{fileKey(sources[0]): true, fileKey(sources[1]): true}})
	require.NoError(err)
	require.NoError(json.Unmarshal(runKataCommand(ctx, t, "", "issues", "--message", strconv.FormatInt(messageID, 10), "--attachment", strconv.FormatInt(attachmentID, 10), "--json"), &citing))
	uids := []string{}
	for _, issue := range citing.Issues {
		uids = append(uids, issue.UID)
	}
	assert.Equal([]string{created.Issue.UID, chunkOnly.UID, both.UID}, uids)
}
