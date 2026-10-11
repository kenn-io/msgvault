package archive_test

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/archive"
)

type slackPeer struct {
	mu       sync.Mutex
	requests []string
	reply    bool
	rootTS   string
}

func (p *slackPeer) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, r.URL.Path)
	scopes := "channels:read,channels:history,users:read,search:read"
	if r.Header.Get("Authorization") == "Bearer broad" {
		scopes += ",groups:read,groups:history"
	}
	w.Header().Set("X-Oauth-Scopes", scopes)
	w.Header().Set("Content-Type", "application/json")
	_ = r.ParseForm()
	var body any
	root := map[string]any{"type": "message", "ts": p.rootTS, "user": "UAUTHOR", "text": "Selected telescope discussion"}
	if p.reply {
		root["reply_count"] = 1
		root["thread_ts"] = p.rootTS
	}
	switch r.URL.Path {
	case "/auth.test":
		user := "UFIRST"
		if r.Header.Get("Authorization") == "Bearer replacement" {
			user = "USECOND"
		}
		body = map[string]any{"ok": true, "team_id": "TPUBLIC", "team": "Example workspace", "user_id": user}
	case "/users.list":
		body = map[string]any{"ok": true, "members": []any{
			map[string]any{"id": "UAUTHOR", "name": "author", "profile": map[string]any{"display_name": "Example Author"}},
			map[string]any{"id": "UFIRST", "tz": "UTC"}, map[string]any{"id": "USECOND", "tz": "UTC"},
		}}
	case "/conversations.list", "/users.conversations":
		wantTypes := "public_channel"
		if r.Header.Get("Authorization") == "Bearer broad" {
			wantTypes += ",private_channel"
		}
		if r.Form.Get("types") != wantTypes {
			http.Error(w, "unexpected conversation type", http.StatusBadRequest)
			return
		}
		body = map[string]any{"ok": true, "channels": []any{
			map[string]any{"id": "CSELECTED", "name": "renamed", "is_channel": true, "num_members": 3},
			map[string]any{"id": "COTHER", "name": "unselected", "is_channel": true},
			map[string]any{"id": "CPRIVATE", "name": "private", "is_private": true},
		}}
	case "/conversations.members":
		body = map[string]any{"ok": true, "members": []string{"UAUTHOR", "UFIRST", "USECOND"}}
	case "/conversations.history":
		if r.Form.Get("channel") != "CSELECTED" && (r.Header.Get("Authorization") != "Bearer broad" || r.Form.Get("channel") != "CPRIVATE") {
			http.Error(w, "unexpected channel", http.StatusForbidden)
			return
		}
		messages := []any{}
		oldest := r.Form.Get("oldest")
		if oldest == "" || oldest < p.rootTS {
			messages = append(messages, root)
		}
		if ownerTS := p.ownerTS(); r.Form.Get("channel") == "CSELECTED" && (oldest == "" || oldest < ownerTS) {
			messages = append(messages, map[string]any{"type": "message", "ts": ownerTS, "user": "UFIRST", "text": "Owner calibration note"})
		}
		body = map[string]any{"ok": true, "messages": messages, "has_more": false}
	case "/conversations.replies":
		if r.Form.Get("channel") != "CSELECTED" {
			http.Error(w, "unexpected reply channel", http.StatusForbidden)
			return
		}
		seconds, _ := strconv.ParseInt(strings.Split(p.rootTS, ".")[0], 10, 64)
		body = map[string]any{"ok": true, "messages": []any{root,
			map[string]any{"type": "message", "ts": fmt.Sprintf("%d.000002", seconds+1), "thread_ts": p.rootTS, "user": "UAUTHOR", "text": "Later telescope reply"},
		}, "has_more": false}
	case "/search.messages":
		if !strings.Contains(r.Form.Get("query"), "in:<#CSELECTED>") {
			http.Error(w, "unscoped search", http.StatusForbidden)
			return
		}
		body = map[string]any{"ok": true, "messages": map[string]any{"total": 0, "matches": []any{}, "paging": map[string]any{"page": 1, "pages": 0}}}
	default:
		http.Error(w, "unexpected Slack operation", http.StatusNotFound)
		return
	}
	_ = json.MarshalWrite(w, body)
}

// ownerTS is a message the first credential's user sent before the root.
func (p *slackPeer) ownerTS() string {
	seconds, _ := strconv.ParseInt(strings.Split(p.rootTS, ".")[0], 10, 64)
	return fmt.Sprintf("%d.000001", seconds-60)
}

func TestSlackInspectionReportsCredentialScopes(t *testing.T) {
	assert := assert.New(t)
	peer := &slackPeer{}
	server := httptest.NewServer(http.HandlerFunc(peer.serve))
	defer server.Close()
	identity, err := archive.InspectSlack(t.Context(), archive.SlackCredential{Token: "broad", BaseURL: server.URL})
	require.NoError(t, err)
	assert.Contains(identity.Scopes, "groups:history")
	peer.mu.Lock()
	defer peer.mu.Unlock()
	assert.Equal([]string{"/auth.test"}, peer.requests)
}

