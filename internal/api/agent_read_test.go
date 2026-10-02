package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAgentReadAuthorization(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	requestCount := 0
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "allowed@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("test", "other@example.test")
	requirements.NoError(err)
	ids := make([]int64, 2)
	for i, s := range []*store.Source{src, other} {
		conv, err := st.EnsureConversationWithType(s.ID, "thread", "email_thread", "Synthetic")
		requirements.NoError(err)
		ids[i], err = st.UpsertMessage(&store.Message{SourceID: s.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(i), Subject: sql.NullString{String: fmt.Sprintf("glacier source-%d", i), Valid: true}, MessageType: "email"})
		requirements.NoError(err)
		requirements.NoError(st.UpsertFTS(ids[i], fmt.Sprintf("glacier source-%d", i), "", s.Identifier, "", ""))
		participant, err := st.EnsureParticipant(s.Identifier, "", "example.test")
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(ids[i], "from", []int64{participant}, nil))
	}
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner-key", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	defer srv.agentGrants.Close()
	for _, coll := range []struct {
		name string
		ids  []int64
	}{{"allowed", []int64{src.ID}}, {"mixed", []int64{src.ID, other.ID}}, {"empty", nil}} {
		members := coll.ids
		if len(members) == 0 {
			members = []int64{src.ID}
		}
		_, err := st.CreateCollection(coll.name, "", members)
		requirements.NoError(err)
		if len(coll.ids) == 0 {
			requirements.NoError(st.RemoveSourcesFromCollection(coll.name, members))
		}
	}
	requirements.NoError(os.MkdirAll(cfg.AttachmentsDir(), 0700))
	hash := strings.Repeat("a", 64)
	storagePath, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(storagePath), 0700))
	requirements.NoError(os.WriteFile(storagePath, []byte("synthetic attachment"), 0600))
	requirements.NoError(os.WriteFile(filepath.Join(cfg.AttachmentsDir(), "sample.bin"), []byte("synthetic attachment"), 0600))
	requirements.NoError(st.UpsertAttachment(ids[0], "sample.bin", "application/octet-stream", "sample.bin", hash, 20))
	var attID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM attachments WHERE message_id=?"), ids[0]).Scan(&attID))
	outsideHash := strings.Repeat("b", 64)
	requirements.NoError(st.UpsertAttachment(ids[1], "outside.bin", "application/octet-stream", "outside.bin", outsideHash, 20))
	outsidePath, err := msgexport.StoragePath(cfg.AttachmentsDir(), outsideHash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(outsidePath), 0700))
	requirements.NoError(os.WriteFile(outsidePath, []byte("outside attachment"), 0600))
	for _, tc := range []struct{ path, permission string }{
		{"/api/v1/cli/accounts", "search.read"},
		{"/api/v1/cli/collections", "search.read"},
		{"/api/v1/cli/search?q=glacier", "search.read"},
		{"/api/v1/search?q=glacier", "search.read"},
		{"/api/v1/search/fast?q=glacier", "search.read"},
		{"/api/v1/aggregates", "search.read"},
		{"/api/v1/stats/total", "stats.read"},
		{"/api/v1/messages/filter", "search.read"},
		{"/api/v1/messages", "search.read"},
		{"/api/v1/aggregates/sub?view_type=senders&key=example.test", "search.read"},
		{"/api/v1/search/deep?q=glacier", "search.read"},
		{"/api/v1/search/domains?domains=example.test", "search.read"},
		{fmt.Sprintf("/api/v1/attachments/%d", attID), "attachment.read"},
		{"/api/v1/cli/attachment?content_hash=" + hash, "attachment.read"},
		{"/api/v1/attachments/" + hash + "/content", "attachment.read"},
		{fmt.Sprintf("/api/v1/cli/message/thread?id=%d", ids[0]), "message.read"},
		{"/api/v1/cli/stats", "stats.read"},
		{"/api/v1/stats", "stats.read"},
		{fmt.Sprintf("/api/v1/messages/%d", ids[0]), "message.read"},
		{fmt.Sprintf("/api/v1/cli/message?id=%d", ids[0]), "message.read"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			requirements := require.New(t)
			_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.Permission(tc.permission)}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}})
			requirements.NoError(err)
			for _, access := range []struct {
				name, token string
				code        int
			}{{"allowed", secret, 200}, {"missing permission", "", 401}} {
				t.Run(access.name, func(t *testing.T) {
					requirements := require.New(t)
					assertions := assert.New(t)
					token := access.token
					if token == "" {
						_, token, _, err = srv.agentGrants.Issue("wrong", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}})
						requirements.NoError(err)
					}
					r := httptest.NewRequest(http.MethodGet, tc.path, nil)
					requestCount++
					r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", requestCount)
					r.Header.Set(apiprotocol.AgentTokenHeader, token)
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, r)
					assertions.Equal(access.code, w.Code, w.Body.String())
					if access.code == 200 {
						assertions.NotContains(w.Body.String(), "glacier source-1")
					}
					if access.code != 200 {
						assertions.Contains(w.Body.String(), tc.permission)
					}
				})
			}
		})
	}
	_, secret, _, err := srv.agentGrants.Issue("all reads", []agentgrant.Permission{"search.read", "stats.read", "message.read", "attachment.read"}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}})
	requirements.NoError(err)
	for _, path := range []string{fmt.Sprintf("/api/v1/messages/%d", ids[1]), fmt.Sprintf("/api/v1/search/fast?q=glacier&source_ids=%d,%d", src.ID, other.ID), fmt.Sprintf("/api/v1/cli/attachment?id=%d&content_hash=%s", attID, outsideHash), "/api/v1/settings", "/api/v1/agent-tokens", "/api/v1/cli/message/thread?thread_id=thread"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		requestCount++
		r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", requestCount)
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(http.StatusUnauthorized, w.Code, path+": "+w.Body.String())
	}
	_, multiToken, _, err := srv.agentGrants.Issue("multiple sources", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}, {ID: other.ID, Type: other.SourceType, Identifier: other.Identifier}})
	requirements.NoError(err)
	for _, tc := range []struct {
		path string
		both bool
	}{{"/api/v1/cli/search?q=glacier", true}, {"/api/v1/cli/search?q=glacier&collection=mixed", true}, {fmt.Sprintf("/api/v1/cli/search?q=glacier&source_ids=%d", src.ID), false}, {fmt.Sprintf("/api/v1/cli/search?q=glacier&source_ids=%d&collection=mixed", src.ID), false}} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, multiToken)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), "glacier source-0")
		if tc.both {
			assertions.Contains(w.Body.String(), "glacier source-1")
		} else {
			assertions.NotContains(w.Body.String(), "glacier source-1")
		}
	}
}

