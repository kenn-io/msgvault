package api

import (
	"context"
	"crypto/sha256"
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/config"
	msgexport "go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type agentReadGateStore struct {
	*store.Store

	sourceContextMu            sync.Mutex
	onSources                  func()
	onStats                    func(context.Context)
	onQuick                    func()
	snapshotContext            context.Context
	sourceLists, sourceLookups atomic.Int32
}

func (s *agentReadGateStore) ListSourcesContext(ctx context.Context, kind string) ([]*store.Source, error) {
	s.sourceLists.Add(1)
	s.sourceContextMu.Lock()
	s.snapshotContext = ctx
	s.sourceContextMu.Unlock()
	sources, err := s.Store.ListSourcesContext(ctx, kind)
	if s.onSources != nil {
		s.onSources()
	}
	return sources, err
}

func (s *agentReadGateStore) GetSourceByIDContext(ctx context.Context, id int64) (*store.Source, error) {
	s.sourceLookups.Add(1)
	return s.Store.GetSourceByIDContext(ctx, id)
}

func (s *agentReadGateStore) GetStatsForScopeContext(ctx context.Context, ids []int64) (*store.Stats, error) {
	if s.onStats != nil {
		s.onStats(ctx)
	}
	return s.Store.GetStatsForScopeContext(ctx, ids)
}

func (s *agentReadGateStore) NeedsFTSBackfillQuickContext(ctx context.Context) bool {
	if s.onQuick != nil {
		s.onQuick()
	}
	return s.Store.NeedsFTSBackfillQuickContext(ctx)
}

func TestAgentReadSourceReplacementSnapshot(t *testing.T) {
	for _, route := range []string{"list", "search", "aggregate", "message", "store message", "thread", "domain", "stats", "subaggregate", "cached search"} {
		t.Run(route, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			src, id, err := testutil.CreateIndexedSourceMessage(st, "reader@example.test", "message", "glacier allowed", "allowed body")
			requirements.NoError(err)
			participant, err := st.EnsureParticipant(src.Identifier, "Synthetic", "example.test")
			requirements.NoError(err)
			requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{participant}, nil))
			ready, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			protected := &agentReadGateStore{Store: st, onSources: func() { once.Do(func() { close(ready); <-resume }) }}
			gate := NewSerialOperationGate()
			opts := ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), OperationGate: gate}
			if route == "cached search" {
				opts.Engine = newExploreDuckDBFixture(t)
			}
			if route == "store message" {
				opts.Engine = nil
			}
			srv := NewServerWithOptions(opts)
			t.Cleanup(srv.agentGrants.Close)
			_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead, agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
			requirements.NoError(err)
			path := map[string]string{"cached search": "/api/v1/search/fast?q=glacier", "domain": "/api/v1/search/domains?domains=example.test", "stats": "/api/v1/stats/total", "subaggregate": "/api/v1/aggregates/sub?view_type=labels&sender=reader%40example.test", "list": "/api/v1/messages", "search": "/api/v1/search/fast?q=glacier", "aggregate": "/api/v1/aggregates", "message": fmt.Sprintf("/api/v1/messages/%d", id), "store message": fmt.Sprintf("/api/v1/messages/%d", id), "thread": fmt.Sprintf("/api/v1/cli/message/thread?id=%d", id)}[route]
			get := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set(apiprotocol.AgentTokenHeader, secret)
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, r)
				return w
			}
			readDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { readDone <- get() }()
			<-ready
			mutationDone := make(chan error, 1)
			go func() {
				done, ok := gate.BeginRequestWorkContext(t.Context(), "source replacement")
				if !ok {
					mutationDone <- context.Canceled
					return
				}
				defer done()
				_, _, err := st.RemoveSourceSerialized(t.Context(), src.ID)
				if err == nil {
					var outside *store.Source
					if st.IsPostgreSQL() {
						_, err = st.DB().Exec(st.Rebind("INSERT INTO sources (id, source_type, identifier) OVERRIDING SYSTEM VALUE VALUES (?, ?, ?)"), src.ID, "test", "outside@example.test")
						outside = &store.Source{ID: src.ID}
					} else {
						outside, err = st.GetOrCreateSource("test", "outside@example.test")
					}
					if err == nil {
						var conv int64
						conv, err = st.EnsureConversation(outside.ID, "thread", "Synthetic")
						if err == nil {
							_, err = st.UpsertMessage(&store.Message{SourceID: outside.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email", Subject: sql.NullString{String: "glacier private", Valid: true}})
						}
					}
				}
				mutationDone <- err
			}()
			requirements.NoError(<-mutationDone)
			close(resume)
			response := <-readDone
			requirements.Equal(http.StatusOK, response.Code, response.Body.String())
			assertions.NotContains(response.Body.String(), "glacier private")
			if route == "stats" {
				var result TotalStatsResponse
				requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
				assertions.Equal(int64(1), result.MessageCount)
			} else if route != "aggregate" && route != "subaggregate" {
				assertions.Contains(response.Body.String(), "glacier allowed")
			}
			if route == "message" || route == "store message" {
				assertions.Contains(response.Body.String(), "allowed body")
			}
			if !st.IsPostgreSQL() {
				replacement, err := st.GetSourceByIdentifier("outside@example.test")
				requirements.NoError(err)
				requirements.Equal(src.ID, replacement.ID)
			}
			response = get()
			assertions.NotContains(response.Body.String(), "glacier private")
			assertions.NotContains(response.Body.String(), "outside@example.test")
		})
	}
}

