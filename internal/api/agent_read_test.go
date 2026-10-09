package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestAgentReadAuthorization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	requestCount := 0
	st := testutil.NewTestStore(t)
	sources := make([]*store.Source, 2)
	ids := make([]int64, 2)
	for i, identifier := range []string{"allowed@example.test", "other@example.test"} {
		var err error
		sources[i], ids[i], err = testutil.CreateIndexedSourceMessage(st, identifier, strconv.Itoa(i), fmt.Sprintf("glacier source-%d", i), "")
		requirements.NoError(err)
		participant, err := st.EnsureParticipant(identifier, "", "example.test")
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(ids[i], "from", []int64{participant}, nil))
	}
	src, other := sources[0], sources[1]
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

			_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.Permission(tc.permission)}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
			requirements.NoError(err)
			for _, access := range []struct {
				name, token string
				code        int
			}{{"allowed", secret, 200}, {"missing permission", "", http.StatusForbidden}} {
				t.Run(access.name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)

					token := access.token
					if token == "" {
						_, token, _, err = srv.agentGrants.Issue("wrong", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
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
	_, secret, _, err := srv.agentGrants.Issue("all reads", []agentgrant.Permission{"search.read", "stats.read", "message.read", "attachment.read"}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		path string
		code int
	}{
		{fmt.Sprintf("/api/v1/messages/%d", ids[1]), http.StatusNotFound},
		{fmt.Sprintf("/api/v1/cli/attachment?id=%d&content_hash=%s", attID, outsideHash), http.StatusNotFound},
		{"/api/v1/attachments/" + outsideHash + "/content", http.StatusNotFound},
		{"/api/v1/cli/attachment?content_hash=" + strings.Repeat("c", 64), http.StatusNotFound},
		{"/api/v1/attachments/" + strings.Repeat("c", 64) + "/content", http.StatusNotFound},
	} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		requestCount++
		r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", requestCount)
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		assertions.Equal(tc.code, w.Code, tc.path+": "+w.Body.String())
	}
	// An object outside the grant must be indistinguishable from a missing one.
	for _, pair := range [][2]string{
		{fmt.Sprintf("/api/v1/messages/%d", ids[1]), "/api/v1/messages/999999"},
		{"/api/v1/attachments/" + outsideHash + "/content", "/api/v1/attachments/" + strings.Repeat("c", 64) + "/content"},
		{"/api/v1/cli/attachment?content_hash=" + outsideHash, "/api/v1/cli/attachment?content_hash=" + strings.Repeat("c", 64)},
	} {
		bodies := [2]string{}
		for i, path := range pair {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			requestCount++
			r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", requestCount)
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			assertions.Equal(http.StatusNotFound, w.Code, path+": "+w.Body.String())
			bodies[i] = w.Body.String()
		}
		assertions.Equal(bodies[1], bodies[0], pair[0])
	}
	_, multiToken, _, err := srv.agentGrants.Issue("multiple sources", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}, {ID: other.ID, Type: other.SourceType, Identifier: other.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, path := range []string{"/api/v1/cli/search?q=glacier", "/api/v1/cli/search?q=glacier&collection=mixed"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, multiToken)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), "glacier source-0")
		assertions.Contains(w.Body.String(), "glacier source-1")
	}
}

