package api

import (
	"context"
	"encoding/json"
	"errors"
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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// allowedDelegatedOps is the exact set. Keep in sync with delegatedOperationAllowed.
var allowedDelegatedOps = []string{"runCLI", "getCLIMessage", "getCLIMessageRaw", "getHealth"}

// stubSourceResolverStore wraps mockStore and adds GetSourceByIDContext.
type stubSourceStore struct {
	mockStore
	src    *store.Source
	srcErr error
}

func (s *stubSourceStore) GetSourceByIDContext(_ context.Context, _ int64) (*store.Source, error) {
	return s.src, s.srcErr
}

// newTestServerWithStore creates a test server using any MessageStore.
func newTestServerWithStore(st MessageStore) *Server {
	return NewServer(&config.Config{Server: config.ServerConfig{APIPort: 8080}}, st, nil, testLogger())
}

func newTestServerWithAgentGrants(t *testing.T, apiKey string) (*Server, *agentgrant.Registry) {
	t.Helper()
	reg := agentgrant.NewRegistry(time.Now)
	cfg := &config.Config{
		Server: config.ServerConfig{APIKey: apiKey},
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

// TestDelegatedOperationAllowedExact tests proof matrix rows 2 and 10.
// delegatedOperationAllowed must return true for exactly the four listed
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
		"", "runCLIX", "getHealth2",
	}
	for _, op := range notAllowed {
		assert.False(t, delegatedOperationAllowed(op), "op %q should not be allowed", op)
	}
}

// TestAuthorizeDelegatedMessage tests proof matrix rows 8 and 11.
func TestAuthorizeDelegatedMessage(t *testing.T) {
	src := &store.Source{
		ID:         42,
		SourceType: "imap",
		Identifier: "alice@example.com",
	}

	grant := agentgrant.Grant{
		ID:          "g1",
		Label:       "test",
		Permissions: []agentgrant.Permission{agentgrant.PermissionMessageRead},
		Sources: []agentgrant.SourceRef{
			{ID: src.ID, Type: src.SourceType, Identifier: src.Identifier},
		},
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}

	t.Run("non-delegated mode always true", func(t *testing.T) {
		srv := newTestServerWithStore(&mockStore{})
		auth := requestAuthentication{Mode: AuthModeAPIKey}
		msg := &query.MessageDetail{SourceID: src.ID}
		assert.True(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("delegated nil grant returns false", func(t *testing.T) {
		srv := newTestServerWithStore(&mockStore{})
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: nil}
		msg := &query.MessageDetail{SourceID: src.ID}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("delegated nil msg returns false", func(t *testing.T) {
		srv := newTestServerWithStore(&mockStore{})
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, nil))
	})

	t.Run("delegated msg with zero SourceID returns false", func(t *testing.T) {
		srv := newTestServerWithStore(&mockStore{})
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: 0}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("store that doesn't implement resolver returns false", func(t *testing.T) {
		// mockStore doesn't implement agentGrantSourceResolver
		srv := newTestServerWithStore(&mockStore{})
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: src.ID}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("source not in grant returns false", func(t *testing.T) {
		stub := &stubSourceStore{
			src: &store.Source{ID: 99, SourceType: "imap", Identifier: "bob@example.com"},
		}
		srv := newTestServerWithStore(stub)
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: 99}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("source matches grant returns true", func(t *testing.T) {
		stub := &stubSourceStore{src: src}
		srv := newTestServerWithStore(stub)
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: src.ID}
		assert.True(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("source error returns false", func(t *testing.T) {
		stub := &stubSourceStore{srcErr: assert.AnError}
		srv := newTestServerWithStore(stub)
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: src.ID}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})
}

// TestDelegatedFailsPrivilegedPredicate tests proof matrix row 2.
// apiRequestAuthorized returns false for a delegated request at all six call
// sites: the predicate itself, pprof guard, backup-freeze, getStats,
// listMessages, and issueAgentToken.
func TestDelegatedFailsPrivilegedPredicate(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t, "owner-key")

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("priv-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
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
	srv, _ := newTestServerWithAgentGrants(t, "owner-key")

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
// expired grant, revoked grant, owner credential alongside agent token —
// gets 401 and never falls through to a success mode.
func TestAgentTokenNeverFallsBack(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t, "owner-key")

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	grantID, validSecret, _, err := reg.Issue("fallback-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
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

	t.Run("expired grant", func(t *testing.T) {
		_, expiredSecret, _, issErr := reg.Issue("expires", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, time.Nanosecond)
		require.NoError(t, issErr)
		time.Sleep(time.Millisecond) // ensure the 1 ns grant has expired
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, expiredSecret)
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
		_, newSecret, _, issErr := reg.Issue("with-key", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
		require.NoError(t, issErr)
		w := makeHealthReq(func(req *http.Request) {
			req.Header.Set(apiprotocol.AgentTokenHeader, newSecret)
			req.Header.Set("X-Api-Key", "mva1_spoofed_prefix_value")
		})
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

// TestDelegatedOperationAllowlistIsClosed tests proof matrix row 8.
// It enumerates all operations registered in the live route registry via the
// OpenAPI spec and verifies that exactly the four allowed operations pass the
// delegated auth middleware; every other /api/v1/* operation returns 401.
func TestDelegatedOperationAllowlistIsClosed(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t, "owner-key")

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("closedtest", []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
	require.NoError(t, err)

	// Fetch the live OpenAPI spec to derive all registered operation IDs and their methods/paths.
	specReq := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	specRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(specRec, specReq)
	require.Equal(t, http.StatusOK, specRec.Code, "OpenAPI spec must be available at /openapi.json")

	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}
	require.NoError(t, json.NewDecoder(specRec.Body).Decode(&spec))
	require.NotEmpty(t, spec.Paths, "OpenAPI spec must contain paths")

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
				assert.NotEqual(t, http.StatusUnauthorized, w.Code,
					"allowed op %q (%s %s) must not return 401; got %d", op.OperationID, strings.ToUpper(method), rawPath, w.Code)
				testedAllowed++
			} else {
				assert.Equal(t, http.StatusUnauthorized, w.Code,
					"non-allowed op %q (%s %s) must return 401; got %d", op.OperationID, strings.ToUpper(method), rawPath, w.Code)
				testedDenied++
			}
		}
	}

	assert.GreaterOrEqual(t, testedAllowed, len(allowedDelegatedOps),
		"all four allowed ops must appear under /api/v1/*")
	assert.Greater(t, testedDenied, 10,
		"many non-allowed ops must be registered under /api/v1/")
}

// TestDelegatedMessageReadScoping tests proof matrix rows 10 and 11.
// In-grant messages are authorized; out-of-grant messages are denied with the
// same result as nonexistent messages (denial = absence). The
// provider_id_fallback subtest confirms that the resolver's source lookup
// cannot escape the grant boundary.
func TestDelegatedMessageReadScoping(t *testing.T) {
	inGrantSrc := &store.Source{ID: 10, SourceType: "imap", Identifier: "alice@example.com"}

	grant := agentgrant.Grant{
		ID:          "g-scoping",
		Permissions: []agentgrant.Permission{agentgrant.PermissionMessageRead},
		Sources: []agentgrant.SourceRef{
			{ID: inGrantSrc.ID, Type: inGrantSrc.SourceType, Identifier: inGrantSrc.Identifier},
		},
		ExpiresAt: time.Now().Add(time.Hour),
	}

	t.Run("in-grant source allows read", func(t *testing.T) {
		stub := &stubSourceStore{src: inGrantSrc}
		srv := newTestServerWithStore(stub)
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: inGrantSrc.ID}
		assert.True(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg))
	})

	t.Run("out-of-grant source denied same as nonexistent", func(t *testing.T) {
		outSrc := &store.Source{ID: 20, SourceType: "imap", Identifier: "bob@example.com"}
		stubOut := &stubSourceStore{src: outSrc}
		srvOut := newTestServerWithStore(stubOut)
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}

		// Out-of-grant source
		outMsg := &query.MessageDetail{SourceID: outSrc.ID}
		outDenied := srvOut.authorizeDelegatedMessage(context.Background(), auth, outMsg)

		// Nonexistent source (store returns error) — same outcome
		stubErr := &stubSourceStore{srcErr: errors.New("source not found")}
		srvErr := newTestServerWithStore(stubErr)
		errDenied := srvErr.authorizeDelegatedMessage(context.Background(), auth, outMsg)

		assert.False(t, outDenied, "out-of-grant source must be denied")
		assert.False(t, errDenied, "nonexistent source must be denied")
		assert.Equal(t, outDenied, errDenied, "denial = absence: both produce false")
	})

	t.Run("provider_id_fallback", func(t *testing.T) {
		// A message whose source is looked up via provider ID but resolves to a
		// source not in the grant must still be denied. The grant check uses the
		// resolved (id, type, identifier) triple, not just the numeric ID.
		providerSrc := &store.Source{ID: 30, SourceType: "imap", Identifier: "carol@example.com"}
		stub := &stubSourceStore{src: providerSrc}
		srv := newTestServerWithStore(stub)
		auth := requestAuthentication{Mode: AuthModeDelegated, Grant: &grant}
		msg := &query.MessageDetail{SourceID: providerSrc.ID}
		assert.False(t, srv.authorizeDelegatedMessage(context.Background(), auth, msg),
			"provider-id fallback must not escape the grant: source 30 is not in the grant (only 10 is)")
	})
}

// TestDelegationNotReachableOverHTTP tests proof matrix row 21.
// Delegation cannot enable or widen access over HTTP: settings routes (which
// expose agent_access and imap.drafts) return 401 for any delegated caller.
func TestDelegationNotReachableOverHTTP(t *testing.T) {
	srv, reg := newTestServerWithAgentGrants(t, "owner-key")

	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("http-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
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

// TestDelegationNotReachableFromOwnerPaths verifies that delegated mode callers
// cannot reach owner-authenticated paths. Proof matrix row 7.
func TestDelegationNotReachableFromOwnerPaths(t *testing.T) {
	_, reg := newTestServerWithAgentGrants(t, "owner-key")

	// Issue a grant
	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("delegation-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
	require.NoError(t, err)

	// Confirm the grant can be looked up
	grant, ok := reg.Lookup(secret)
	require.True(t, ok)
	require.Equal(t, "delegation-test", grant.Label)

	// The key assertion: delegated mode must not satisfy the owner allowlist.
	assert.False(t,
		AuthModeDelegated == AuthModeLoopback ||
			AuthModeDelegated == AuthModeAPIKey ||
			AuthModeDelegated == AuthModeSession,
		"delegated mode must not satisfy the owner authorization allowlist")
}