func TestAgentAttachmentReplacementSnapshot(t *testing.T) {
	for _, route := range []string{"metadata", "content", "cli"} {
		t.Run(route, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			allowed, err := st.GetOrCreateSource("test", "reader@example.test")
			requirements.NoError(err)
			outside, err := st.GetOrCreateSource("test", "outside@example.test")
			requirements.NoError(err)
			var messages []int64
			for _, source := range []*store.Source{allowed, outside} {
				conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
				requirements.NoError(err)
				id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
				requirements.NoError(err)
				messages = append(messages, id)
			}
			hash := strings.Repeat("d", 64)
			requirements.NoError(st.UpsertAttachment(messages[1], "private.bin", "application/octet-stream", "dd/"+hash, hash, 7))
			rows, err := query.NewEngine(st.DB(), st.IsPostgreSQL()).GetAttachmentsByHash(t.Context(), hash)
			requirements.NoError(err)
			requirements.Len(rows, 1)
			cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
			blob, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
			requirements.NoError(err)
			requirements.NoError(os.MkdirAll(filepath.Dir(blob), 0700))
			requirements.NoError(os.WriteFile(blob, []byte("private"), 0600))
			ready, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			protected := &agentReadGateStore{Store: st, onSources: func() { once.Do(func() { close(ready); <-resume }) }}
			gate := NewSerialOperationGate()
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), OperationGate: gate})
			t.Cleanup(srv.agentGrants.Close)
			_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead}, []agentgrant.SourceRef{{ID: allowed.ID, Type: allowed.SourceType, Identifier: allowed.Identifier}}, time.Time{})
			requirements.NoError(err)
			path := map[string]string{"metadata": fmt.Sprintf("/api/v1/attachments/%d", rows[0].ID), "content": "/api/v1/attachments/" + hash + "/content", "cli": "/api/v1/cli/attachment?content_hash=" + hash}[route]
			readDone := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set(apiprotocol.AgentTokenHeader, secret)
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, r)
				readDone <- w
			}()
			<-ready
			mutationDone := make(chan error, 1)
			go func() {
				done, ok := gate.BeginRequestWorkContext(t.Context(), "attachment replacement")
				if !ok {
					mutationDone <- context.Canceled
					return
				}
				defer done()
				_, _, err := st.RemoveSourceSerialized(t.Context(), outside.ID)
				if err == nil {
					err = st.UpsertAttachment(messages[0], "allowed.bin", "application/octet-stream", "dd/"+hash, hash, 7)
				}
				mutationDone <- err
			}()
			requirements.NoError(<-mutationDone)
			close(resume)
			response := <-readDone
			requirements.Equal(http.StatusNotFound, response.Code, response.Body.String())
			assertions.NotContains(response.Body.String(), "private.bin")
		})
	}
}

type agentStreamBarrier struct {
	*httptest.ResponseRecorder

	started, resume chan struct{}
	once            sync.Once
}

func (w *agentStreamBarrier) Write(body []byte) (int, error) {
	w.once.Do(func() { close(w.started); <-w.resume })
	n, err := w.ResponseRecorder.Write(body)
	if err != nil {
		return n, fmt.Errorf("write attachment test response: %w", err)
	}
	return n, nil
}

func TestAgentSlowDownloadAllowsOwnerMutation(t *testing.T) {
	t.Run("packing", testAgentPackingSnapshot)
	for _, cli := range []bool{false, true} {
		t.Run(fmt.Sprintf("cli=%v", cli), func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource("test", "reader@example.test")
			requirements.NoError(err)
			conv, err := st.EnsureConversation(src.ID, "thread", "Synthetic")
			requirements.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
			requirements.NoError(err)
			hash := strings.Repeat("e", 64)
			requirements.NoError(st.UpsertAttachment(id, "allowed.bin", "application/octet-stream", "ee/"+hash, hash, 7))
			cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
			path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
			requirements.NoError(err)
			requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
			requirements.NoError(os.WriteFile(path, []byte("allowed"), 0600))
			gate := NewSerialOperationGate()
			protected := &agentReadGateStore{Store: st}
			srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), OperationGate: gate})
			t.Cleanup(srv.agentGrants.Close)
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
			requirements.NoError(err)
			route := "/api/v1/attachments/" + hash + "/content"
			if cli {
				route = "/api/v1/cli/attachment?content_hash=" + hash
			}
			w := &agentStreamBarrier{ResponseRecorder: httptest.NewRecorder(), started: make(chan struct{}), resume: make(chan struct{})}
			readDone := make(chan struct{})
			go func() {
				r := httptest.NewRequest(http.MethodGet, route, nil)
				r.Header.Set(apiprotocol.AgentTokenHeader, token)
				srv.Router().ServeHTTP(w, r)
				close(readDone)
			}()
			<-w.started
			mutated := make(chan error, 1)
			go func() {
				release, ok := gate.BeginRequestWorkContext(t.Context(), "remove source")
				if !ok {
					mutated <- context.Canceled
					return
				}
				defer release()
				_, _, err := st.RemoveSourceSerialized(t.Context(), src.ID)
				mutated <- err
			}()
			requirements.NoError(<-mutated)
			close(w.resume)
			<-readDone
			assertions.Equal(http.StatusOK, w.Code)
			assertions.Equal("allowed", w.Body.String())
		})
	}
}

