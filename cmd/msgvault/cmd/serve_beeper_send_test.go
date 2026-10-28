package cmd

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/beeper"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestBeeperSend(t *testing.T) {
	for _, tc := range []struct {
		name, code                                                                                                       string
		status, replyStatus                                                                                              int
		disabled, unarchived, file, relative, directory, reply, wrongReply, grant, wrongSource, draftOnly, uploadFailure bool
		merged, changedReply, padded, body, uploadError                                                                  bool
	}{
		{name: "accepted", code: "accepted"},
		{name: "padded chat", code: "accepted", padded: true},
		{name: "text and file", code: "accepted", file: true, body: true},
		{name: "merged chat", code: "not_supported", file: true, merged: true},
		{name: "reply lookup unavailable", code: "provider_unavailable", reply: true, file: true, replyStatus: 500},
		{name: "missing reply", code: "invalid_reply_target", reply: true, file: true, replyStatus: 404},
		{name: "changed reply", code: "invalid_reply_target", reply: true, file: true, changedReply: true},
		{name: "disabled", code: "send_disabled", disabled: true},
		{name: "send grant disabled", code: "send_disabled", grant: true, disabled: true},
		{name: "unknown", code: "remote_unknown", status: 500},
		{name: "rejected", code: "provider_rejected", status: 409},
		{name: "not archived", code: "chat_not_archived", unarchived: true},
		{name: "draft grant", code: "not_permitted", grant: true, draftOnly: true},
		{name: "wrong source grant", code: "not_permitted", grant: true, wrongSource: true},
		{name: "send grant", code: "accepted", grant: true},
		{name: "agent file", code: "not_permitted", grant: true, file: true},
		{name: "file only", code: "accepted", file: true},
		{name: "relative file", code: "accepted", file: true, relative: true},
		{name: "upload error payload", code: "provider_rejected", file: true, uploadError: true},
		{name: "upload failure", code: "provider_unavailable", file: true, uploadFailure: true},
		{name: "directory", code: "invalid_args", file: true, directory: true},
		{name: "upload rejected", code: "provider_rejected", file: true, uploadFailure: true, status: 413},
		{name: "reply", code: "accepted", reply: true},
		{name: "wrong reply", code: "invalid_reply_target", reply: true, wrongReply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBeeperDraftFixture(t)
			if !tc.disabled {
				f.adapter.beeperSendPolicy = []config.GmailDraftSource{{SourceID: f.source.ID, Enabled: true}}
			}
			conversation, err := f.store.EnsureConversationWithType(f.source.ID, beeperDraftTestChat, "direct", "Example chat")
			require.NoError(t, err)
			var requests []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v1/chats/"+beeperDraftTestChat+"/messages/original":
					if tc.replyStatus != 0 {
						w.Header().Set("Retry-After", "0")
						w.WriteHeader(tc.replyStatus)
						return
					}
					sent := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
					if tc.changedReply {
						sent = sent.Add(time.Hour)
					}
					_, _ = fmt.Fprintf(w, `{"id":"original","chatID":%q,"accountID":%q,"timestamp":%q}`, beeperDraftTestChat, f.source.Identifier, sent.Format(time.RFC3339))
				case r.Method == http.MethodGet:
					merge := "null"
					if tc.merged {
						merge = `{}`
					}
					_, _ = fmt.Fprintf(w, `{"id":%q,"accountID":%q,"merge":%s}`, beeperDraftTestChat, f.source.Identifier, merge)
				case r.URL.Path == "/v1/assets/upload":
					if tc.uploadError {
						_, _ = io.WriteString(w, `{"error":"file too large"}`)
						return
					}
					if tc.uploadFailure {
						status := tc.status
						if status == 0 {
							status = 500
						}
						w.WriteHeader(status)
						return
					}
					_, _ = io.WriteString(w, `{"uploadID":"upload-1"}`)
				default:
					var body map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
					if tc.file {
						assert.Equal(t, map[string]any{"uploadID": "upload-1"}, body["attachment"])
					}
					if !tc.file || tc.body {
						assert.Equal(t, "hello", body["text"])
					} else {
						assert.NotContains(t, body, "text")
					}
					if tc.reply {
						assert.Equal(t, "original", body["replyToMessageID"])
					}
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					_, _ = io.WriteString(w, `{"chatID":"merged-chat","pendingMessageID":"pending-1"}`)
				}
			}))
			defer server.Close()
			f.adapter.config.Beeper.URL = server.URL
			chat := beeperDraftTestChat
			if tc.unarchived {
				chat = "!missing:beeper.local"
			}
			if tc.padded {
				chat = " \t" + chat + " \n"
			}
			args := []string{"send-message", "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", chat, "--json"}
			req := api.CLIRunRequest{}
			if tc.file {
				req.Cwd = t.TempDir()
				path := filepath.Join(req.Cwd, "example.txt")
				require.NoError(t, os.WriteFile(path, []byte("file content"), 0o600))
				if tc.directory {
					path = req.Cwd
				}
				if tc.relative {
					path = filepath.Base(path)
				}
				args = append(args, "--file", path)
			}
			if !tc.file || tc.body {
				args = append(args, "--body", "hello")
			}
			if tc.reply {
				if tc.wrongReply {
					conversation, err = f.store.EnsureConversationWithType(f.source.ID, "!other:beeper.local", "direct", "Other chat")
					require.NoError(t, err)
				}
				id, err := f.store.UpsertMessage(&store.Message{SourceID: f.source.ID, SourceMessageID: "original", ConversationID: conversation, MessageType: "beeper", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
				require.NoError(t, err)
				args = append(args, "--reply-to", strconv.FormatInt(id, 10))
			}
			if tc.grant {
				permission := agentgrant.PermissionMessageSend
				if tc.draftOnly {
					permission = agentgrant.PermissionDraftCreate
				}
				identifier := f.source.Identifier
				if tc.wrongSource {
					identifier = "other-account"
				}
				req.Grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{permission}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: identifier}}}
			}
			req.Args = args
			var output beeperSendOutput
			err = f.adapter.runCLISendMessage(t.Context(), req, func(event api.CLIRunEvent) error { return json.Unmarshal([]byte(event.Data), &output) })
			if tc.code == "accepted" {
				require.NoError(t, err)
				assert.Equal(t, "accepted", output.Status)
				assert.Equal(t, "pending-1", output.PendingMessageID)
				assert.Equal(t, "merged-chat", output.ChatID)
			} else {
				assertBeeperDraftCode(t, err, tc.code)
			}
			if tc.code == "accepted" || tc.code == "remote_unknown" || (tc.code == "provider_rejected" && !tc.uploadFailure && !tc.uploadError) {
				assert.Equal(t, tc.code, output.Status)
			} else {
				assert.Empty(t, output.Status)
			}
			if tc.replyStatus != 0 {
				require.GreaterOrEqual(t, len(requests), 2)
				assert.Equal(t, "GET /v1/chats/"+beeperDraftTestChat, requests[0])
				for _, request := range requests[1:] {
					assert.Equal(t, "GET /v1/chats/"+beeperDraftTestChat+"/messages/original", request)
				}
				return
			}
			switch tc.code {
			case "accepted", "remote_unknown", "provider_rejected":
				expected := []string{"GET /v1/chats/" + beeperDraftTestChat}
				if tc.reply {
					expected = append(expected, "GET /v1/chats/"+beeperDraftTestChat+"/messages/original")
				}
				if tc.file {
					expected = append(expected, "POST /v1/assets/upload")
				}
				if !tc.uploadFailure && !tc.uploadError {
					expected = append(expected, "POST /v1/chats/"+beeperDraftTestChat+"/messages")
				}
				assert.Equal(t, expected, requests)
			case "provider_unavailable":
				assert.Equal(t, []string{"GET /v1/chats/" + beeperDraftTestChat, "POST /v1/assets/upload"}, requests)
			default:
				if tc.changedReply || tc.wrongReply {
					expected := []string{"GET /v1/chats/" + beeperDraftTestChat}
					if !tc.wrongReply {
						expected = append(expected, "GET /v1/chats/"+beeperDraftTestChat+"/messages/original")
					}
					assert.Equal(t, expected, requests)
				} else if tc.merged {
					assert.Equal(t, []string{"GET /v1/chats/" + beeperDraftTestChat}, requests)
				} else {
					assert.Empty(t, requests)
				}
			}
		})
	}
}