func TestAgentCollectionsAndCLIRun(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "scoped@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("test", "outside@example.test")
	requirements.NoError(err)
	for _, coll := range []struct {
		name string
		ids  []int64
	}{{"allowed", []int64{src.ID}}, {"mixed", []int64{src.ID, other.ID}}, {"empty", nil}} {
		members := coll.ids
		if len(members) == 0 {
			members = []int64{src.ID}
		}
		_, err := st.CreateCollection(coll.name, "", members)
		requirements.NoError(err)
		if len(coll.ids) == 0 {
			requirements.NoError(st.RemoveSourcesFromCollection(coll.name, members))
		}
	}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{"search.read", "stats.read"}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}})
	requirements.NoError(err)
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/v1/search/fast?q=glacier&collection=allowed", 200}, {"/api/v1/search/fast?q=glacier&collection=empty", 200}, {"/api/v1/search/fast?q=glacier&collection=mixed", 401}, {"/api/v1/cli/collections", 200}, {"/api/v1/cli/accounts", 200}} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(tc.code, w.Code, w.Body.String())
		if tc.code == 200 {
			assertions.NotContains(w.Body.String(), other.Identifier)
			assertions.NotContains(w.Body.String(), `"name":"mixed"`)
		}
	}
	for _, tc := range []struct {
		args []string
		code int
	}{{[]string{"search", "glacier", "--collection=allowed"}, 200}, {[]string{"search", "glacier", "-n", "5", "--explain"}, 200}, {[]string{"stats"}, 200}, {[]string{"search", "glacier", "--collection=mixed"}, 401}, {[]string{"show-message", "1"}, 401}, {[]string{"search", "glacier", "--config=/tmp/config"}, 400}, {[]string{"sync"}, 400}} {
		body, err := json.Marshal(CLIRunRequest{Args: tc.args})
		requirements.NoError(err)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(tc.code, w.Code, w.Body.String())
	}
	requirements.NoError(st.AddSourcesToCollection("allowed", []int64{other.ID}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/stats/total?collection=allowed", nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(401, w.Code, w.Body.String())
}

func TestAgentReadGrantDeniesUnlistedRoutes(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	_, secret, _, err := srv.agentGrants.Issue("all reads", []agentgrant.Permission{"search.read", "message.read", "attachment.read", "stats.read"}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}})
	requirements.NoError(err)
	spec := httptest.NewRecorder()
	srv.Router().ServeHTTP(spec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	var document struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	requirements.NoError(json.Unmarshal(spec.Body.Bytes(), &document))
	parameters := regexp.MustCompile(`\{[^}]+\}`)
	count := 0
	for path, methods := range document.Paths {
		if !strings.HasPrefix(path, "/api/v1/") {
			continue
		}
		for method, op := range methods {
			if op.OperationID == "" || delegatedOperationAllowed(op.OperationID) {
				continue
			}
			count++
			r := httptest.NewRequest(strings.ToUpper(method), parameters.ReplaceAllString(path, "1"), nil)
			r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234", count/65536, (count/256)%256, count%256)
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assertions.Equal(http.StatusUnauthorized, w.Code, op.OperationID+": "+w.Body.String())
		}
	}
	assertions.Greater(count, 10)
}