func TestAgentSearchInputAndScopeMetadata(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	_, err = st.CreateCollection("Selected", "", []int64{src.ID})
	requirements.NoError(err)
	_, err = st.CreateCollection("Empty", "", []int64{src.ID})
	requirements.NoError(err)
	requirements.NoError(st.RemoveSourcesFromCollection("Empty", []int64{src.ID}))
	gate := NewSerialOperationGate()
	release, ok := gate.BeginLabeledWorkContext(t.Context(), "owner sync")
	requirements.True(ok)
	defer release()
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), OperationGate: gate})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	for _, owner := range []bool{false, true} {
		if owner {
			assertions.False(gate.HasRequestWaiters())
			release()
		}
		for _, tc := range []struct {
			path, label   string
			count, status int
		}{
			{"/api/v1/cli/search?q=", "", 0, 400},
			{"/api/v1/cli/search?q=glacier", "agent grant", 1, 200},
			{"/api/v1/cli/search?q=&account=reader%40example.test", src.Identifier, 1, 200},
			{"/api/v1/cli/search?q=&collection=Selected", "Selected", 1, 200},
			{"/api/v1/cli/search?q=&collection=Empty", "Empty", 0, 400},
			{"/api/v1/cli/stats?account=reader%40example.test", src.Identifier, 1, 200},
			{"/api/v1/cli/stats?collection=Selected", "Selected", 1, 200},
			{"/api/v1/cli/stats?collection=Empty", "Empty", 0, 400},
		} {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if owner {
				r.Header.Del(apiprotocol.AgentTokenHeader)
				r.Header.Set("Authorization", "Bearer owner")
			} else {
				r.Header.Set(apiprotocol.AgentTokenHeader, token)
			}
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, r)
			requirements.Equal(tc.status, w.Code, w.Body.String())
			if tc.status == 400 {
				code := "empty_search"
				if tc.label == "Empty" {
					code = "empty_scope"
				}
				assertions.Contains(w.Body.String(), code)
				continue
			}
			var scope struct {
				Label string `json:"scope_label"`
				Count int    `json:"scope_source_count"`
			}
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &scope))
			if !owner || tc.label != "agent grant" {
				assertions.Equal(tc.label, scope.Label)
			}
			if owner && tc.label == "agent grant" {
				assertions.Zero(scope.Count)
			} else {
				assertions.Equal(tc.count, scope.Count)
			}
		}
	}
	assertions.False(gate.HasRequestWaiters())
}

type boundedAgentIndexStore struct {
	*store.Store

	probeContext context.Context
	probeCalls   int
	repairs      int
	needs        bool
	failRepair   bool
	full         func(context.Context) (bool, error)
}

func (s *boundedAgentIndexStore) NeedsFTSBackfillQuick() bool                       { return false }
func (s *boundedAgentIndexStore) NeedsFTSBackfillQuickContext(context.Context) bool { return false }
func (s *boundedAgentIndexStore) NeedsFTSBackfillContext(ctx context.Context) (bool, error) {
	s.probeContext = ctx
	s.probeCalls++
	return s.full(ctx)
}
func (s *boundedAgentIndexStore) BackfillFTS(func(int64, int64)) (int64, error) {
	s.repairs++
	if s.failRepair {
		s.failRepair = false
		return 0, errors.New("synthetic repair failure")
	}
	s.needs = false
	return 1, nil
}