func TestAgentReadSourceFilterPrecedence(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	sources := make([]*store.Source, 3)
	refs := make([]agentgrant.SourceRef, 2)
	for i := range sources {
		subject := fmt.Sprintf("glacier source-%d", i)
		source, id, err := testutil.CreateIndexedSourceMessage(st, fmt.Sprintf("source-%d@example.test", i), "message", subject, subject)
		requirements.NoError(err)
		sources[i] = source
		participant, err := st.EnsureParticipant(source.Identifier, "", "example.test")
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
		if i == 0 {
			conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
			requirements.NoError(err)
			extra, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "second", MessageType: "email", Subject: sql.NullString{String: "glacier source-0-extra", Valid: true}})
			requirements.NoError(err)
			requirements.NoError(st.UpsertFTS(extra, "glacier source-0-extra", "", source.Identifier, "", ""))
		}
		if i < len(refs) {
			refs[i] = agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}
		}
	}
	for _, coll := range []struct {
		name string
		ids  []int64
	}{{"allowed", []int64{sources[0].ID, sources[1].ID}}, {"one", []int64{sources[0].ID}}, {"mixed", []int64{sources[1].ID, sources[2].ID}}, {"empty", []int64{sources[0].ID}}} {
		_, err := st.CreateCollection(coll.name, "", coll.ids)
		requirements.NoError(err)
	}
	requirements.NoError(st.RemoveSourcesFromCollection("empty", []int64{sources[0].ID}))
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionStatsRead}, refs, time.Time{})
	requirements.NoError(err)
	requests := 0
	for _, route := range []struct {
		name, path, want string
		owner            bool
	}{
		{"CLI search", "/api/v1/cli/search?q=glacier", "glacier source-1", false},
		{"search", "/api/v1/search?q=glacier", "glacier source-1", false},
		{"fast search", "/api/v1/search/fast?q=glacier", "glacier source-1", true},
		{"deep search", "/api/v1/search/deep?q=glacier", "glacier source-1", false},
		{"filter", "/api/v1/messages/filter?limit=10", "glacier source-1", true},
		{"list", "/api/v1/messages?page=1", "glacier source-1", false},
		{"aggregates", "/api/v1/aggregates?view_type=senders", sources[1].Identifier, true},
		{"subaggregates", "/api/v1/aggregates/sub?view_type=senders", sources[1].Identifier, true},
		{"domains", "/api/v1/search/domains?domains=example.test", "glacier source-1", true},
		{"totals", "/api/v1/stats/total?hide_deleted=true", `"message_count":1`, true},
		{"stats", "/api/v1/stats?unused=1", `"total_messages":1`, false},
		{"CLI stats", "/api/v1/cli/stats?unused=1", `"total_messages":1`, false},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, scope string
				status      int
			}{
				{"plural overrides scalar", fmt.Sprintf("source_id=%d&source_ids=%d", sources[0].ID, sources[1].ID), 200},
				{"ignored ungranted scalar", fmt.Sprintf("source_id=%d&source_ids=%d", sources[2].ID, sources[1].ID), 200},
				{"scalar", fmt.Sprintf("source_id=%d", sources[1].ID), 200},
				{"repeated plural", fmt.Sprintf("source_id=%d&source_ids=%d&source_ids=%d,%d", sources[0].ID, sources[1].ID, sources[1].ID, sources[1].ID), 200},
				{"malformed ignored scalar", fmt.Sprintf("source_id=bad&source_ids=%d", sources[1].ID), 400},
				{"scalar is not a list", fmt.Sprintf("source_id=%d,%d&source_ids=%d", sources[0].ID, sources[1].ID, sources[1].ID), 400},
				{"malformed plural", fmt.Sprintf("source_id=%d&source_ids=bad", sources[0].ID), 400},
				{"ungranted effective source", fmt.Sprintf("source_id=%d&source_ids=%d", sources[0].ID, sources[2].ID), 403},
				{"mixed collection cannot narrow", fmt.Sprintf("collection=mixed&source_ids=%d", sources[1].ID), 403},
				{"allowed collection narrowing", fmt.Sprintf("collection=allowed&source_id=%d&source_ids=%d", sources[0].ID, sources[1].ID), 200},
				{"ignored scalar outside collection", fmt.Sprintf("collection=allowed&source_id=%d&source_ids=%d", sources[2].ID, sources[1].ID), 200},
				{"ignored scalar outside account", fmt.Sprintf("account=%s&source_id=%d&source_ids=%d", sources[1].Identifier, sources[0].ID, sources[1].ID), 200},
				{"outside collection", fmt.Sprintf("collection=one&source_ids=%d", sources[1].ID), 403},
				{"outside account", fmt.Sprintf("account=%s&source_ids=%d", sources[0].Identifier, sources[1].ID), 403},
				{"empty collection cannot widen", fmt.Sprintf("collection=empty&source_ids=%d", sources[1].ID), 403},
			} {
				t.Run(tc.name, func(t *testing.T) {
					assertions := assert.New(t)
					requirements := require.New(t)
					for _, owner := range []bool{false, true} {
						if owner && (!route.owner || tc.name != "plural overrides scalar") {
							continue
						}
						req := httptest.NewRequest(http.MethodGet, route.path+"&"+tc.scope, nil)
						requests++
						req.RemoteAddr = fmt.Sprintf("10.1.%d.%d:1234", requests/256, requests%256)
						if owner {
							req.Header.Set("Authorization", "Bearer owner")
						} else {
							req.Header.Set(apiprotocol.AgentTokenHeader, token)
						}
						w := httptest.NewRecorder()
						srv.Router().ServeHTTP(w, req)
						requirements.Equal(tc.status, w.Code, w.Body.String())
						switch tc.status {
						case 200:
							assertions.Contains(w.Body.String(), route.want)
							assertions.NotContains(w.Body.String(), "glacier source-0")
							assertions.NotContains(w.Body.String(), sources[0].Identifier)
							assertions.NotContains(w.Body.String(), sources[2].Identifier)
						case 400:
							assertions.Contains(w.Body.String(), "invalid_source")
						default:
							assertions.Contains(w.Body.String(), "permission_denied")
						}
					}
				})
			}
		})
	}
}

