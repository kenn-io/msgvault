package cmd

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/threads"
)

func TestThreadsDocumentedWorkflow(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := newThreadsSendDaemonFixture(t)
	f.cfg.Threads.Sends = nil
	original := f.provider.readHandler
	pause := true
	timestamp := f.provider.clock().Add(-30 * time.Minute).Format(time.RFC3339Nano)
	f.provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet {
			return original(w, r)
		}
		switch r.URL.Path {
		case "/v1.0/me/threads", "/v1.0/me/replies", "/v1.0/100/conversation":
			if pause {
				w.Header().Set("X-App-Usage", `{"call_count":80}`)
			}
			if r.URL.Query().Get("after") != "" {
				_, _ = fmt.Fprint(w, `{"data":[]}`)
				return true
			}
			if r.URL.Path == "/v1.0/me/replies" {
				_, _ = fmt.Fprint(w, `{"data":[]}`)
				return true
			}
			data := []map[string]any{{"id": "100", "text": "Documented project update", "timestamp": timestamp, "has_replies": true}}
			if r.URL.Path == "/v1.0/100/conversation" {
				data = []map[string]any{
					{"id": "101", "text": "First reply", "timestamp": timestamp, "root_post": map[string]string{"id": "100"}, "replied_to": map[string]string{"id": "100"}, "is_reply": true, "username": "synthetic_author"},
					{"id": "102", "text": "Nested reply", "timestamp": timestamp, "root_post": map[string]string{"id": "100"}, "replied_to": map[string]string{"id": "101"}, "is_reply": true, "username": "synthetic_other"},
				}
			}
			payload, err := json.Marshal(map[string]any{"data": data, "paging": map[string]any{"cursors": map[string]string{"after": "next"}}})
			require.NoError(err)
			_, err = w.Write(payload)
			require.NoError(err)
			return true
		default:
			return original(w, r)
		}
	}
	adapter := &threadsSyncDaemon{storeAPIAdapter: f.adapter, deps: f.deps}
	engine := query.NewSQLiteEngine(f.adapter.store.DB())
	if f.adapter.store.IsPostgreSQL() {
		engine = query.NewEngineWithDialect(f.adapter.store.DB(), query.PostgreSQLQueryDialect{})
	}
	server := httptest.NewServer(api.NewServerWithOptions(api.ServerOptions{Config: f.cfg, Store: adapter, Engine: engine, Logger: slog.New(slog.DiscardHandler), OperationGate: api.NewSerialOperationGate()}).Router())
	defer server.Close()
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: f.cfg.Server.APIKey, AllowInsecure: true})
	require.NoError(err)
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := client.RunCLICommand(t.Context(), daemonclient.CLIRunRequest{Args: args}, func(stream, data string) error {
			if stream == "stdout" {
				out.WriteString(data)
			}
			return nil
		})
		return out.String(), err
	}
	output, err := run("sync-threads", f.source.Identifier, "--json")
	require.NoError(err)
	var summary threads.ImportSummary
	require.NoError(json.Unmarshal([]byte(output), &summary))
	assert.Equal(store.SyncStatusPaused, summary.Status)
	f.provider.mu.Lock()
	pause = false
	f.provider.mu.Unlock()
	for range 3 {
		_, err = run("sync-threads", f.source.Identifier, "--json")
		require.NoError(err)
	}
	remote := daemonclient.NewEngineAdapter(client)
	matches, err := remote.SearchFast(t.Context(), search.ParseOR("Documented project"), query.MessageFilter{SourceID: &f.source.ID, MessageType: "threads"}, 20, 0)
	require.NoError(err)
	require.Len(matches, 1)
	var nested int64
	require.NoError(f.adapter.store.DB().QueryRow(f.adapter.store.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id='102'`), f.source.ID).Scan(&nested))
	parent, err := f.adapter.store.GetMessageRelationTarget(nested)
	require.NoError(err)
	output, err = run("draft-compose", "--conversation="+fmt.Sprint(parent.ConversationID), "--reply-to="+fmt.Sprint(nested), "--body=reviewed daemon reply", "--json")
	require.NoError(err)
	var draft chatDraftOutput
	require.NoError(json.Unmarshal([]byte(output), &draft))
	assert.Equal("102", draft.ReplyToSourceMessageID)
	f.draft = store.ChatDraft{DraftID: draft.DraftID, Revision: draft.Revision, Body: draft.Body, ReplyToSourceMessageID: "102"}
	output, err = run("send-reply", "--draft="+draft.DraftID, "--revision=1", "--json")
	require.Error(err)
	var result threads.SendResult
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.Equal("reply_not_permitted", result.Status)
	assert.Zero(f.creates.Load())
	f.cfg.Threads.Sends = []config.ThreadsSendConfig{{SourceID: f.source.ID}}
	// Canonical send response must describe the nested parent, not the root.
	sendOriginal := f.provider.readHandler
	f.provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1.0/500" {
			_, _ = fmt.Fprintf(w, `{"id":"500","timestamp":%q,"text":%q,"root_post":{"id":"100"},"replied_to":{"id":"102"},"is_reply":true,"is_reply_owned_by_me":true}`, timestamp, f.draft.Body)
			return true
		}
		return sendOriginal(w, r)
	}
	output, err = run("send-reply", "--draft="+draft.DraftID, "--revision=1", "--json")
	require.NoError(err)
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.Equal("complete", result.Status)
	assert.Equal("sent", result.RemoteStatus)
	// A second, intentionally separate draft exercises lost-response adoption.
	output, err = run("draft-compose", "--conversation="+fmt.Sprint(parent.ConversationID), "--reply-to="+fmt.Sprint(nested), "--body=reviewed daemon reply", "--json")
	require.NoError(err)
	require.NoError(json.Unmarshal([]byte(output), &draft))
	f.draft.DraftID = draft.DraftID
	f.mode = "drop"
	previous := f.provider.readHandler
	f.provider.readHandler = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/v1.0/501":
			_, _ = fmt.Fprintf(w, `{"id":"501","timestamp":%q,"text":%q,"root_post":{"id":"100"},"replied_to":{"id":"102"},"is_reply":true,"is_reply_owned_by_me":true}`, timestamp, f.draft.Body)
			return true
		case r.URL.Path == "/v1.0/me/threads" && r.Method == http.MethodPost:
			f.creates.Add(1)
			_, _ = fmt.Fprint(w, `{"id":"401"}`)
			return true
		case r.URL.Path == "/v1.0/401":
			_, _ = fmt.Fprint(w, `{"id":"401","status":"FINISHED"}`)
			return true
		default:
			return previous(w, r)
		}
	}

	output, err = run("send-reply", "--draft="+draft.DraftID, "--revision=1", "--json")
	require.Error(err)
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.Equal("remote_unknown", result.Status)
	output, err = run("send-reply", "--draft="+draft.DraftID, "--revision=1", "--adopt-media-id=501", "--json")
	require.NoError(err)
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.Equal("complete", result.Status)
	assert.Equal(int32(2), f.publishes.Load())

	f.mode = "drop"
	f.draft.Body = "direct daemon reply"
	f.draft.ReplyToSourceMessageID = "102"
	output, err = run("send-reply", fmt.Sprint(nested), "--body="+f.draft.Body, "--json")
	require.Error(err)
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.Equal("remote_unknown", result.Status)
	assert.NotEmpty(result.DraftID)
	assert.Equal(int32(3), f.publishes.Load())
	directDraftID := result.DraftID
	output, err = run("send-reply", fmt.Sprint(nested), "--body="+f.draft.Body, "--json")
	require.Error(err)
	require.NoError(json.Unmarshal([]byte(output), &result))
	assert.Equal(directDraftID, result.DraftID)
	assert.Equal("remote_unknown", result.Status)
	assert.Equal(int32(3), f.publishes.Load())
}
