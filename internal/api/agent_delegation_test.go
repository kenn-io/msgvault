package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// allowedDelegatedOps is the exact set. Keep in sync with delegatedOperationAllowed.
var allowedDelegatedOps = []string{"runCLI", "getHealth"}

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

// TestDelegatedOperationAllowedExact tests proof matrix row 2.
// delegatedOperationAllowed must return true for exactly the two listed
// operations and false for others sampled from the live route list.
func TestDelegatedOperationAllowedExact(t *testing.T) {
	for _, op := range allowedDelegatedOps {
		assert.True(t, delegatedOperationAllowed(op), "op %q should be allowed", op)
	}

	// A representative sample of owner-only or irrelevant ops must be denied.
	notAllowed := []string{
		"getStats", "listMessages", "getMessage", "syncCLI", "syncFullCLI",
		"buildCLICache", "listCLIAccounts", "searchCLI", "getCLIStats",
		"listAgentTokens", "issueAgentToken", "revokeAgentToken",
		"loginSession", "getSession",
		"getCLIMessage", "getCLIMessageRaw",
		"", "runCLIX", "getHealth2",
	}
	for _, op := range notAllowed {
		assert.False(t, delegatedOperationAllowed(op), "op %q should not be allowed", op)
	}
}

// TestDelegatedFailsPrivilegedPredicate tests proof matrix row 2.
// apiRequestAuthorized returns false for a delegated request at all six call
// sites: the predicate itself, pprof guard, backup-freeze, getStats,
// listMessages, and issueAgentToken.
func TestDelegatedFailsPrivilegedPredicate(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("priv-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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

	t.Run("getStats returns 401", func(t *testing.T) {
		req := makeRequest(http.MethodGet, "/api/v1/stats")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("listMessages returns 401", func(t *testing.T) {
		req := makeRequest(http.MethodGet, "/api/v1/messages")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
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
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	grantID, validSecret, _, err := reg.Issue("fallback-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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
		_, newSecret, _, issErr := reg.Issue("with-key", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set("X-Api-Key", "mva1_spoofed_prefix_value")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("Authorization header alongside agent token", func(t *testing.T) {
		_, newSecret, _, issErr := reg.Issue("with-auth", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set("Authorization", "Bearer spoofed-owner-key")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "Authorization alongside agent token must classify AuthModeRequired")
	})

	t.Run("session cookie alongside agent token", func(t *testing.T) {
		_, newSecret, _, issErr := reg.Issue("with-cookie", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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
		_, newSecret, _, issErr := reg.Issue("with-daemon-token", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set(apiprotocol.DaemonRuntimeTokenHeader, "spoofed-daemon-token")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code, "daemon runtime token alongside agent token must classify AuthModeRequired")
	})
}

// TestDelegatedOperationAllowlistIsClosed tests proof matrix row 8.
// It enumerates all operations registered in the live route registry via the
// OpenAPI spec and verifies that exactly the two allowed operations pass the
// delegated auth middleware; every other /api/v1/* operation returns 401.
func TestDelegatedOperationAllowlistIsClosed(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("closedtest", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
	require.NoError(err)

	// Fetch the live OpenAPI spec to derive all registered operation IDs and their methods/paths.
	specReq := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	specRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(specRec, specReq)
	require.Equal(http.StatusOK, specRec.Code, "OpenAPI spec must be available at /openapi.json")

	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	require.NoError(json.NewDecoder(specRec.Body).Decode(&spec))
	require.NotEmpty(spec.Paths, "OpenAPI spec must contain paths")

	allowed := make(map[string]bool, len(allowedDelegatedOps))
	for _, op := range allowedDelegatedOps {
		allowed[op] = true
	}

	pathParamRE := regexp.MustCompile(`\{[^}]+\}`)
	testedAllowed, testedDenied := 0, 0
	// Each request uses a unique source IP to avoid tripping the per-IP rate
	// limiter, which is exercised by a dedicated rate-limit test and is not the
	// subject of this test.
	ipCounter := 0

	for rawPath, methods := range spec.Paths {
		// Only check /api/v1/* paths — these go through the huma auth middleware.
		if !strings.HasPrefix(rawPath, "/api/v1/") {
			continue
		}
		testPath := pathParamRE.ReplaceAllString(rawPath, "1")
		for method, op := range methods {
			if op.OperationID == "" {
				continue
			}
			req := httptest.NewRequest(strings.ToUpper(method), testPath, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, secret)
			// Use a unique source IP per request so the rate limiter does not
			// interfere with the auth check we are testing here.
			req.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:1234",
				(ipCounter/65536)%256, (ipCounter/256)%256, ipCounter%256)
			ipCounter++
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)

			if allowed[op.OperationID] {
				assert.NotEqual(http.StatusUnauthorized, w.Code,
					"allowed op %q (%s %s) must not return 401; got %d", op.OperationID, strings.ToUpper(method), rawPath, w.Code)
				testedAllowed++
			} else {
				assert.Equal(http.StatusUnauthorized, w.Code,
					"non-allowed op %q (%s %s) must return 401; got %d", op.OperationID, strings.ToUpper(method), rawPath, w.Code)
				testedDenied++
			}
		}
	}

	assert.GreaterOrEqual(testedAllowed, len(allowedDelegatedOps),
		"all two allowed ops must appear under /api/v1/*")
	assert.Greater(testedDenied, 10,
		"many non-allowed ops must be registered under /api/v1/")
}

// TestDelegationNotReachableOverHTTP tests proof matrix row 21.
// Delegation cannot enable or widen access over HTTP: settings routes (which
// expose agent_access and imap.drafts) return 401 for any delegated caller.
func TestDelegationNotReachableOverHTTP(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t)

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("http-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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
func TestDelegatedDraftAcquiresOperationGate(t *testing.T) {
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
	_, secret, _, err := reg.Issue("gate-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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
		done, ok := gate.BeginWork()
		require.True(t, ok, "must acquire the gate to hold it for this subtest")

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

		// requestGateEligible returns true for delegated, so the request enters
		// the gate, inspects the body, and registers as a waiter.
		// Gate label observed: "msgvault draft-reply".
		require.Eventually(t, func() bool { return gate.HasRequestWaiters() },
			time.Second, time.Millisecond,
			"delegated draft-reply must register as gate waiter (label: msgvault draft-reply)")

		done()
		<-reqDone
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
		case <-time.After(200 * time.Millisecond):
			t.Fatal("unauthenticated request must not block on the operation gate")
		}
		assert.False(t, gate.HasRequestWaiters(),
			"unauthenticated request must not register as a gate waiter")
	})
}

// TestDelegatedNonAllowlistedRouteDoesNotRegisterAsWaiter verifies that a
// delegated caller targeting a gated route outside the two-operation allowlist
// (e.g. POST /api/v1/accounts) is not admitted to the operation gate and
// receives 401 directly from the auth layer without ever queuing as a waiter.
func TestDelegatedNonAllowlistedRouteDoesNotRegisterAsWaiter(t *testing.T) {
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
	_, secret, _, err := reg.Issue("gate-nonallowed", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
	require.NoError(err)

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
	case <-time.After(200 * time.Millisecond):
		t.Fatal("delegated request on non-allowlisted gated route must not block on the operation gate")
	}
	assert.Equal(http.StatusUnauthorized, w.Code,
		"delegated caller on non-allowlisted route must get 401, not 503")
	assert.False(gate.HasRequestWaiters(),
		"delegated caller on non-allowlisted route must not register as a gate waiter")
}

// TestDelegatedGateBusyRedactsHolderLabel verifies that when a delegated caller
// times out on the /api/v1/cli/run gate the 503 body does not contain the
// internal holder label (which names configured account identifiers).
func TestDelegatedGateBusyRedactsHolderLabel(t *testing.T) {
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
	_, secret, _, err := reg.Issue("label-redact", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
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

// TestDelegatedNonDraftReplyDoesNotRegisterAsGateWaiter verifies that a
// delegated /api/v1/cli/run request carrying a non-draft-reply body bypasses
// the operation gate entirely: the request must return immediately (no waiter
// registered, no gate slot taken), the gate label is not influenced by the
// caller-supplied args, and the response is 400 command_not_allowed.
// Proof-matrix row 32.
func TestDelegatedNonDraftReplyDoesNotRegisterAsGateWaiter(t *testing.T) {
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
	_, secret, _, err := reg.Issue("gate-nondraft", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src})
	require.NoError(err)

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
	case <-time.After(200 * time.Millisecond):
		t.Fatal("delegated non-draft-reply must not block on the operation gate")
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
}

// TestDelegationDefaultOff verifies that delegation is off when agent_access is
// unset in config. The constructor (server.go:596-601) must leave agentGrants nil,
// and a presented agent token must be refused with 401.
func TestDelegationDefaultOff(t *testing.T) {
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