func TestAgentIndexProbeHasFiniteLifetime(t *testing.T) {
	for _, arrival := range []string{"healthy", "stale", "owner after request cancellation", "agent only", "owner before timeout", "owner after timeout", "owner first", "probe failure", "failed repair", "owner at exit", "owner probe failure", "gate refused"} {
		st := &boundedAgentIndexStore{Store: testutil.NewTestStore(t), needs: arrival != "healthy"}
		t.Run(arrival, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				exit := make(chan struct{})
				resume := make(chan struct{})
				st.failRepair = arrival == "failed repair"
				if arrival == "failed repair" {
					source, err := st.GetOrCreateSource("test", "hole@example.test")
					requirements.NoError(err)
					conv, err := st.EnsureConversation(source.ID, "hole", "Synthetic")
					requirements.NoError(err)
					for i := range 3 {
						if i != 1 {
							_, _, err := testutil.CreateIndexedSourceMessage(st.Store, source.Identifier, strconv.Itoa(i), "glacier", "interior body")
							requirements.NoError(err)
							continue
						}
						id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(i), MessageType: "email", Subject: sql.NullString{String: "glacier", Valid: true}})
						requirements.NoError(err)
						requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "interior body", Valid: true}, sql.NullString{}))
					}
					assertions.Equal(st.IsPostgreSQL(), st.Store.NeedsFTSBackfillQuickContext(t.Context()), "PostgreSQL detects interior holes; SQLite checks the tail")
					needed, err := st.Store.NeedsFTSBackfillContext(t.Context())
					requirements.NoError(err)
					assertions.True(needed)
				}

				st.full = func(ctx context.Context) (bool, error) {
					if _, bounded := ctx.Deadline(); bounded {
						if arrival == "healthy" || arrival == "stale" {
							return st.needs, nil
						}
						if arrival == "owner after request cancellation" {
							close(exit)
							<-resume
							return st.needs, nil
						}
						if arrival == "probe failure" {
							return false, errors.New("synthetic probe failure")
						}
						<-ctx.Done()
						if arrival == "owner at exit" {
							<-exit
						}
						return false, ctx.Err()
					}
					if arrival == "owner probe failure" {
						if st.probeCalls <= 2 {
							return false, errors.New("synthetic SQL probe failure")
						}
						<-resume
						return false, nil
					}
					if arrival == "owner first" && st.probeCalls == 1 {
						time.Sleep(QueryEndpointTimeout + time.Second)
					}
					return st.needs, nil
				}
				gate := NewSerialOperationGate()
				if arrival == "gate refused" {
					gate.StartDrain()
				}
				srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), OperationGate: gate})
				defer func() { requirements.NoError(srv.Shutdown(context.Background())); synctest.Wait() }()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if arrival == "owner probe failure" {
					req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=glacier", nil)
					req.RemoteAddr = "127.0.0.1:1234"
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, req)
					requirements.Equal(http.StatusOK, w.Code, w.Body.String())
					assertions.Contains(w.Body.String(), `"index_state":"checking"`)
				} else {
					srv.ensureCLISearchIndexAsync(bindCLIStoreContext(ctx, st), arrival == "owner first" || arrival == "failed repair" || arrival == "gate refused")
				}
				if arrival == "owner after request cancellation" {
					<-exit
					cancel()
					synctest.Wait()
					assertions.True(srv.ftsEnsureRunning.Load())
					srv.ensureCLISearchIndexAsync(st, true)
					close(resume)
				}
				synctest.Wait()
				if arrival == "healthy" || arrival == "stale" {
					assertions.Equal(arrival == "healthy", srv.ftsIndexComplete.Load())
					assertions.Zero(st.repairs)
					if arrival == "healthy" {
						assertions.Empty(srv.ensureCLISearchIndexAsync(st, false))
					} else {
						assertions.Equal(cliSearchIndexStateAwaitingOwner, srv.ensureCLISearchIndexAsync(st, false))
					}
					assertions.Equal(1, st.probeCalls)
					if arrival == "healthy" {
						return
					}
					srv.ensureCLISearchIndexAsync(st, true)
					synctest.Wait()
				}
				if arrival == "owner before timeout" {
					srv.ensureCLISearchIndexAsync(st, true)
				}
				time.Sleep(QueryEndpointTimeout + time.Second)
				synctest.Wait()
				if arrival == "owner at exit" {
					srv.ensureCLISearchIndexAsync(st, true)
					close(exit)
					synctest.Wait()
				}
				if arrival == "gate refused" {
					assertions.False(srv.ftsEnsureRunning.Load())
					assertions.False(srv.ftsIndexComplete.Load())
					assertions.Equal(cliSearchIndexStateAwaitingOwner, srv.ensureCLISearchIndexAsync(st, false))
					assertions.Zero(st.repairs)
					assertions.False(gate.HasRequestWaiters())
					_, _, held := gate.Holder()
					assertions.False(held)
					return
				}
				if arrival == "failed repair" {
					assertions.False(srv.ftsIndexComplete.Load())
					assertions.Equal(1, st.repairs)
					assertions.Equal(cliSearchIndexStateAwaitingOwner, srv.ensureCLISearchIndexAsync(st, false))
					assertions.Equal(cliSearchIndexStateBuilding, srv.ensureCLISearchIndexAsync(st, true))
					synctest.Wait()
				}
				assertions.False(srv.ftsEnsureRunning.Load())
				if arrival == "agent only" || arrival == "owner after timeout" || arrival == "probe failure" || arrival == "owner probe failure" {
					assertions.False(srv.ftsIndexComplete.Load())
					assertions.Equal(cliSearchIndexStateUnverified, srv.ensureCLISearchIndexAsync(st, false))
					assertions.Equal(1, st.probeCalls)
					assertions.Zero(st.repairs)
					if arrival == "owner probe failure" {
						for range 2 {
							req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=glacier", nil)
							req.RemoteAddr = "127.0.0.1:1234"
							w := httptest.NewRecorder()
							srv.Router().ServeHTTP(w, req)
							requirements.Equal(http.StatusOK, w.Code, w.Body.String())
							assertions.Contains(w.Body.String(), `"index_state":"unverified"`)
							synctest.Wait()
						}
						assertions.True(srv.ftsEnsureRunning.Load())
						close(resume)
						synctest.Wait()
						assertions.True(srv.ftsIndexComplete.Load())
						req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/search?q=glacier", nil)
						req.RemoteAddr = "127.0.0.1:1234"
						w := httptest.NewRecorder()
						srv.Router().ServeHTTP(w, req)
						requirements.Equal(http.StatusOK, w.Code, w.Body.String())
						assertions.NotContains(w.Body.String(), `"index_state"`)
						assertions.Equal(3, st.probeCalls)
						assertions.Zero(st.repairs)
						_, bounded := st.probeContext.Deadline()
						assertions.False(bounded)
						return
					}
					if arrival == "agent only" {
						requirements.ErrorIs(st.probeContext.Err(), context.DeadlineExceeded)
						w := httptest.NewRecorder()
						srv.handleCLIRebuildFTS(w, httptest.NewRequest(http.MethodPost, "/api/v1/cli/rebuild-fts", nil))
						assertions.NotContains(w.Body.String(), `"type":"error"`)
						assertions.True(srv.ftsIndexComplete.Load())
						return
					}
					srv.ensureCLISearchIndexAsync(st, true)
					synctest.Wait()
				}
				assertions.True(srv.ftsIndexComplete.Load())
				wantRepairs := 1
				if arrival == "failed repair" {
					wantRepairs = 2
				}
				assertions.Equal(wantRepairs, st.repairs)
				if arrival == "owner after request cancellation" {
					assertions.Equal(2, st.probeCalls)
				}
				_, bounded := st.probeContext.Deadline()
				assertions.False(bounded, "owner checks use daemon lifetime")
			})
		})
	}
}