func TestAgentTokenPersistentAPI(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	newServer := func() *Server {
		return NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	}
	srv := newServer()
	request := func(server *Server, method, path, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if token == "" {
			r.Header.Set("X-Api-Key", "owner")
		} else {
			r.Header.Set(apiprotocol.AgentTokenHeader, token)
		}
		w := httptest.NewRecorder()
		server.Router().ServeHTTP(w, r)
		return w
	}
	future := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	var issued agentTokenIssueResponse
	for _, tc := range []struct {
		expires time.Time
		status  int
	}{{time.Time{}, 400}, {time.Now().Add(-time.Hour), 400}, {future, 201}} {
		body, err := json.Marshal(agentTokenIssueRequest{Label: "reader", Permissions: []string{"stats.read"}, SourceIDs: []int64{src.ID}, ExpiresAt: &tc.expires})
		requirements.NoError(err)
		w := request(srv, http.MethodPost, agentTokensPath, string(body), "")
		requirements.Equal(tc.status, w.Code, w.Body.String())
		if tc.status == 201 {
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &issued))
			requirements.NotNil(issued.ExpiresAt)
			assertions.True(future.Equal(*issued.ExpiresAt))
		}
	}
	restarted := newServer()
	w := request(restarted, http.MethodGet, "/api/v1/cli/stats", "", issued.Secret)
	requirements.Equal(200, w.Code, w.Body.String())
	w = request(restarted, http.MethodDelete, agentTokensPath+"/"+issued.ID, "", "")
	requirements.Equal(204, w.Code, w.Body.String())
	w = request(srv, http.MethodGet, "/api/v1/cli/stats", "", issued.Secret)
	assertions.Equal(401, w.Code, "revocation must affect the original server immediately")
	_, err = st.DB().Exec("DROP TABLE agent_grants")
	requirements.NoError(err)
	body, err := json.Marshal(agentTokenIssueRequest{Label: "reader", Permissions: []string{"stats.read"}, SourceIDs: []int64{src.ID}})
	requirements.NoError(err)
	w = request(restarted, http.MethodPost, agentTokensPath, string(body), "")
	assertions.Equal(500, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), "grant_store_failed")
}