func TestAgentCollections(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "scoped@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("test", "outside@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(src.ID, "Selected archive"))
	conversation, err := st.EnsureConversation(src.ID, "thread", "Synthetic")
	requirements.NoError(err)
	for _, key := range []string{"live", "deleted"} {
		_, err = st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation, SourceMessageID: key, MessageType: "email"})
		requirements.NoError(err)
	}
	requirements.NoError(st.MarkMessageDeleted(src.ID, "deleted"))
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
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{"search.read", "stats.read"}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		path string
		code int
	}{{"/api/v1/search/fast?q=glacier&collection=empty", 200}, {"/api/v1/cli/collections", 200}} {
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		requirements.Equal(tc.code, w.Code, w.Body.String())
		if tc.code == 200 {
			assertions.NotContains(w.Body.String(), other.Identifier)
			assertions.NotContains(w.Body.String(), `"name":"mixed"`)
			if tc.path == "/api/v1/cli/collections" {
				assertions.NotContains(w.Body.String(), `"name":"empty"`)
				var response cliCollectionsResponse
				requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
				var allowed *cliCollectionResponse
				for i := range response.Collections {
					if response.Collections[i].Name == "allowed" {
						allowed = &response.Collections[i]
					}
				}
				requirements.NotNil(allowed)
				assertions.Equal(int64(1), allowed.MessageCount)
				assertions.Equal(int64(1), allowed.SourceDeletedCount)
				requirements.Len(allowed.Sources, 1)
				assertions.Equal("Selected archive", allowed.Sources[0].DisplayName)
			}
		}
	}
	requirements.NoError(st.AddSourcesToCollection("allowed", []int64{other.ID}))
	r := httptest.NewRequest(http.MethodGet, "/api/v1/stats/total?collection=allowed", nil)
	r.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, r)
	assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
}

type agentThreadErrorEngine struct {
	query.Engine
	query.OriginalMessageReader

	err error
}

func (e *agentThreadErrorEngine) ListThread(context.Context, query.ThreadQuery) (*query.ThreadPage, error) {
	return nil, e.err
}