func TestAgentFTSResponsesReportIndexState(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "unindexed", MessageType: "email", Subject: sql.NullString{String: "ordinary subject", Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageBody(id, sql.NullString{String: "hiddenneedle body", Valid: true}, sql.NullString{}))
	protected := &agentReadGateStore{Store: st}
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	get := func(path string, agent bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		if agent {
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
		} else {
			req.Header.Set("Authorization", "Bearer owner")
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	first := get("/api/v1/search/fast?q=hiddenneedle", true)
	requirements.Equal(200, first.Code, first.Body.String())
	assertions.Contains(first.Body.String(), `"index_state":"checking"`)
	requirements.Eventually(func() bool { return !srv.ftsEnsureRunning.Load() }, 10*time.Second, time.Millisecond)
	for _, path := range []string{
		"/api/v1/cli/search?q=hiddenneedle", "/api/v1/search?q=hiddenneedle", "/api/v1/search/fast?q=hiddenneedle",
		"/api/v1/search/deep?q=hiddenneedle", "/api/v1/search/deep?q=hiddenneedle&scope=body",
		"/api/v1/aggregates?search_query=hiddenneedle", "/api/v1/aggregates/sub?view_type=senders&key=example.test&search_query=hiddenneedle",
		"/api/v1/messages/filter?search_query=hiddenneedle", "/api/v1/stats/total?search_query=hiddenneedle",
	} {
		response := get(path, true)
		if st.IsPostgreSQL() && strings.Contains(path, "scope=body") {
			assertions.Equal(503, response.Code, response.Body.String())
			assertions.Contains(response.Body.String(), "body_search_index_unavailable")
			assertions.Contains(response.Body.String(), "rebuild-fts")
		} else {
			requirements.Equal(200, response.Code, path+response.Body.String())
			assertions.Contains(response.Body.String(), `"index_state":"awaiting_owner"`, path)
		}
		if !strings.Contains(path, "/cli/") {
			assertions.NotContains(get(path, false).Body.String(), `"index_state"`, "owner routes retain their original behavior")
		}
	}
	assertions.True(st.NeedsFTSBackfill(), "agent requests do not repair")
	const repairBudget = 10 * time.Second
	for _, manual := range []bool{false, true} {
		ready, resume := make(chan struct{}), make(chan struct{})
		protected.onSources = func() { close(ready); <-resume }
		oldDone := make(chan *httptest.ResponseRecorder, 1)
		go func() { oldDone <- get("/api/v1/search/fast?q=hiddenneedle", true) }()
		select {
		case <-ready:
		case <-time.After(repairBudget):
			requirements.FailNow("old snapshot did not reach admission")
		}
		if manual {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/rebuild-fts", nil)
			req.Header.Set("Authorization", "Bearer owner")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			requirements.Equal(200, w.Code, w.Body.String())
		} else {
			response := get("/api/v1/cli/search?q=hiddenneedle", false)
			requirements.Equal(200, response.Code, response.Body.String())
		}
		requirements.Eventually(func() bool { return srv.ftsIndexComplete.Load() }, repairBudget, time.Millisecond)
		close(resume)
		select {
		case old := <-oldDone:
			requirements.Equal(200, old.Code, old.Body.String())
			assertions.Contains(old.Body.String(), `"index_state":"unverified"`)
			if !manual {
				assertions.NotContains(old.Body.String(), "hiddenneedle body")
			}
		case <-time.After(repairBudget):
			requirements.FailNow("old snapshot did not finish")
		}
		protected.onSources = nil
		fresh := get("/api/v1/search/fast?q=hiddenneedle", true)
		requirements.Equal(200, fresh.Code, fresh.Body.String())
		assertions.Contains(fresh.Body.String(), "hiddenneedle")
		assertions.NotContains(fresh.Body.String(), `"index_state"`)
	}
	response := get("/api/v1/search/deep?q=hiddenneedle&scope=body", true)
	requirements.Equal(200, response.Code, response.Body.String())
	assertions.Contains(response.Body.String(), "hiddenneedle body")
	assertions.NotContains(response.Body.String(), `"index_state"`)
}

func TestAgentSearchDeadlineReleasesSnapshot(t *testing.T) {
	const requestBudget = 2 * time.Second
	const transitionBudget = 10 * time.Second
	for _, route := range []struct{ method, path string }{{http.MethodGet, "/api/v1/cli/search?q=needle"}, {http.MethodHead, "/api/v1/cli/search?q=needle"}, {http.MethodGet, "/api/v1/stats"}, {http.MethodGet, "/api/v1/cli/stats"}} {
		path := route.path
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s %s/canceled=%t", route.method, path, canceled), func(t *testing.T) {
				requirements := require.New(t)
				st := testutil.NewTestStore(t)
				source, err := st.GetOrCreateSource("test", "reader@example.test")
				requirements.NoError(err)
				ready := make(chan struct{})
				protected := &agentReadGateStore{Store: st}
				if strings.Contains(path, "search") {
					protected.onSources = func() { close(ready); <-protected.snapshotContext.Done() }
				} else {
					protected.onStats = func(ctx context.Context) { close(ready); <-ctx.Done() }
				}
				srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger(), RequestTimeout: requestBudget})
				t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
				t.Cleanup(srv.agentGrants.Close)
				srv.ftsIndexComplete.Store(true)
				_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionSearchRead, agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
				requirements.NoError(err)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := httptest.NewRequest(route.method, path, nil).WithContext(ctx)
				req.Header.Set(apiprotocol.AgentTokenHeader, token)
				responses := make(chan *httptest.ResponseRecorder, 1)
				go func() {
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, req)
					responses <- w
				}()
				select {
				case <-ready:
				case <-time.After(transitionBudget):
					requirements.FailNow("read did not acquire its snapshot")
				}
				requirements.Equal(1, st.DB().Stats().InUse)
				deadline, bounded := protected.snapshotContext.Deadline()
				requirements.True(bounded)
				requirements.LessOrEqual(time.Until(deadline), requestBudget)
				code := "query_timeout"
				if canceled {
					code = "query_canceled"
					cancel()
				}
				select {
				case response := <-responses:
					requireErrorCode(t, response, http.StatusServiceUnavailable, code)
				case <-time.After(transitionBudget):
					requirements.FailNow("read did not honor its context")
				}
				requirements.Eventually(func() bool { return st.DB().Stats().InUse == 0 }, transitionBudget, time.Millisecond)
				requirements.NoError(st.DB().QueryRowContext(t.Context(), "SELECT 1").Scan(new(int)))
			})
		}
	}
}

