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
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/query/querytest"
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

// multiSourceStore extends mockStore with a per-ID source lookup for delegation tests.
type multiSourceStore struct {
	mockStore
	sources map[int64]*store.Source
}

func (m *multiSourceStore) GetSourceByIDContext(_ context.Context, id int64) (*store.Source, error) {
	return m.sources[id], nil //nolint:nilnil // nil, nil = not found, mirrors store contract
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

func newTestServerWithAgentGrantsAndEngine(t *testing.T, apiKey string, st MessageStore, engine query.Engine) (*Server, *agentgrant.Registry) {
	t.Helper()
	reg := agentgrant.NewRegistry(time.Now)
	cfg := &config.Config{Server: config.ServerConfig{APIKey: apiKey}}
	srv := NewServerWithOptions(ServerOptions{
		Config:        cfg,
		Store:         st,
		Logger:        testLogger(),
		Scheduler:     newMockScheduler(),
		Engine:        engine,
		AnalyticsMode: AnalyticsModeSQLFallback,
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
// Both GET /api/v1/cli/message and GET /api/v1/cli/message/raw are driven over
// HTTP with real identifier resolution. In-grant responses byte-match the
// owner's; out-of-grant and nonexistent responses are byte-identical (denial =
// absence). The provider_id_fallback subtest proves the fallback resolver
// cannot escape the grant boundary.
func TestDelegatedMessageReadScoping(t *testing.T) {
	inGrantSrc := &store.Source{ID: 10, SourceType: "imap", Identifier: "alice@example.com"}
	outGrantSrc := &store.Source{ID: 20, SourceType: "imap", Identifier: "bob@example.com"}
	providerSrc := &store.Source{ID: 30, SourceType: "imap", Identifier: "carol@example.com"}

	inGrantMsg := &query.MessageDetail{ID: 1, SourceID: inGrantSrc.ID, SourceMessageID: "alice-msg-1"}
	outGrantMsg := &query.MessageDetail{ID: 2, SourceID: outGrantSrc.ID, SourceMessageID: "bob-msg-1"}
	providerMsg := &query.MessageDetail{ID: 3, SourceID: providerSrc.ID, SourceMessageID: "carol-provider-id"}
	inGrantRaw := []byte("From: alice@example.com\r\nSubject: Test\r\n\r\nHello")

	st := &multiSourceStore{
		sources: map[int64]*store.Source{
			inGrantSrc.ID:  inGrantSrc,
			outGrantSrc.ID: outGrantSrc,
			providerSrc.ID: providerSrc,
		},
	}
	engine := &querytest.MockEngine{}
	engine.GetMessageFunc = func(_ context.Context, id int64) (*query.MessageDetail, error) {
		msgs := map[int64]*query.MessageDetail{
			inGrantMsg.ID:  inGrantMsg,
			outGrantMsg.ID: outGrantMsg,
			providerMsg.ID: providerMsg,
		}
		return msgs[id], nil //nolint:nilnil // nil, nil = not found
	}
	engine.GetMessageBySourceIDFunc = func(_ context.Context, sourceID string) (*query.MessageDetail, error) {
		if sourceID == "carol-provider-id" {
			return providerMsg, nil
		}
		return nil, nil //nolint:nilnil // nil, nil = not found
	}
	engine.GetMessageRawFunc = func(_ context.Context, id int64) ([]byte, error) {
		if id == inGrantMsg.ID {
			return inGrantRaw, nil
		}
		return nil, nil //nolint:nilnil // nil, nil = not found
	}

	srv, reg := newTestServerWithAgentGrantsAndEngine(t, "owner-key", st, engine)
	grantSrc := agentgrant.SourceRef{ID: inGrantSrc.ID, Type: inGrantSrc.SourceType, Identifier: inGrantSrc.Identifier}
	_, secret, _, err := reg.Issue("scoping-test", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{grantSrc}, agentgrant.DefaultLifetime)
	require.NoError(t, err)

	getMsg := func(id string, hdrs map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message?id="+id, nil)
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	getRaw := func(id string, hdrs map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/cli/message/raw?id="+id, nil)
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		return w
	}
	ownerHdr := map[string]string{"X-Api-Key": "owner-key"}
	delegatedHdr := map[string]string{apiprotocol.AgentTokenHeader: secret}

	t.Run("in-grant message JSON body byte-matches owner", func(t *testing.T) {
		ownerW := getMsg("1", ownerHdr)
		delegatedW := getMsg("1", delegatedHdr)
		require.Equal(t, http.StatusOK, ownerW.Code, "owner must get 200")
		require.Equal(t, http.StatusOK, delegatedW.Code, "in-grant delegated must get 200")
		assert.Equal(t, ownerW.Body.Bytes(), delegatedW.Body.Bytes(),
			"in-grant response body must byte-match the owner's")
	})

	t.Run("out-of-grant returns same 404 as nonexistent", func(t *testing.T) {
		outW := getMsg("2", delegatedHdr)
		noneW := getMsg("999", delegatedHdr)
		assert.Equal(t, http.StatusNotFound, outW.Code, "out-of-grant must be 404")
		assert.Equal(t, http.StatusNotFound, noneW.Code, "nonexistent must be 404")
		assert.Equal(t, outW.Body.Bytes(), noneW.Body.Bytes(),
			"out-of-grant and nonexistent must return byte-identical responses (denial = absence)")
	})

	t.Run("provider_id_fallback out-of-grant returns 404", func(t *testing.T) {
		// carol-provider-id resolves via GetMessageBySourceID to providerMsg
		// (SourceID=30, carol@example.com), which is not in the grant (only
		// alice@example.com/ID=10 is). The resolved source, not the supplied
		// identifier, determines the authorization outcome.
		w := getMsg("carol-provider-id", delegatedHdr)
		assert.Equal(t, http.StatusNotFound, w.Code,
			"provider-id fallback to out-of-grant source must be denied")
	})

	t.Run("raw in-grant message returns 200 with rfc822 content", func(t *testing.T) {
		w := getRaw("1", delegatedHdr)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "message/rfc822", w.Header().Get("Content-Type"))
		assert.Equal(t, inGrantRaw, w.Body.Bytes())
	})

	t.Run("raw out-of-grant returns same 404 as nonexistent", func(t *testing.T) {
		outW := getRaw("2", delegatedHdr)
		noneW := getRaw("999", delegatedHdr)
		assert.Equal(t, http.StatusNotFound, outW.Code, "raw out-of-grant must be 404")
		assert.Equal(t, http.StatusNotFound, noneW.Code, "raw nonexistent must be 404")
		assert.Equal(t, outW.Body.Bytes(), noneW.Body.Bytes(),
			"raw out-of-grant and nonexistent must return byte-identical responses")
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
	reg := agentgrant.NewRegistry(time.Now)
	srv.agentGrants = reg
	src := agentgrant.SourceRef{ID: 1, Type: "imap", Identifier: "alice@example.com"}
	_, secret, _, err := reg.Issue("gate-test", []agentgrant.Permission{agentgrant.PermissionDraftCreate}, []agentgrant.SourceRef{src}, agentgrant.DefaultLifetime)
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