func exerciseSlackReplacement(t *testing.T, runtime *archive.Archive) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)
	peer := &slackPeer{rootTS: fmt.Sprintf("%d.000001", time.Now().Add(-time.Hour).Unix())}
	server := httptest.NewServer(http.HandlerFunc(peer.serve))
	defer server.Close()
	credential := archive.SlackCredential{Token: "first", BaseURL: server.URL}
	sourceID, err := runtime.BindSlack(t.Context(), credential, "TPUBLIC", 0)
	require.NoError(err)
	opts := archive.SlackSync{Credential: credential, Options: archive.SlackOptions{SourceID: sourceID, TeamID: "TPUBLIC", ChannelIDs: []string{"CSELECTED", "CPRIVATE"}, ExcludePrivateChannels: true, ExcludeDMs: true, ExcludeGroupDMs: true, NoMedia: true}}
	first, err := runtime.SyncSlack(t.Context(), opts)
	require.NoError(err)
	assert.Equal(1, first.ConversationsProcessed)
	assert.Equal(2, first.MessagesAdded)
	assertFromMe(t, runtime, "CSELECTED:"+peer.ownerTS(), true)
	firstIDs := searchIDs(t, runtime, "telescope", 1)
	peer.mu.Lock()
	peer.reply = true
	peer.mu.Unlock()
	opts.Credential.Token = "replacement"
	rebound, err := runtime.BindSlack(t.Context(), opts.Credential, "TPUBLIC", sourceID)
	require.NoError(err)
	assert.Equal(sourceID, rebound)
	second, err := runtime.SyncSlack(t.Context(), opts)
	require.NoError(err)
	assert.Equal(sourceID, second.SourceID)
	assert.Equal(1, second.MessagesAdded, "replacement rescans old roots for replies without duplicating them")
	third, err := runtime.SyncSlack(t.Context(), opts)
	require.NoError(err)
	assert.Zero(third.MessagesAdded, "repeated collection deduplicates retained history")
	progress, err := runtime.Progress(t.Context(), sourceID)
	require.NoError(err)
	require.NotNil(progress.Slack)
	assert.Equal("completed", progress.Status)
	assert.Equal("USECOND", progress.Slack.PrincipalID)
	retainedIDs := searchIDs(t, runtime, "telescope", 2)
	assert.Contains(retainedIDs, firstIDs[0], "credential replacement preserves message identity")
	assertFromMe(t, runtime, "CSELECTED:"+peer.ownerTS(), true)
	assertFromMe(t, runtime, "CSELECTED:"+peer.rootTS, false)
	require.NoError(runtime.PurgeChannel(t.Context(), sourceID, "CSELECTED"))
	searchIDs(t, runtime, "telescope", 0)
	searchIDs(t, runtime, "observatory", 1)
	for _, id := range retainedIDs {
		response := httptest.NewRecorder()
		archiveAPI(t, runtime).Handler(func(http.ResponseWriter, *http.Request, archive.Operation) bool { return true }).ServeHTTP(response, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil))
		assert.Equal(http.StatusNotFound, response.Code, "purged detail cannot be fetched by retained ID")
	}
	opts.Options.ChannelIDs = []string{}
	empty, err := runtime.SyncSlack(t.Context(), opts)
	require.NoError(err)
	assert.Zero(empty.MessagesProcessed)
	require.NoError(runtime.PurgeSource(t.Context(), sourceID))
	require.NoError(runtime.PurgeSource(t.Context(), sourceID), "retry after successful purge")
	searchIDs(t, runtime, "telescope", 0)
	_, err = runtime.SyncSlack(t.Context(), opts)
	require.Error(err, "a retried run must not recreate a purged source")
}

// assertFromMe checks sender attribution, which stays with the source owner
// when another user's credential replaces the original one.
func assertFromMe(t *testing.T, runtime *archive.Archive, sourceMessageID string, want bool) {
	t.Helper()
	st := runtime.Store()
	var fromMe bool
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(
		"SELECT is_from_me FROM messages WHERE source_message_id = ?"), sourceMessageID).Scan(&fromMe))
	assert.Equal(t, want, fromMe, sourceMessageID)
}

func searchIDs(t *testing.T, runtime *archive.Archive, query string, want int) []int64 {
	t.Helper()
	require := require.New(t)
	response := httptest.NewRecorder()
	archiveAPI(t, runtime).Handler(func(http.ResponseWriter, *http.Request, archive.Operation) bool { return true }).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/search?q="+url.QueryEscape(query), nil))
	require.Equal(http.StatusOK, response.Code, response.Body.String())
	var result struct {
		Total    int64 `json:"total"`
		Messages []struct {
			ID int64 `json:"id"`
		} `json:"messages"`
	}
	require.NoError(json.Unmarshal(response.Body.Bytes(), &result))
	require.Equal(int64(want), result.Total)
	require.Len(result.Messages, want)
	ids := make([]int64, len(result.Messages))
	for i, message := range result.Messages {
		ids[i] = message.ID
	}
	return ids
}
