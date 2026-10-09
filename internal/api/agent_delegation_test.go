package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
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

// allowedDelegatedOps is the exact set. Keep in sync with delegatedOperationAllowed.
var allowedDelegatedOps = []string{"runCLI", "getHealth", "controlCalendar", "getAgentTokenSelf"}

// stubSourceResolverStore wraps mockStore and adds GetSourceByIDContext.
type stubSourceStore struct {
	mockStore

	src    *store.Source
	srcErr error
}

func (s *stubSourceStore) GetSourceByIDContext(_ context.Context, _ int64) (*store.Source, error) {
	return s.src, s.srcErr
}

func newTestServerWithAgentGrants(t *testing.T) (*Server, *agentgrant.Registry) {
	t.Helper()
	reg := agentgrant.NewRegistry()
	cfg := &config.Config{
		Server: config.ServerConfig{APIKey: "owner-key"},
	}
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     &stubSourceStore{},
		Logger:    testLogger(),
		Scheduler: newMockScheduler(),
	})
	srv.agentGrants = reg
	return srv, reg
}

// TestDelegatedFailsPrivilegedPredicate tests proof matrix row 2.
// apiRequestAuthorized returns false for a delegated request at all six call
// sites: the predicate itself, pprof guard, backup-freeze, getStats,
// listMessages, and issueAgentToken.
func TestDelegatedFailsPrivilegedPredicate(t *testing.T) {
	t.Parallel()
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("priv-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
	require.NoError(t, err)

	makeRequest := func(method, path string) *http.Request {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		return req
	}

	t.Run("apiRequestAuthorized returns false", func(t *testing.T) {
		req := makeRequest(http.MethodGet, "/api/v1/stats")
		assert.False(t, srv.apiRequestAuthorized(req), "delegated mode must not satisfy apiRequestAuthorized")
	})

	t.Run("pprof returns 404 to delegated loopback caller", func(t *testing.T) {
		req := makeRequest(http.MethodGet, "/debug/pprof/")
		req.RemoteAddr = "127.0.0.1:4242"
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "pprof must be hidden from delegated callers")
	})

	t.Run("backup freeze begin returns 401", func(t *testing.T) {
		req := makeRequest(http.MethodPost, "/api/v1/backup/freeze/begin")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "beginBackupFreeze must deny delegated callers")
	})

	t.Run("getStats returns 403 without its read permission", func(t *testing.T) {
		req := makeRequest(http.MethodGet, "/api/v1/stats")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("listMessages returns 403 without its read permission", func(t *testing.T) {
		req := makeRequest(http.MethodGet, "/api/v1/messages")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusForbidden, w.Code)
	})

	t.Run("issueAgentToken returns 401", func(t *testing.T) {
		req := makeRequest(http.MethodPost, "/api/v1/agent-tokens")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestOwnerPathsUnchangedWithoutAgentHeader tests proof matrix row 3 (api-side).
// A request without an agent token header uses normal owner authentication
// paths, behaving identically to before the feature was added.
func TestOwnerPathsUnchangedWithoutAgentHeader(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServerWithAgentGrants(t)

	t.Run("owner API key without agent header gets normal response", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
		req.Header.Set("X-Api-Key", "owner-key")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "owner requests must reach handlers unchanged")
	})

	t.Run("no auth without agent header returns 401 as before", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/stats", nil)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestAgentTokenNeverFallsBack tests proof matrix row 7.
// Every bad-credential shape — unknown secret, empty value, duplicated header,
// revoked grant, owner credential alongside agent token —
// gets 401 and never falls through to a success mode.
func TestAgentTokenNeverFallsBack(t *testing.T) {
	t.Parallel()
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	grantID, validSecret, _, err := reg.Issue("fallback-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
	require.NoError(t, err)

	// getHealth is in allowedDelegatedOps, so a VALID token returns 200.
	// Any bad shape must return 401 instead.
	makeHealthReq := func(setup func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		setup(req)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}

	t.Run("unknown secret", func(t *testing.T) {
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, "mva1_completelyunknown")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("empty value", func(t *testing.T) {
		w := makeHealthReq(func(req *http.Request) {
			req.Header[http.CanonicalHeaderKey(apiprotocol.AgentTokenHeader)] = []string{""}
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("duplicated header", func(t *testing.T) {
		w := makeHealthReq(func(req *http.Request) {
			req.Header[http.CanonicalHeaderKey(apiprotocol.AgentTokenHeader)] = []string{validSecret, validSecret}
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("revoked grant", func(t *testing.T) {
		revoked := reg.Revoke(grantID)
		require.True(t, revoked)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, validSecret)
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("owner credential alongside agent token", func(t *testing.T) {
		_, newSecret, _, issErr := reg.Issue("with-key", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set("X-Api-Key", "mva1_spoofed_prefix_value")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("Authorization header alongside agent token", func(t *testing.T) {
		_, newSecret, _, issErr := reg.Issue("with-auth", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set("Authorization", "Bearer spoofed-owner-key")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "Authorization alongside agent token must classify AuthModeRequired")
	})

	t.Run("session cookie alongside agent token", func(t *testing.T) {
		_, newSecret, _, issErr := reg.Issue("with-cookie", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.AddCookie(&http.Cookie{
				Name:     sessionCookieName,
				Value:    "spoofed-session-token",
				Secure:   true,
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "session cookie alongside agent token must classify AuthModeRequired")
	})

	t.Run("daemon runtime token alongside agent token", func(t *testing.T) {
		_, newSecret, _, issErr := reg.Issue("with-daemon-token", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, "spoofed-daemon-token")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "daemon runtime token alongside agent token must classify AuthModeRequired")
	})
}

// TestDelegatedOperationAllowlistIsClosed compares every registered API operation with an independent policy.
func TestDelegatedOperationAllowlistIsClosed(t *testing.T) {
	t.Parallel()
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := &config.Config{Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	var src *store.Source
	var id int64
	for _, account := range []string{"allowed@example.test", "outside@example.test"} {
		subject := "glacier " + account
		source, messageID, err := testutil.CreateIndexedSourceMessage(st, account, "message", subject, subject)
		requirements.NoError(err)
		participant, err := st.EnsureParticipant(account, "", "example.test")
		requirements.NoError(err)
		requirements.NoError(st.ReplaceMessageRecipients(messageID, "from", []int64{participant}, nil))
		if src == nil {
			src, id = source, messageID
		}
	}
	_, err := st.CreateCollection("Selected", "", []int64{src.ID})
	requirements.NoError(err)
	hash := strings.Repeat("a", 64)
	path, err := msgexport.StoragePath(cfg.AttachmentsDir(), hash)
	requirements.NoError(err)
	requirements.NoError(os.MkdirAll(filepath.Dir(path), 0700))
	requirements.NoError(os.WriteFile(path, []byte("permitted bytes"), 0600))
	requirements.NoError(st.UpsertAttachment(id, "permitted.bin", "application/octet-stream", filepath.ToSlash(path), hash, 15))
	var attachmentID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind("SELECT id FROM attachments WHERE message_id=?"), id).Scan(&attachmentID))
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(func() { requirements.NoError(srv.Shutdown(context.Background())) })
	t.Cleanup(srv.agentGrants.Close)
	_, secret, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionSearchRead, agentgrant.PermissionMessageRead, agentgrant.PermissionAttachmentRead, agentgrant.PermissionStatsRead}, []agentgrant.SourceRef{{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier}}, time.Time{})
	requirements.NoError(err)
	// These expectations stay independent of the production permission map.
	reads := map[string]struct{ path, want string }{
		"searchCLI":               {"/api/v1/cli/search?q=glacier", src.Identifier},
		"searchMessages":          {"/api/v1/search?q=glacier", src.Identifier},
		"fastSearch":              {"/api/v1/search/fast?q=glacier", src.Identifier},
		"deepSearch":              {"/api/v1/search/deep?q=glacier", src.Identifier},
		"getAggregates":           {"/api/v1/aggregates?view_type=senders", src.Identifier},
		"getSubAggregates":        {"/api/v1/aggregates/sub?view_type=senders", src.Identifier},
		"filterMessages":          {"/api/v1/messages/filter", src.Identifier},
		"listMessages":            {"/api/v1/messages", src.Identifier},
		"searchMessagesByDomains": {"/api/v1/search/domains?domains=example.test", src.Identifier},
		"getStats":                {"/api/v1/stats", `"total_messages":1`},
		"getCLIStats":             {"/api/v1/cli/stats", `"total_messages":1`},
		"getTotalStats":           {"/api/v1/stats/total", `"message_count":1`},
		"getCLIMessageThread":     {fmt.Sprintf("/api/v1/cli/message/thread?id=%d", id), src.Identifier},
		"getMessage":              {fmt.Sprintf("/api/v1/messages/%d", id), src.Identifier},
		"getCLIMessage":           {fmt.Sprintf("/api/v1/cli/message?id=%d", id), src.Identifier},
		"getAttachment":           {fmt.Sprintf("/api/v1/attachments/%d", attachmentID), "permitted.bin"},
		"getAttachmentContent":    {"/api/v1/attachments/" + hash + "/content", "permitted bytes"},
		"getCLIAttachment":        {"/api/v1/cli/attachment?content_hash=" + hash, "permitted bytes"},
		"listCLIAccounts":         {"/api/v1/cli/accounts", src.Identifier},
		"listCLICollections":      {"/api/v1/cli/collections", `"name":"Selected"`},
	}
	allowed := make(map[string]bool, len(allowedDelegatedOps))
	for _, op := range allowedDelegatedOps {
		allowed[op] = true
	}
	specRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(specRec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	requirements.Equal(http.StatusOK, specRec.Code)
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	requirements.NoError(json.Unmarshal(specRec.Body.Bytes(), &spec))
	requirements.NotEmpty(spec.Paths)
	pathParamRE := regexp.MustCompile(`\{[^}]+\}`)
	seen := make(map[string]bool)
	denied, requests, testedReads := 0, 0, 0
	for rawPath, methods := range spec.Paths {
		if !strings.HasPrefix(rawPath, "/api/v1/") {
			continue
		}
		for method, op := range methods {
			if op.OperationID == "" {
				continue
			}
			seen[op.OperationID] = true
			testPath := pathParamRE.ReplaceAllString(rawPath, "1")
			read, isRead := reads[op.OperationID]
			if isRead {
				testPath = read.path
				assertions.Equal("get", method)
			}
			req := httptest.NewRequest(strings.ToUpper(method), testPath, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234", requests/65536, (requests/256)%256, requests%256)
			requests++
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			if isRead {
				testedReads++
				requirements.Equal(http.StatusOK, w.Code, op.OperationID+": "+w.Body.String())
				assertions.Contains(w.Body.String(), read.want, op.OperationID)
				if op.OperationID == "getAttachmentContent" || op.OperationID == "getCLIAttachment" {
					assertions.Equal(read.want, w.Body.String())
				}
				assertions.NotContains(w.Body.String(), "outside@example.test", op.OperationID)
			} else if allowed[op.OperationID] {
				assertions.NotEqual(http.StatusUnauthorized, w.Code, op.OperationID)
			} else {
				assertions.Equal(http.StatusUnauthorized, w.Code, op.OperationID)
				denied++
			}
		}
	}
	for op := range reads {
		assertions.True(seen[op], "missing read operation: "+op)
	}
	for op := range allowed {
		assertions.True(seen[op], "missing preexisting delegated operation: "+op)
	}
	assertions.Len(reads, 20)
	assertions.Equal(len(reads), testedReads)
	assertions.Greater(denied, 10)
}

// TestDelegationNotReachableOverHTTP tests proof matrix row 21.
// Delegation cannot enable or widen access over HTTP: settings routes (which
// expose agent_access and imap.drafts) return 401 for any delegated caller.
func TestDelegationNotReachableOverHTTP(t *testing.T) {
	t.Parallel()
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("http-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
	require.NoError(t, err)

	denied := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/settings"},
		{http.MethodPatch, "/api/v1/settings"},
		{http.MethodGet, "/api/v1/accounts"},
		{http.MethodPost, "/api/v1/accounts"},
		{http.MethodGet, "/api/v1/scheduler/status"},
	}

	for _, route := range denied {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			req := httptest.NewRequest(route.method, route.path, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			assert.Equal(t, http.StatusUnauthorized, w.Code,
				"route %s %s must deny delegated callers", route.method, route.path)
		})
	}
}

// TestDelegatedDraftAcquiresOperationGate tests the P1 operation-gate fix.
// A delegated POST /api/v1/cli/run for draft-reply must register as a gate
// waiter (gate label: "msgvault draft-reply"); an unauthenticated request with
// the same body must bypass the gate entirely and return without waiting.
func TestDelegatedDraftAcquiresOperationGate(t *testing.T) { //nolint:paralleltest // subtests take turns holding one shared operation gate
	var gate LabeledOperationGate = NewSerialOperationGate()
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner-key"}}
	srv := NewServerWithOptions(ServerOptions{
		Config:        cfg,
		Store:         &stubSourceStore{},
		Logger:        testLogger(),
		Scheduler:     newMockScheduler(),
		OperationGate: gate,
	})
	reg := agentgrant.NewRegistry()
	srv.agentGrants = reg
	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("gate-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
	require.NoError(t, err)

	t.Run("delegated request is gate eligible, owner predicate returns false", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		assert.True(t, srv.requestGateEligible(req), "delegated request must be gate eligible")
		assert.False(t, srv.apiRequestAuthorized(req), "delegated request must not satisfy the owner predicate")
	})

	t.Run("unauthenticated request is not gate eligible", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", nil)
		assert.False(t, srv.requestGateEligible(req), "unauthenticated request must not be gate eligible")
	})

	t.Run("delegated draft-reply registers as gate waiter with label msgvault draft-reply", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			gate := NewSerialOperationGate()
			srv := NewServerWithOptions(ServerOptions{
				Config:        &config.Config{Server: config.ServerConfig{APIKey: "owner-key"}},
				Store:         &stubSourceStore{},
				Logger:        testLogger(),
				Scheduler:     newMockScheduler(),
				OperationGate: gate,
			})
			defer func() {
				synctest.Wait()
				require.NoError(t, srv.Shutdown(context.Background()), "shutdown")
			}()

			reg := agentgrant.NewRegistry()
			srv.agentGrants = reg
			src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
			_, secret, _, err := reg.Issue("gate-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
			require.NoError(t, err)

			hold, ok := gate.BeginWork()
			require.True(t, ok, "must acquire the gate to hold it for this subtest")
			releaseHold := func() {
				if hold != nil {
					hold()
					hold = nil
				}
			}
			defer releaseHold()

			body := `{"args":["draft-reply","--from","alice@example.com"]}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			w := httptest.NewRecorder()
			reqDone := make(chan struct{})
			go func() {
				defer close(reqDone)
				srv.Router().ServeHTTP(w, req)
			}()

			synctest.Wait()
			assert.True(t, gate.HasRequestWaiters(),
				"delegated draft-reply must register as gate waiter (label: msgvault draft-reply)")

			releaseHold()
			synctest.Wait()
			<-reqDone
		})
	})

	t.Run("unauthenticated draft-reply does not register as gate waiter", func(t *testing.T) {
		done, ok := gate.BeginWork()
		require.True(t, ok)
		defer done()

		body := `{"args":["draft-reply","--from","alice@example.com"]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		reqDone := make(chan struct{})
		go func() {
			defer close(reqDone)
			srv.Router().ServeHTTP(w, req)
		}()

		// requestGateEligible returns false for AuthModeRequired, so the gate is
		// bypassed and the request returns immediately (401 from the auth layer).
		select {
		case <-reqDone:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "unauthenticated request must not block on the operation gate")
		}
		assert.False(t, gate.HasRequestWaiters(),
			"unauthenticated request must not register as a gate waiter")
	})

	// A gated route outside the two-operation allowlist gets 401 from the auth
	// layer without ever queuing as a waiter.
	t.Run("delegated non-allowlisted route does not register as gate waiter", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		done, ok := gate.BeginWork()
		require.True(ok, "must acquire the gate to hold it for this subtest")
		defer done()

		req := httptest.NewRequest(http.MethodPost, "/api/v1/accounts", nil)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()

		reqDone := make(chan struct{})
		go func() {
			defer close(reqDone)
			srv.Router().ServeHTTP(w, req)
		}()

		select {
		case <-reqDone:
		case <-time.After(5 * time.Second):
			require.FailNow("delegated request on non-allowlisted gated route must not block on the operation gate")
		}
		assert.Equal(http.StatusUnauthorized, w.Code,
			"delegated caller on non-allowlisted route must get 401, not 503")
		assert.False(gate.HasRequestWaiters(),
			"delegated caller on non-allowlisted route must not register as a gate waiter")
	})

	// A non-draft-reply body bypasses the gate entirely, gets
	// command_not_allowed, and cannot change the holder label. Proof-matrix row 32.
	t.Run("delegated non-draft-reply does not register as gate waiter", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		holderDone, ok := gate.BeginRequestWorkContext(context.Background(), "owner-msgvault-sync")
		require.True(ok)
		defer holderDone()

		body := `{"args":["sync","alice@example.com"]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(apiprotocol.AgentTokenHeader, secret)
		w := httptest.NewRecorder()

		reqDone := make(chan struct{})
		go func() {
			defer close(reqDone)
			srv.Router().ServeHTTP(w, req)
		}()

		select {
		case <-reqDone:
		case <-time.After(5 * time.Second):
			require.FailNow("delegated non-draft-reply must not block on the operation gate")
		}
		assert.False(gate.HasRequestWaiters(),
			"delegated non-draft-reply must not register as a gate waiter")
		var resp ErrorResponse
		require.NoError(json.NewDecoder(w.Body).Decode(&resp))
		assert.Equal("command_not_allowed", resp.Error,
			"delegated non-draft-reply must return command_not_allowed")

		// The gate label must not reflect caller-supplied args.
		label, _, held := gate.Holder()
		assert.True(held, "gate must still be held by the owner")
		assert.Equal("owner-msgvault-sync", label,
			"gate label must not be overwritten by the delegated caller's args")
	})
}

// TestDelegatedGateBusyRedactsHolderLabel verifies that when a delegated caller
// times out on the /api/v1/cli/run gate the 503 body does not contain the
// internal holder label (which names configured account identifiers).
func TestDelegatedGateBusyRedactsHolderLabel(t *testing.T) { //nolint:paralleltest // swaps the package-level operationGateWaitLimit
	assert := assert.New(t)
	require := require.New(t)
	var gate LabeledOperationGate = NewSerialOperationGate()
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner-key"}}
	srv := NewServerWithOptions(ServerOptions{
		Config:        cfg,
		Store:         &stubSourceStore{},
		Logger:        testLogger(),
		Scheduler:     newMockScheduler(),
		OperationGate: gate,
	})
	reg := agentgrant.NewRegistry()
	srv.agentGrants = reg
	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("label-redact", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Time{})
	require.NoError(err)

	// Acquire gate with an identifiable holder label.
	holderDone, ok := gate.BeginRequestWorkContext(context.Background(), "owner-msgvault-sync")
	require.True(ok)
	defer holderDone()

	// Override the wait limit so the test doesn't take 10 s.
	orig := operationGateWaitLimit
	operationGateWaitLimit = time.Millisecond
	t.Cleanup(func() { operationGateWaitLimit = orig })

	body := `{"args":["draft-reply","--from","alice@example.com"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(apiprotocol.AgentTokenHeader, secret)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)

	assert.Equal(http.StatusServiceUnavailable, w.Code)
	body503 := w.Body.String()
	assert.Contains(body503, "operation_in_progress",
		"busy response must use operation_in_progress code")
	assert.NotContains(body503, "owner-msgvault-sync",
		"holder label must be redacted for delegated callers")
}

// TestDelegationDefaultOff verifies that delegation is off when agent_access is
// unset in config. The constructor (server.go:596-601) must leave agentGrants nil,
// and a presented agent token must be refused with 401.
func TestDelegationDefaultOff(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: config.ServerConfig{APIKey: "owner-key"}}
	srv := NewServerWithOptions(ServerOptions{
		Config:    cfg,
		Store:     &stubSourceStore{},
		Logger:    testLogger(),
		Scheduler: newMockScheduler(),
	})
	require.Nil(t, srv.agentGrants, "agentGrants must be nil when AgentAccess is unset in config")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, "mva1_some_token_value_for_test")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code,
		"agent token must be refused when agent_access is unset")
}