func TestAgentThreadErrors(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "reader@example.test")
	require.NoError(t, err)
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	originalReader, ok := engine.(query.OriginalMessageReader)
	require.True(t, ok)
	for _, tc := range []struct {
		name, path, code string
		status           int
		err              error
	}{
		{"invalid", "", "invalid_request", http.StatusBadRequest, nil},
		{"missing message", "?id=999999", "message_not_found", http.StatusNotFound, nil},
		{"missing thread", "?thread_id=missing", "thread_not_found", http.StatusNotFound, nil},
		{"backend", "?id=1", "internal_error", http.StatusInternalServerError, errors.New("synthetic backend failure")},
		{"cancelled", "?id=1", "query_canceled", http.StatusServiceUnavailable, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			reader := engine
			if tc.err != nil {
				reader = &agentThreadErrorEngine{Engine: engine, OriginalMessageReader: originalReader, err: tc.err}
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: reader, Logger: testLogger()})
			t.Cleanup(srv.agentGrants.Close)
			_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
			requirements.NoError(err)
			r := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/thread"+tc.path, nil)
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			w := httptest.NewRecorder()
			if tc.err == nil {
				srv.Router().ServeHTTP(w, r)
			} else {
				r = r.WithContext(context.WithValue(r.Context(), agentReadScopeKey{}, agentReadScope{ids: []int64{source.ID}}))
				srv.handleCLIMessageThread(w, r)
			}
			requireErrorCode(t, w, tc.status, tc.code)
			assertions.NotContains(w.Body.String(), "outside@example.test")
		})
	}
}

type observedThreadEngine struct {
	query.Engine
	query.OriginalMessageReader

	pages []*query.ThreadPage
}

func (e *observedThreadEngine) ListThread(ctx context.Context, q query.ThreadQuery) (*query.ThreadPage, error) {
	page, err := e.OriginalMessageReader.ListThread(ctx, q)
	if page != nil {
		e.pages = append(e.pages, page)
	}
	return page, err
}

func TestAgentThreadOutsideGrantLoadsNothing(t *testing.T) {
	requirements := require.New(t)
	f := newCLIOriginalFixture(t)
	allowed, err := f.st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	engine := f.srv.QueryEngine()
	originalReader, ok := engine.(query.OriginalMessageReader)
	requirements.True(ok)
	observed := &observedThreadEngine{Engine: engine, OriginalMessageReader: originalReader}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: f.st, Engine: observed, Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, secret, grant, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}}, time.Time{})
	requirements.NoError(err)

	owner := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/cli/message/thread?id=%d&all=true", f.withRaw), nil)
	owner.Header.Set("X-Api-Key", "owner")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, owner)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	requirements.Len(observed.pages, 1)
	requirements.Len(observed.pages[0].Messages, 2)

	for _, selector := range []string{
		fmt.Sprintf("id=%d&all=true", f.withRaw),
		fmt.Sprintf("id=%d&limit=500", f.withRaw),
		fmt.Sprintf("id=%d&limit=1&offset=1", f.withRaw),
		"source_message_id=provider-original&all=true",
		"thread_id=thread-original&all=true",
	} {
		t.Run(selector, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			observed.pages = nil
			r := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/thread?"+selector, nil)
			r.Header.Set(apiprotocol.AgentTokenHeader, secret)
			requirements.Nil(srv.authorizeAgentRead(r, "getCLIMessageThread", &grant, agentgrant.PermissionMessageRead, 0, false))
			w := httptest.NewRecorder()
			srv.handleCLIMessageThread(w, r)
			code := cliErrorMessageNotFound
			if strings.HasPrefix(selector, "thread_id=") {
				code = "thread_not_found"
			}
			requireErrorCode(t, w, http.StatusNotFound, code)
			assertions.Empty(observed.pages, "an ungranted thread must not be loaded")
			assertions.NotContains(w.Body.String(), "provider-original")
		})
	}
}

func TestAgentTokenExpiryAPI(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	defer srv.agentGrants.Close()
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
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
		srv.Router().ServeHTTP(w, r)
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
		w := request(http.MethodPost, agentTokensPath, string(body), "")
		requirements.Equal(tc.status, w.Code, w.Body.String())
		if tc.status == 201 {
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &issued))
			requirements.NotNil(issued.ExpiresAt)
			assertions.True(future.Equal(*issued.ExpiresAt))
		}
	}
	w := request(http.MethodGet, "/api/v1/cli/stats", "", issued.Secret)
	requirements.Equal(200, w.Code, w.Body.String())
	w = request(http.MethodGet, agentTokensPath, "", "")
	requirements.Equal(200, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), `"expires_at"`)
	w = request(http.MethodDelete, agentTokensPath+"/"+issued.ID, "", "")
	requirements.Equal(204, w.Code, w.Body.String())
	w = request(http.MethodGet, "/api/v1/cli/stats", "", issued.Secret)
	assertions.Equal(401, w.Code, "revocation must take effect immediately")
}