// Delete after the real authorization lookup to reproduce an owner deletion
// between the middleware's message resolution and the handler's resolution.
type agentMessageDeletionStore struct {
	*store.Store

	removeID      int64
	replaceWith   *store.Message
	replacementID int64
}

func (s *agentMessageDeletionStore) GetMessageSourceContext(ctx context.Context, id int64) (*store.Source, error) {
	source, err := s.Store.GetMessageSourceContext(ctx, id)
	if err != nil || id != s.removeID {
		return source, err
	}
	s.removeID = 0
	_, err = s.DB().ExecContext(ctx, s.Rebind("DELETE FROM messages WHERE id=?"), id)
	if err != nil {
		return nil, fmt.Errorf("delete message during authorization: %w", err)
	}
	if s.replaceWith != nil {
		s.replacementID, err = s.UpsertMessage(s.replaceWith)
		if err != nil {
			return nil, fmt.Errorf("replace message during authorization: %w", err)
		}
		if err := s.UpsertMessageBody(s.replacementID, sql.NullString{String: "outside body", Valid: true}, sql.NullString{}); err != nil {
			return nil, fmt.Errorf("replace body during authorization: %w", err)
		}
	}
	return source, nil
}

func TestAgentCLIMessageChecksResolvedSource(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	allowed, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	outside, err := st.GetOrCreateSource("test", "outside@example.test")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(allowed.ID, "allowed-thread", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: allowed.ID, ConversationID: conv, SourceMessageID: "allowed-message", MessageType: "email"})
	requirements.NoError(err)
	conv, err = st.EnsureConversation(outside.ID, "outside-thread", "Synthetic")
	requirements.NoError(err)
	outsideID, err := st.UpsertMessage(&store.Message{SourceID: outside.ID, ConversationID: conv, SourceMessageID: strconv.FormatInt(id, 10), MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(outsideID, sql.NullString{String: "outside body", Valid: true}, sql.NullString{}))
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: &agentMessageDeletionStore{Store: st, removeID: id}, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}})
	requirements.NoError(err)
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/cli/message?id=%d", id), nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusUnauthorized, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), "outside body")
	assertions.Contains(w.Body.String(), "message.read")
}