func TestAgentReadSnapshotPoolPressure(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		permission agentgrant.Permission
	}{
		{"aggregates", "/api/v1/aggregates", agentgrant.PermissionSearchRead},
		{"search", "/api/v1/cli/search?q=needle", agentgrant.PermissionSearchRead},
		{"stats", "/api/v1/stats", agentgrant.PermissionStatsRead},
		{"cli stats", "/api/v1/cli/stats", agentgrant.PermissionStatsRead},
		{"html", "", agentgrant.PermissionMessageRead},
		{"html store", "", agentgrant.PermissionMessageRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			st.DB().SetMaxOpenConns(4)
			source, err := st.GetOrCreateSource("test", "reader@example.test")
			requirements.NoError(err)
			conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
			requirements.NoError(err)
			id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
			requirements.NoError(err)
			imageURL := "https://images.example.test/chart.png"
			key := fmt.Sprintf("remote-image:%x", sha256.Sum256([]byte(imageURL)))
			requirements.NoError(st.UpsertMessageBody(id, sql.NullString{}, sql.NullString{String: `<img src="` + imageURL + `">`, Valid: true}))
			requirements.NoError(st.UpsertRemoteImageAttachment(t.Context(), id, store.AttachmentWrite{SourcePartKey: key, SourceAttachmentID: key, ContentID: key, StoragePath: "images/chart.png", ContentHash: strings.Repeat("a", 64), MIMEType: "image/png"}))
			ownerStats, err := st.GetStatsForScope([]int64{source.ID})
			requirements.NoError(err)
			assertions.Positive(ownerStats.DatabaseSize)
			path := tc.path
			if strings.HasPrefix(tc.name, "html") {
				path = fmt.Sprintf("/api/v1/messages/%d", id)
			}
			ready, resume := make(chan struct{}, 4), make(chan struct{})
			barrier := &agentReadGateStore{Store: st, onSources: func() { ready <- struct{}{}; <-resume }}
			engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
			if tc.name == "html store" {
				engine = nil
			}
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: barrier, Engine: engine, Logger: testLogger()})
			t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
			t.Cleanup(srv.agentGrants.Close)
			_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{tc.permission}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
			requirements.NoError(err)
			responses := make(chan *httptest.ResponseRecorder, 4)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			for range 4 {
				go func() {
					req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
					req.Header.Set(apiprotocol.AgentTokenHeader, token)
					w := httptest.NewRecorder()
					srv.Router().ServeHTTP(w, req)
					responses <- w
				}()
			}
			for range agentReadConcurrency {
				select {
				case <-ready:
				case <-ctx.Done():
					requirements.FailNow("agent snapshots did not reach the barrier")
				}
			}
			// Waiting agent reads hold no connection, so owner work keeps the rest of the pool.
			assertions.Equal(agentReadConcurrency, st.DB().Stats().InUse)
			ownerPath := "/api/v1/stats"
			if tc.name == "search" {
				ownerPath = tc.path
			}
			ownerReq := httptest.NewRequest(http.MethodGet, ownerPath, nil).WithContext(ctx)
			ownerReq.Header.Set("Authorization", "Bearer owner")
			ownerResponse := httptest.NewRecorder()
			srv.Router().ServeHTTP(ownerResponse, ownerReq)
			requirements.Equal(200, ownerResponse.Code, ownerResponse.Body.String())
			close(resume)
			for range 4 {
				select {
				case response := <-responses:
					requirements.Equal(200, response.Code, response.Body.String())
					if strings.HasPrefix(tc.name, "html") {
						assertions.Contains(response.Body.String(), "cid:"+key)
					} else if tc.permission == agentgrant.PermissionStatsRead {
						var stats StatsResponse
						if tc.name == "cli stats" {
							var result cliStatsResponse
							requirements.NoError(json.Unmarshal(response.Body.Bytes(), &result))
							stats = result.Stats
						} else {
							requirements.NoError(json.Unmarshal(response.Body.Bytes(), &stats))
						}
						assertions.Equal(int64(1), stats.TotalMessages)
						assertions.Nil(stats.DatabaseSize)
						assertions.NotContains(response.Body.String(), "database_size_bytes")
					}
				case <-ctx.Done():
					requirements.FailNow("scoped reads did not release their snapshots")
				}
			}
			requirements.Eventually(func() bool { return st.DB().Stats().InUse == 0 }, 10*time.Second, time.Millisecond)
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(canceled)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			srv.Router().ServeHTTP(httptest.NewRecorder(), req)
			assertions.Zero(st.DB().Stats().InUse)
		})
	}
}