func TestBeeperSendTextOutput(t *testing.T) {
	var output string
	require.NoError(t, emitBeeperSend(func(event api.CLIRunEvent) error { output += event.Data; return nil }, cliStreamStderr, false, beeperSendOutput{Status: "remote_unknown", ChatID: "chat", FileName: "example.txt"}))
	assert.Contains(t, output, "file: example.txt")
	assert.NotContains(t, output, "pending message:")
	assert.Contains(t, output, "Read the chat before sending again")
}

func TestBeeperSendPreRequestFailure(t *testing.T) {
	assert.Equal(t, "provider_unavailable", beeperSendFailureCode(errors.New("token unavailable"), true))
}

func TestBeeperSendReplyArchiveUnavailable(t *testing.T) {
	testutil.SkipIfPostgres(t, "local read fault injection alters SQLite tables")
	f := newBeeperDraftFixture(t)
	f.adapter.beeperSendPolicy = []config.GmailDraftSource{{SourceID: f.source.ID, Enabled: true}}
	conversation, err := f.store.EnsureConversationWithType(f.source.ID, beeperDraftTestChat, "direct", "Example chat")
	require.NoError(t, err)
	id, err := f.store.UpsertMessage(&store.Message{SourceID: f.source.ID, SourceMessageID: "original", ConversationID: conversation, MessageType: "beeper"})
	require.NoError(t, err)
	_, err = f.store.GetMessageContext(t.Context(), id)
	require.NoError(t, err)

	// Only the message detail read needs this table; source and chat lookups still work.
	_, err = f.store.DB().Exec("ALTER TABLE message_bodies RENAME TO fault_message_bodies")
	require.NoError(t, err)
	_, err = f.store.GetMessageContext(t.Context(), id)
	require.Error(t, err)
	require.NotErrorIs(t, err, sql.ErrNoRows)

	err = f.adapter.runCLISendMessage(t.Context(), api.CLIRunRequest{Args: []string{
		"send-message", "--source-id", strconv.FormatInt(f.source.ID, 10),
		"--to", beeperDraftTestChat, "--body", "hello", "--reply-to", strconv.FormatInt(id, 10),
	}}, nil)

	_, restoreErr := f.store.DB().Exec("ALTER TABLE fault_message_bodies RENAME TO message_bodies")
	require.NoError(t, restoreErr)
	_, lookupErr := f.store.GetMessageContext(t.Context(), id)
	require.NoError(t, lookupErr)
	requests, patches := f.beeper.counts()
	assert.Equal(t, 1, requests, "only the live chat lookup should run")
	assert.Zero(t, patches)
	assertBeeperDraftCode(t, err, "provider_unavailable")
}

func TestBeeperSendRedirectFailureCode(t *testing.T) {
	assert.Equal(t, "provider_rejected", beeperSendFailureCode(&beeper.WriteError{Status: http.StatusTemporaryRedirect}, true))
}