func TestAgentTokenSelf(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	ref := []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}
	readerID, reader, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, ref, time.Time{})
	requirements.NoError(err)
	_, drafter, _, err := srv.agentGrants.Issue("drafter", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, ref, time.Time{})
	requirements.NoError(err)
	get := func(header, value string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, agentTokensPath+"/self", nil)
		r.Header.Set(header, value)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}

	w := get(apiprotocol.AgentTokenHeader, reader)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var view agentTokenView
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &view))
	assertions.Equal(readerID, view.ID)
	assertions.Equal([]string{"message.read"}, view.Permissions)
	assertions.NotContains(w.Body.String(), reader, "the view must not echo the secret")

	w = get(apiprotocol.AgentTokenHeader, drafter)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.Contains(w.Body.String(), `"draft.create"`)
	assertions.NotContains(w.Body.String(), `"message.read"`)

	requireErrorCode(t, get("X-Api-Key", "owner"), http.StatusBadRequest, "agent_token_required")
	assertions.Equal(http.StatusUnauthorized, get(apiprotocol.AgentTokenHeader, "not-a-token").Code)
}

func TestAgentReadScopeCounts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	other, err := st.GetOrCreateSource("test", "stats@example.test")
	requirements.NoError(err)
	conv, err := st.EnsureConversationWithType(src.ID, "chat", "group_chat", "Synthetic")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "chat-1", MessageType: "whatsapp"})
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "deleted-email", MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(st.MarkMessageDeleted(src.ID, "deleted-email"))
	requirements.NoError(st.UpdateSourceDisplayName(src.ID, "Reader archive"))
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: newExploreDuckDBFixture(t), Logger: testLogger()})
	defer srv.agentGrants.Close()
	get := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}

	// The list total counts the same message types the list returns.
	_, reader, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	w := get("/api/v1/messages", reader)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var list MessageListResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &list))
	requirements.Len(list.Messages, 1)
	assertions.Equal(int64(1), list.Total)

	w = get("/api/v1/cli/accounts", reader)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var accounts cliAccountsResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &accounts))
	requirements.Len(accounts.Accounts, 1)
	assertions.Equal(src.ID, accounts.Accounts[0].ID)
	assertions.Equal(int64(1), accounts.Accounts[0].MessageCount)
	assertions.Equal(int64(1), accounts.Accounts[0].SourceDeletedCount)
	assertions.Equal("Reader archive", accounts.Accounts[0].DisplayName)

	// Discovery lists a source granted for any read permission.
	_, split, _, err := srv.agentGrants.Issue("split", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}, {ID: other.ID, Type: other.SourceType, Identifier: other.Identifier}}, time.Time{})
	requirements.NoError(err)
	_, statsOnly, _, err := srv.agentGrants.Issue("stats only", []agentgrant.Permission{agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: other.ID, Type: other.SourceType, Identifier: other.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, token := range []string{split, statsOnly} {
		w = get("/api/v1/cli/accounts", token)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		assertions.Contains(w.Body.String(), other.Identifier)
	}

	// A grant whose sources no longer exist covers zero accounts, not one.
	_, gone, _, err := srv.agentGrants.Issue("gone", []agentgrant.Permission{agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: 999, Type: "test", Identifier: "removed@example.test"}}, time.Time{})
	requirements.NoError(err)
	w = get("/api/v1/cli/stats", gone)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), "scope_source_count")
	var cliStats cliStatsResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &cliStats))
	assertions.Zero(cliStats.Stats.TotalAccounts)
	w = get("/api/v1/stats", gone)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var plain StatsResponse
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &plain))
	assertions.Zero(plain.TotalAccounts)
}