func TestOwnerCLIRunUnavailablePrecedesValidation(t *testing.T) {
	st := testutil.NewTestStore(t)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	for _, tc := range []struct{ name, body string }{
		{"invalid JSON", "{"},
		{"empty args", `{"args":[]}`},
		{"read command", `{"args":["stats"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("X-Api-Key", "owner")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		})
	}
}

func TestAgentMessageChecksReusedArchiveID(t *testing.T) {
	for _, useEngine := range []bool{false, true} {
		t.Run(fmt.Sprintf("engine=%t", useEngine), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			if st.IsPostgreSQL() {
				t.Skip("PostgreSQL sequences do not reuse deleted message IDs")
			}
			allowed, err := st.GetOrCreateSource("test", "reader@example.test")
			requirements.NoError(err)
			outside, err := st.GetOrCreateSource("test", "outside@example.test")
			requirements.NoError(err)
			conv, err := st.EnsureConversation(allowed.ID, "allowed-thread", "Synthetic")
			requirements.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: allowed.ID, ConversationID: conv, SourceMessageID: "allowed-message", MessageType: "email"})
			requirements.NoError(err)
			conv, err = st.EnsureConversation(outside.ID, "outside-thread", "Synthetic")
			requirements.NoError(err)
			mutating := &agentMessageDeletionStore{Store: st, removeID: id, replaceWith: &store.Message{SourceID: outside.ID, ConversationID: conv, SourceMessageID: "outside-message", MessageType: "email"}}
			opts := ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: mutating, Logger: testLogger()}
			if useEngine {
				opts.Engine = query.NewEngine(st.DB(), false)
			}
			srv := NewServerWithOptions(opts)
			_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}})
			requirements.NoError(err)
			r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/messages/%d", id), nil)
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assertions.Equal(id, mutating.replacementID, "SQLite must reuse the deleted archive ID to exercise the regression")
			assertions.Equal(http.StatusUnauthorized, w.Code, w.Body.String())
			assertions.NotContains(w.Body.String(), "outside body")
			assertions.Contains(w.Body.String(), "message.read")
		})
	}
}

// Reproduce deletion and ID reuse with the real store and query engine.
type agentAttachmentReplacementStore struct {
	*store.Store

	attachmentID, outsideMessageID, replacementID int64
}

func (s *agentAttachmentReplacementStore) AgentAttachmentSourceIDsContext(ctx context.Context, id int64, hash string) ([]int64, error) {
	ids, err := s.Store.AgentAttachmentSourceIDsContext(ctx, id, hash)
	if err != nil || id == 0 || id != s.attachmentID {
		return ids, err
	}
	s.attachmentID = 0
	if _, err := s.DB().ExecContext(ctx, s.Rebind("DELETE FROM attachments WHERE id=?"), id); err != nil {
		return nil, fmt.Errorf("delete attachment during authorization: %w", err)
	}
	if err := s.UpsertAttachment(s.outsideMessageID, "outside.bin", "application/octet-stream", "outside.bin", strings.Repeat("b", 64), 20); err != nil {
		return nil, fmt.Errorf("replace attachment during authorization: %w", err)
	}
	if err := s.DB().QueryRowContext(ctx, s.Rebind("SELECT id FROM attachments WHERE message_id=?"), s.outsideMessageID).Scan(&s.replacementID); err != nil {
		return nil, fmt.Errorf("resolve replacement attachment: %w", err)
	}
	return ids, nil
}

func TestAgentAttachmentChecksReusedArchiveID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owner  bool
		status int
	}{{"grant", false, http.StatusUnauthorized}, {"owner", true, http.StatusOK}} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			if st.IsPostgreSQL() {
				t.Skip("PostgreSQL sequences do not reuse deleted attachment IDs")
			}
			allowed, err := st.GetOrCreateSource("test", "reader@example.test")
			requirements.NoError(err)
			outside, err := st.GetOrCreateSource("test", "outside@example.test")
			requirements.NoError(err)
			messageIDs := make([]int64, 2)
			for i, source := range []*store.Source{allowed, outside} {
				conversation, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
				requirements.NoError(err)
				messageIDs[i], err = st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conversation, SourceMessageID: "message", MessageType: "email"})
				requirements.NoError(err)
			}
			requirements.NoError(st.UpsertAttachment(messageIDs[0], "allowed.bin", "application/octet-stream", "allowed.bin", strings.Repeat("a", 64), 20))
			var id int64
			requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM attachments WHERE message_id=?"), messageIDs[0]).Scan(&id))
			mutating := &agentAttachmentReplacementStore{Store: st, attachmentID: id, outsideMessageID: messageIDs[1]}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: mutating, Engine: query.NewEngine(st.DB(), false), Logger: testLogger()})
			defer srv.agentGrants.Close()
			r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/attachments/%d", id), nil)
			if tc.owner {
				_, err := mutating.AgentAttachmentSourceIDsContext(t.Context(), id, "")
				requirements.NoError(err)
				r.Header.Set("X-Api-Key", "owner")
			} else {
				_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}})
				requirements.NoError(err)
				r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assertions.Equal(id, mutating.replacementID, "SQLite must reuse the attachment ID to exercise the regression")
			assertions.Equal(tc.status, w.Code, w.Body.String())
			if tc.owner {
				assertions.Contains(w.Body.String(), "outside.bin")
			} else {
				assertions.NotContains(w.Body.String(), "outside.bin")
				assertions.Contains(w.Body.String(), "attachment.read")
			}
		})
	}
}