type agentPackingResolver struct {
	packstore.Resolver

	before func(context.Context)
	calls  int
}

func (r *agentPackingResolver) Resolve(ctx context.Context, hash packstore.Hash) (packstore.Location, error) {
	location, err := r.Resolver.Resolve(ctx, hash)
	if err != nil {
		return location, fmt.Errorf("resolve test placement: %w", err)
	}
	r.calls++
	if r.before != nil {
		before := r.before
		r.before = nil
		before(ctx)
	}
	return location, nil
}

type agentPackingStream struct {
	*attachmentstore.Store

	before func(context.Context)
}

func (s *agentPackingStream) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	if s.before != nil {
		s.before(ctx)
	}
	return s.Store.OpenStream(ctx, hash)
}

func testAgentPackingSnapshot(t *testing.T) {
	for _, route := range []string{"content", "cli", "cid"} {
		for _, transition := range []string{"pack before open", "pack during retry", "repack before open", "repack during retry", "cancel"} {
			t.Run(route+"/"+transition, func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				st := testutil.NewTestStore(t)
				source, err := st.GetOrCreateSource("test", "reader@example.test")
				requirements.NoError(err)
				conv, err := st.EnsureConversation(source.ID, "thread", "Synthetic")
				requirements.NoError(err)
				id, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
				requirements.NoError(err)
				content := []byte("packing preserves authorized bytes")
				if route == "cid" {
					content = fakePNG
				}
				hash := fmt.Sprintf("%x", sha256.Sum256(content))
				cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
				loose, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
				requirements.NoError(err)
				requirements.NoError(os.MkdirAll(filepath.Dir(loose), 0700))
				requirements.NoError(os.WriteFile(loose, content, 0600))
				cid := "remote-image:chart"
				if route == "cid" {
					requirements.NoError(st.UpsertRemoteImageAttachment(t.Context(), id, store.AttachmentWrite{SourcePartKey: cid, SourceAttachmentID: cid, ContentID: cid, StoragePath: loose, ContentHash: hash, MIMEType: "image/png"}))
				} else {
					requirements.NoError(st.UpsertAttachment(id, "allowed.bin", "application/octet-stream", loose, hash, len(content)))
				}
				record := func(entry store.PackIndexEntry) store.PackRecord {
					return store.PackRecord{PackID: entry.PackID, EntryCount: 1, StoredBytes: entry.StoredLen, CreatedAt: time.Now()}
				}
				var old store.PackIndexEntry
				if strings.HasPrefix(transition, "repack") {
					old = buildTestPack(t, cfg.AttachmentsDir(), content)
					requirements.NoError(st.RecordPackedBlobs(record(old), []store.PackIndexEntry{old}))
					requirements.NoError(os.Remove(loose))
				}
				protected := &agentReadGateStore{Store: st}
				resolver := &agentPackingResolver{Resolver: store.NewPackCatalog(st)}
				physical, err := attachmentstore.New(resolver, cfg.AttachmentsDir())
				requirements.NoError(err)
				t.Cleanup(func() { requirements.NoError(physical.Close()) })
				stream := &agentPackingStream{Store: physical}
				ctx, cancel := context.WithTimeout(context.WithValue(t.Context(), readPackingValueKey{}, "request value"), 10*time.Second)
				defer cancel()
				migrate := func(readCtx context.Context) {
					requirements.ErrorIs(store.ReadDBContext(protected.snapshotContext, st.DB()).QueryRowContext(t.Context(), "SELECT 1").Scan(new(int)), sql.ErrTxDone)
					assertions.Equal("request value", readCtx.Value(readPackingValueKey{}))
					_, deadline := readCtx.Deadline()
					assertions.True(deadline)
					assertions.Zero(st.DB().Stats().InUse, "authorization connection released before placement")
					if transition == "cancel" {
						cancel()
						return
					}
					next := buildTestPack(t, cfg.AttachmentsDir(), content)
					if old.PackID == "" {
						requirements.NoError(st.RecordPackedBlobs(record(next), []store.PackIndexEntry{next}))
						requirements.NoError(os.Remove(loose))
					} else {
						requirements.NoError(st.CommitRepack(t.Context(), []string{old.PackID}, []store.PackRecord{record(next)}, []store.RepackMove{{OldPackID: old.PackID, NewEntry: next}}))
						requirements.NoError(physical.RetirePack(old.PackID))
						oldPath := filepath.Join(cfg.AttachmentsDir(), "packs", old.PackID[:2], old.PackID+packstore.PackExt)
						assertions.NoFileExists(oldPath)
					}
				}
				if strings.Contains(transition, "retry") {
					resolver.before = migrate
				} else {
					stream.before = migrate
				}
				srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: protected, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), BlobStore: stream, Logger: testLogger()})
				t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
				_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionAttachmentRead, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
				requirements.NoError(err)
				path := "/api/v1/attachments/" + hash + "/content"
				if route == "cli" {
					path = "/api/v1/cli/attachment?content_hash=" + hash
				}
				if route == "cid" {
					path = fmt.Sprintf("/api/v1/messages/%d/inline?cid=%s", id, cid)
				}
				req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
				req.Header.Set(apiprotocol.AgentTokenHeader, token)
				response := httptest.NewRecorder()
				if route == "cid" {
					snapshot, release, err := st.BeginReadSnapshotContext(ctx)
					requirements.NoError(err)
					defer release()
					protected.snapshotContext = snapshot
					requirements.NoError(store.ReadDBContext(snapshot, st.DB()).QueryRowContext(snapshot, st.Rebind("SELECT source_id FROM messages WHERE id=?"), id).Scan(new(int64)))
					boundary := &readResponseWriter{ResponseWriter: response, release: release}
					req = req.WithContext(context.WithValue(snapshot, readResponseKey{}, boundary))
					req.SetPathValue("id", strconv.FormatInt(id, 10))
					srv.handleMessageInline(boundary, req)
				} else {
					srv.Router().ServeHTTP(response, req)
				}
				if transition == "cancel" {
					assertions.NotEqual(http.StatusOK, response.Code)
					assertions.NotEqual(content, response.Body.Bytes())
				} else {
					requirements.Equal(http.StatusOK, response.Code, response.Body.String())
					assertions.Equal(content, response.Body.Bytes())
					expectedCalls := 1
					if strings.Contains(transition, "retry") {
						expectedCalls = 2
					}
					assertions.Equal(expectedCalls, resolver.calls)
				}
				assertions.Zero(st.DB().Stats().InUse)
			})
		}
	}
}

type readPackingValueKey struct{}

func TestAgentReadAtCapacityFailsWhenRequestEnds(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("test", "reader@example.test")
	requirements.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	for range agentReadConcurrency {
		srv.agentReadSlots <- struct{}{}
	}

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil).WithContext(ended)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assertions := assert.New(t)
	assertions.Equal(http.StatusServiceUnavailable, w.Code)
	assertions.Contains(w.Body.String(), "agent_read_busy")
	assertions.Zero(st.DB().Stats().InUse)
	assertions.Len(srv.agentReadSlots, agentReadConcurrency)
}