func TestAgentAttachmentCanonicalFallback(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	allowed, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	outside, err := st.GetOrCreateSource("test", "outside@example.test")
	requirements.NoError(err)
	hash := strings.Repeat("c", 64)
	var attachmentIDs []int64
	for _, source := range []*store.Source{outside, allowed} {
		conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
		requirements.NoError(err)
		id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
		requirements.NoError(err)
		filename := "allowed.bin"
		if source.ID == outside.ID {
			filename = "private.bin"
		}
		requirements.NoError(st.UpsertAttachment(id, filename, "application/octet-stream", "cc/"+hash, hash, 8))
		var attachmentID int64
		requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM attachments WHERE message_id=?"), id).Scan(&attachmentID))
		attachmentIDs = append(attachmentIDs, attachmentID)
	}
	// Legacy canonical paths still identify blobs when content_hash is NULL or empty.
	_, err = st.DB().Exec(st.Rebind("UPDATE attachments SET content_hash=NULL WHERE id=?"), attachmentIDs[1])
	requirements.NoError(err)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
	requirements.NoError(os.WriteFile(path, []byte("fallback"), 0600))
	protected := &agentReadGateStore{Store: st}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	defer srv.agentGrants.Close()
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}}, time.Time{})
	requirements.NoError(err)
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, r)
		return w
	}
	var privateMessageID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT message_id FROM attachments WHERE id=?"), attachmentIDs[0]).Scan(&privateMessageID))
	for i := range 24 {
		requirements.NoError(st.UpsertAttachment(privateMessageID, fmt.Sprintf("private-%d.bin", i), "application/octet-stream", "cc/"+hash, hash, 8))
	}
	for _, empty := range []any{nil, ""} {
		_, err = st.DB().Exec(st.Rebind("UPDATE attachments SET content_hash=? WHERE id=?"), empty, attachmentIDs[1])
		requirements.NoError(err)
		for _, path := range []string{"/api/v1/cli/attachment?content_hash=" + hash, "/api/v1/attachments/" + hash + "/content"} {
			before := protected.sourceLists.Load()
			w := get(path)
			assertions.Equal(before+1, protected.sourceLists.Load())
			assertions.Zero(protected.sourceLookups.Load())
			requirements.Equal(http.StatusOK, w.Code, w.Body.String())
			assertions.Equal("fallback", w.Body.String())
			assertions.NotContains(w.Header().Get("Content-Disposition"), "private.bin")
			if strings.Contains(path, "/content") {
				assertions.Contains(w.Header().Get("Content-Disposition"), "allowed.bin")
			}
		}
	}
	w := get(fmt.Sprintf("/api/v1/attachments/%d", attachmentIDs[0]))
	assertions.Equal(http.StatusNotFound, w.Code, w.Body.String())
	assertions.NotContains(w.Body.String(), "private.bin")
}

func TestAgentMessageReferenceScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	sources := make([]*store.Source, 2)
	ids := make([]int64, 2)
	for i, name := range []string{"outside@example.test", "allowed@example.test"} {
		var err error
		sources[i], err = st.GetOrCreateSource("test", name)
		requirements.NoError(err)
		conv, err := st.EnsureConversation(sources[i].ID, "thread", "Synthetic")
		requirements.NoError(err)
		ids[i], err = st.UpsertMessage(&store.Message{SourceID: sources[i].ID, ConversationID: conv, SourceMessageID: "shared", MessageType: "email"})
		requirements.NoError(err)
		participant, err := st.EnsureParticipant(name, "", "example.test")
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(ids[i], "from", []int64{participant}, nil))
	}
	outsideConv, err := st.EnsureConversation(sources[0].ID, "outside", "Synthetic")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&store.Message{SourceID: sources[0].ID, ConversationID: outsideConv, SourceMessageID: "outside-only", MessageType: "email"})
	requirements.NoError(err)
	conv, err := st.EnsureConversation(sources[1].ID, "numeric", "Synthetic")
	requirements.NoError(err)
	numericSourceMessage, err := st.UpsertMessage(&store.Message{SourceID: sources[1].ID, ConversationID: conv, SourceMessageID: strconv.FormatInt(ids[0], 10), MessageType: "email"})
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: sources[1].ID, Type: sources[1].SourceType, Identifier: sources[1].Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, tc := range []struct {
		ref   string
		agent bool
		code  int
		id    int64
	}{
		{"shared", true, 200, ids[1]}, {"shared", false, 200, ids[0]},
		// An ungranted internal ID falls through to the granted message with that source ID.
		{strconv.FormatInt(ids[0], 10), true, 200, numericSourceMessage}, {strconv.FormatInt(ids[0], 10), false, 200, ids[0]},
		{"missing", true, 404, 0}, {"outside-only", true, 404, 0},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message?id="+tc.ref, nil)
		if tc.agent {
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
		} else {
			req.Header.Set("Authorization", "Bearer owner")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(tc.code, w.Code, w.Body.String())
		if tc.id != 0 {
			var response cliMessageResponse
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
			assertions.Equal(tc.id, response.ID)
		}
	}
	for i, tc := range []struct {
		path string
		code int
	}{
		{"source_message_id=shared", 200}, {"thread_id=thread", 200},
		{"source_message_id=outside-only", 404}, {"thread_id=outside", 404},
		{"id=999999&account=" + sources[1].Identifier, 404},
		{"source_message_id=missing&account=" + sources[1].Identifier, 404},
		{"thread_id=missing&account=" + sources[1].Identifier, 404},
		{"id=999999&account=unknown@example.test", 403},
		{"source_message_id=missing&account=unknown@example.test", 403},
		{"thread_id=missing&account=unknown@example.test", 403},
		{"id=999999&account=" + sources[0].Identifier, 403},
		{"source_message_id=missing&account=" + sources[0].Identifier, 403},
		{"thread_id=missing&account=" + sources[0].Identifier, 403},
		{"source_message_id=shared&account=" + sources[0].Identifier, 403},
		{"id=" + strconv.FormatInt(ids[0], 10), 404},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/thread?"+tc.path, nil)
		req.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", i+1)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(tc.code, w.Code, w.Body.String())
		assertions.NotContains(w.Body.String(), sources[0].Identifier)
		if tc.code == 200 {
			var page struct {
				SourceID int64 `json:"source_id"`
				Messages []struct {
					ID int64 `json:"id"`
				} `json:"messages"`
			}
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &page))
			assertions.Equal(sources[1].ID, page.SourceID)
			requirements.Len(page.Messages, 1)
			assertions.Equal(ids[1], page.Messages[0].ID)
		}
	}
	_, both, _, err := srv.agentGrants.Issue("both", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: sources[0].ID, Type: sources[0].SourceType, Identifier: sources[0].Identifier}, {ID: sources[1].ID, Type: sources[1].SourceType, Identifier: sources[1].Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, ref := range []string{"source_message_id=shared", "thread_id=thread"} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/thread?"+ref, nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, both)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requireErrorCode(t, w, 409, "message_ambiguous")
		assertions.NotContains(w.Body.String(), sources[0].Identifier)
		assertions.NotContains(w.Body.String(), sources[1].Identifier)
	}
	for _, scope := range []struct {
		path string
		id   int64
	}{
		{"source_id=" + strconv.FormatInt(sources[0].ID, 10), ids[0]},
		{"source_id=" + strconv.FormatInt(sources[0].ID, 10) + "&source_ids=" + strconv.FormatInt(sources[1].ID, 10), ids[1]},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search/domains?domains=example.test&"+scope.path, nil)
		req.Header.Set("Authorization", "Bearer owner")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(200, w.Code, w.Body.String())
		var response FilteredMessagesResponse
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &response))
		requirements.Len(response.Messages, 1)
		assertions.Equal(scope.id, response.Messages[0].ID)
	}
}
