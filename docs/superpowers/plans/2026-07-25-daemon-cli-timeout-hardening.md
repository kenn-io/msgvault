# Daemon CLI Timeout Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make daemon-backed CLI, TUI, MCP, and backup archive operations wait for completion or caller cancellation on slow storage while preserving bounded discovery, connection setup, browser, and ordinary API traffic.

**Architecture:** A shared internal protocol marker identifies requests made by msgvault's CLI-mode daemon client. The client removes its whole-request timeout but retains the configured transport and command context; the server honors the marker only for the existing API-key or keyless-loopback authentication classifications, clears active read/write deadlines, and preserves the incoming cancellation context. A bounded authenticated health probe remains separate from archive work, and all production daemon-client construction flows through one CLI-mode helper.

**Tech Stack:** Go, `net/http`, Huma/oapi-codegen generated client, Cobra, DuckDB, testify.

## Global Constraints

- Follow `CLAUDE.md` and the repository `AGENTS.md`.
- All Go tests must use `github.com/stretchr/testify`; equality assertions use `(want, got)`.
- Every `go test` command must include `-tags "fts5 sqlite_vec"`; prefer `make test` for the full suite.
- Do not add command-line flags, configuration keys, dependencies, or an OpenAPI declaration for the internal header.
- Do not change authorization, operation-gate, rate-limit, CSRF, discovery, or daemon-autostart policy.
- API-key and loopback eligibility must come from `Server.requestAuthentication`; do not duplicate authentication, proxy, or forwarded-header logic.
- CLI mode must disable only `http.Client.Timeout`; it must preserve the supplied transport, including dial and TLS-handshake bounds.
- Raw SQL may bypass `QueryEndpointTimeout` only when the end-to-end DuckDB interruption test passes reliably.
- Existing explicitly long maintenance paths remain unbounded for compatibility.
- Invoke `kenn:commit` immediately before every `git commit`.

## File Structure

- Create `internal/apiprotocol/client.go`: shared, non-OpenAPI CLI request marker constants.
- Modify `internal/daemonclient/client.go`: CLI request mode, root context, request marker, and timeout policy.
- Modify `internal/daemonclient/errors.go`: make generated busy retries use the client's root context.
- Modify `internal/daemonclient/store_adapter.go`: use the root context in compatibility methods without context parameters.
- Modify `internal/daemonclient/cli.go`: use the root context in `SaveManifest`.
- Modify `internal/daemonclient/client_test.go` and `internal/daemonclient/store_adapter_test.go`: client policy, transport preservation, marker, cancellation, and bounded compatibility coverage.
- Modify `internal/api/server.go`: authenticated request-origin timeout classification and shared read/write deadline clearing.
- Modify `internal/api/server_test.go`: bounded/unbounded authentication matrix and connection deadline assertions.
- Modify `internal/api/middleware_test.go`: invalid-key and trusted-proxy classification regressions.
- Modify `internal/api/handlers_test.go`: marked HTTP request cancellation must interrupt a real DuckDB query.
- Modify `cmd/msgvault/cmd/store_resolver.go`: centralized production CLI client construction and cheap authenticated health probe.
- Modify `cmd/msgvault/cmd/store_resolver_test.go`: production mode, root context, and probe regressions.
- Modify `cmd/msgvault/cmd/stats.go` and `cmd/msgvault/cmd/stats_test.go`: use the context-aware CLI statistics route for global and scoped output.
- Modify `cmd/msgvault/cmd/backup.go`; create `cmd/msgvault/cmd/backup_daemon_test.go`: pass the command context and CLI request mode to freeze coordination.
- Create `docs/internal/daemon-cli-request-audit.md`: durable inventory of all production daemon-client construction and cancellation boundaries.
- Modify `docs/changelog.md`: user-visible slow-storage timeout fix.
- Modify `docs/superpowers/specs/2026-07-25-daemon-cli-timeout-hardening-design.md`: mark the design implemented only after all acceptance gates pass.

---

### Task 1: Add CLI Request Mode and Root-Context Cancellation

**Files:**

- Create: `internal/apiprotocol/client.go`
- Modify: `internal/daemonclient/client.go`
- Modify: `internal/daemonclient/errors.go`
- Modify: `internal/daemonclient/store_adapter.go`
- Modify: `internal/daemonclient/cli.go`
- Modify: `internal/daemonclient/client_test.go`
- Modify: `internal/daemonclient/store_adapter_test.go`

**Interfaces:**

- Produces: `apiprotocol.ClientClassHeader`, `apiprotocol.ClientClassCLI`.
- Produces: `daemonclient.RequestMode`, `daemonclient.RequestModeBounded`, and `daemonclient.RequestModeCLI`.
- Extends: `daemonclient.Config` with `Context context.Context` and `RequestMode RequestMode`.
- Produces: `func (c *Client) requestContext() context.Context` for compatibility adapters and retry waits.
- Preserves: zero-valued `RequestModeBounded` and the existing 30-second default timeout.

- [ ] **Step 1: Write failing client-mode and compatibility tests**

Add these focused cases to `internal/daemonclient/client_test.go`:

```go
func TestNewCLIModeDisablesWholeRequestTimeoutAndPreservesTransport(t *testing.T) {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 7 * time.Second}).DialContext,
		TLSHandshakeTimeout: 11 * time.Second,
	}
	base := &http.Client{Transport: transport, Timeout: 45 * time.Second}

	c, err := New(Config{
		URL:           "https://nas.example:8443",
		HTTPClient:    base,
		RequestMode:   RequestModeCLI,
	})
	require.NoError(t, err, "New")

	assert.Zero(t, c.Timeout(), "CLI operations are governed by their context")
	assert.Same(t, transport, c.httpClient.Transport, "transport-level bounds are retained")
	assert.Equal(t, 11*time.Second, transport.TLSHandshakeTimeout)
	assert.Equal(t, 45*time.Second, base.Timeout, "caller-owned client is not mutated")
}

func TestCLIModeGeneratedRequestCarriesClassAndAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, apiprotocol.ClientClassCLI, r.Header.Get(apiprotocol.ClientClassHeader))
		assert.Equal(t, "secret-key", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n"],"rows":[[1]],"row_count":1}`))
	}))
	t.Cleanup(srv.Close)

	c, err := New(Config{
		URL: srv.URL, APIKey: "secret-key", AllowInsecure: true,
		RequestMode: RequestModeCLI,
	})
	require.NoError(t, err, "New")
	_, err = c.RunSQLQuery(context.Background(), "SELECT 1")
	require.NoError(t, err, "RunSQLQuery")
}
```

Add a raw-request assertion to the existing streaming coverage: inside
`TestRunCLISyncStreamsWithoutAbsoluteClientTimeout`, assert
`apiprotocol.ClientClassCLI` and construct the client with
`RequestMode: RequestModeCLI`. Keep `TestNewDefaultTimeout` and
`TestNew_DefaultTimeout`, changing the latter to assert exactly
`30*time.Second` so bounded construction remains supported.

Add this root-context test to `internal/daemonclient/store_adapter_test.go`:

```go
func TestLegacyAdapterUsesClientRootContext(t *testing.T) {
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-r.Context().Done():
			close(requestCanceled)
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	root, cancel := context.WithCancel(context.Background())
	c, err := New(Config{
		URL: srv.URL, AllowInsecure: true, Context: root,
		RequestMode: RequestModeCLI,
	})
	require.NoError(t, err, "New")

	done := make(chan error, 1)
	go func() {
		_, err := c.GetStats()
		done <- err
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "legacy adapter request did not start")
	}
	cancel()

	require.Eventually(t, func() bool {
		select {
		case <-requestCanceled:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "root cancellation reaches HTTP request")
	require.Error(t, <-done, "canceled compatibility request")
}
```

- [ ] **Step 2: Run the new tests and confirm the intended failures**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/daemonclient \
  -run 'Test(NewCLIMode|CLIModeGenerated|LegacyAdapterUses|NewDefaultTimeout|New_DefaultTimeout|RunCLISync)' -count=1
```

Expected: build failures for the missing protocol constants and request mode, followed by behavior failures until root context and header propagation are implemented.

- [ ] **Step 3: Add the shared marker and client policy**

Create `internal/apiprotocol/client.go`:

```go
package apiprotocol

const (
	ClientClassHeader = "X-Msgvault-Client"
	ClientClassCLI    = "cli"
)
```

In `internal/daemonclient/client.go`, add:

```go
type RequestMode uint8

const (
	RequestModeBounded RequestMode = iota
	RequestModeCLI
)

type Config struct {
	URL           string
	APIKey        string
	AllowInsecure bool
	Timeout       time.Duration
	HTTPClient    *http.Client
	Context       context.Context
	RequestMode   RequestMode
}
```

Store `rootContext context.Context` and `requestMode RequestMode` on `Client`.
In `New`, keep the existing 30-second default only for bounded mode and force
the cloned client's whole-request timeout to zero for CLI mode:

```go
timeout := cfg.Timeout
if cfg.RequestMode == RequestModeCLI {
	timeout = 0
} else if timeout == 0 {
	timeout = 30 * time.Second
}
rootContext := cfg.Context
if rootContext == nil {
	rootContext = context.Background()
}
```

Do not replace or wrap `httpClient.Transport`. Pass the mode to the request
editor and add:

```go
func (c *Client) requestContext() context.Context {
	if c == nil || c.rootContext == nil {
		return context.Background()
	}
	return c.rootContext
}

func requestEditor(apiKey string, mode RequestMode) apiclient.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		if apiKey != "" {
			req.Header.Set("X-Api-Key", apiKey)
		}
		if mode == RequestModeCLI {
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		}
		req.Header.Set("Accept", "application/json")
		return nil
	}
}
```

Because generated, raw, and streaming requests all originate from the generated
client's request builder, this single editor covers all three forms without
duplicated header logic.

- [ ] **Step 4: Replace compatibility `context.Background()` calls**

In `internal/daemonclient/store_adapter.go`, replace the backgrounds in
`GetStats`, `ListMessages`, `GetMessage`, `SearchMessages`, and `ListAccounts`
with `c.requestContext()`. In `internal/daemonclient/cli.go`, change
`SaveManifest` to:

```go
func (c *Client) SaveManifest(manifest *deletion.Manifest) error {
	_, err := c.CreateCLIDeletionManifest(c.requestContext(), manifest)
	return err
}
```

In `internal/daemonclient/errors.go`, make a busy response wait on
`c.requestContext()`:

```go
if waiter.wait(c.requestContext(), checkErr) {
	continue
}
```

Explicit-context methods continue using the context supplied to that method;
the root context is only the compatibility fallback and retry-loop cancellation
source.

- [ ] **Step 5: Format and run the daemon-client package**

Run:

```bash
gofmt -w internal/apiprotocol/client.go internal/daemonclient/client.go \
  internal/daemonclient/errors.go internal/daemonclient/store_adapter.go \
  internal/daemonclient/cli.go internal/daemonclient/client_test.go \
  internal/daemonclient/store_adapter_test.go
go test -tags "fts5 sqlite_vec" ./internal/apiprotocol ./internal/daemonclient -count=1
```

Expected: both packages pass, including exact 30-second bounded construction,
zero-timeout CLI mode, transport identity, marker propagation, streaming, and
root-context cancellation.

- [ ] **Step 6: Commit the client policy**

Invoke `kenn:commit`, then:

```bash
git add internal/apiprotocol/client.go internal/daemonclient/client.go \
  internal/daemonclient/errors.go internal/daemonclient/store_adapter.go \
  internal/daemonclient/cli.go internal/daemonclient/client_test.go \
  internal/daemonclient/store_adapter_test.go
git commit -m "fix: make daemon CLI requests caller-governed" \
  -m "Slow archive operations must not inherit an arbitrary whole-request timeout, while ordinary daemon clients still need their bounded default. Preserve transport liveness limits and use the command context for legacy adapters and gate retries."
```

---

### Task 2: Enforce Authenticated Server Duration Policy and Prove DuckDB Interruption

**Files:**

- Modify: `internal/api/server.go`
- Modify: `internal/api/server_test.go`
- Modify: `internal/api/middleware_test.go`
- Modify: `internal/api/handlers_test.go`

**Interfaces:**

- Consumes: `apiprotocol.ClientClassHeader` and `apiprotocol.ClientClassCLI`.
- Produces: `func (s *Server) requestUsesCLITimeoutPolicy(r *http.Request) bool`.
- Produces: `func serveWithoutRequestDeadlines(w http.ResponseWriter, r *http.Request, next http.Handler)`.
- Preserves: `requestTimeoutForPath`, `QueryEndpointTimeout`, and `isLongDaemonRequest` for unmarked traffic.

- [ ] **Step 1: Write the authentication and deadline-policy tests**

Extend `deadlineClearingRecorder` in `internal/api/server_test.go` with separate
`readDeadlines` and `writeDeadlines` slices and both controller methods:

```go
func (w *deadlineClearingRecorder) SetReadDeadline(deadline time.Time) error {
	w.readDeadlines = append(w.readDeadlines, deadline)
	return nil
}

func (w *deadlineClearingRecorder) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadlines = append(w.writeDeadlines, deadline)
	return nil
}
```

Rename the existing deadline test to
`TestTimeoutMiddlewareClearsReadAndWriteDeadlines` and cover:

```go
longPath := httptest.NewRequest(http.MethodPost, "/api/v1/cli/sync-full", nil)
marked := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats", nil)
marked.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
bounded := httptest.NewRequest(http.MethodGet, "/api/v1/cli/stats", nil)
```

Assert one zero read and write deadline for `longPath` and `marked`, and no
deadline changes for `bounded`.

Add a table test whose wrapped handler waits 40ms while
`RequestTimeout` is 5ms:

```go
tests := []struct {
	name        string
	apiKey      string
	configure   func(*Server, *http.Request)
	wantTimeout bool
}{
	{
		name: "keyless loopback CLI",
		configure: func(_ *Server, req *http.Request) {
			req.RemoteAddr = "127.0.0.1:4242"
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		},
	},
	{
		name:   "API key CLI",
		apiKey: "secret",
		configure: func(_ *Server, req *http.Request) {
			req.Header.Set("X-Api-Key", "secret")
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		},
	},
	{
		name: "unmarked API request",
		wantTimeout: true,
	},
	{
		name:   "browser session cannot opt in",
		apiKey: "secret",
		wantTimeout: true,
		configure: func(srv *Server, req *http.Request) {
			id, _, err := srv.sessions.create()
			require.NoError(t, err, "create session")
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: id})
			req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
		},
	},
}
```

The wrapped handler waits on either `time.After(40*time.Millisecond)` or
`r.Context().Done()`. Assert `context.DeadlineExceeded` only for
`wantTimeout`, and successful completion for authenticated CLI cases.

Add `TestTimeoutMiddlewareMarkedRequestPreservesCallerCancellation`: cancel the
request context after the handler starts and assert the handler sees
`context.Canceled` promptly.

- [ ] **Step 2: Write invalid-auth and proxy-spoofing regressions**

In `internal/api/middleware_test.go`, create an API-key-configured server and
requests to `/api/v1/health` with the CLI marker:

```go
invalid := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
invalid.Header.Set("X-Api-Key", "wrong")
invalid.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
assert.False(t, srv.requestUsesCLITimeoutPolicy(invalid))
invalidResp := httptest.NewRecorder()
srv.Router().ServeHTTP(invalidResp, invalid)
assert.Equal(t, http.StatusUnauthorized, invalidResp.Code)

proxied := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
proxied.RemoteAddr = "127.0.0.1:4242"
proxied.Header.Set("X-Forwarded-For", "203.0.113.10")
proxied.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)
assert.False(t, srv.requestUsesCLITimeoutPolicy(proxied))
proxiedResp := httptest.NewRecorder()
srv.Router().ServeHTTP(proxiedResp, proxied)
assert.Equal(t, http.StatusUnauthorized, proxiedResp.Code)
```

Configure `TrustedProxies: []string{"127.0.0.1/32"}` for the second request.
This pins the rule that neither a marker nor forwarded headers manufacture an
eligible authentication mode.

- [ ] **Step 3: Run the server policy tests and confirm they fail**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./internal/api \
  -run 'Test(TimeoutMiddleware|CLIRequestDuration|InvalidCLI|ProxiedCLI)' -count=1
```

Expected: marked bounded paths still time out, browser/CLI classification is
missing, and only write deadlines are currently cleared.

- [ ] **Step 4: Implement authenticated CLI classification and shared deadline clearing**

In `internal/api/server.go`, import `internal/apiprotocol` and add:

```go
func (s *Server) requestUsesCLITimeoutPolicy(r *http.Request) bool {
	if r.Header.Get(apiprotocol.ClientClassHeader) != apiprotocol.ClientClassCLI {
		return false
	}
	switch s.requestAuthentication(r).Mode {
	case AuthModeAPIKey, AuthModeLoopback:
		return true
	default:
		return false
	}
}

func serveWithoutRequestDeadlines(
	w http.ResponseWriter,
	r *http.Request,
	next http.Handler,
) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	next.ServeHTTP(w, r)
}
```

Change `timeoutMiddleware` so either an authenticated CLI request or an
existing unbounded path uses that helper:

```go
timeout, bounded := s.requestTimeoutForPath(r.URL.Path)
if s.requestUsesCLITimeoutPolicy(r) || !bounded {
	serveWithoutRequestDeadlines(w, r, next)
	return
}
ctx, cancel := context.WithTimeout(r.Context(), timeout)
defer cancel()
next.ServeHTTP(w, r.WithContext(ctx))
```

Do not alter `requestAuthentication`, middleware order, path policy, or the
incoming request context.

- [ ] **Step 5: Add the end-to-end marked raw-SQL interruption test**

In `internal/api/handlers_test.go`, add a real DuckDB-backed test:

```go
func TestMarkedCLIQueryCancellationInterruptsDuckDB(t *testing.T) {
	engine, err := query.NewDuckDBEngine("", "", nil)
	require.NoError(t, err, "NewDuckDBEngine")
	t.Cleanup(func() { _ = engine.Close() })

	queryStarted := make(chan struct{})
	queryReturned := make(chan struct{})
	queryErr := make(chan error, 1)
	srv := NewServerWithOptions(ServerOptions{
		Config: &config.Config{Server: config.ServerConfig{
			APIPort: 8080,
			APIKey:  "secret",
		}},
		Logger: testLogger(),
		SQLQueryRunner: func(ctx context.Context, sql string) (*query.QueryResult, error) {
			close(queryStarted)
			result, err := engine.QuerySQL(ctx, sql)
			queryErr <- err
			close(queryReturned)
			return result, err
		},
	})
	srv.queryTimeout = 20 * time.Millisecond
	httpServer := httptest.NewServer(srv.Router())
	t.Cleanup(httpServer.Close)

	const slowSQL = `SELECT COUNT(*) FROM range(1000000) a, range(1000000) b
		WHERE (a.range * b.range) % 7 = 0`
	body := strings.NewReader(`{"sql":` + strconv.Quote(slowSQL) + `}`)
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL+queryEndpointPath, body)
	require.NoError(t, err, "NewRequestWithContext")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "secret")
	req.Header.Set(apiprotocol.ClientClassHeader, apiprotocol.ClientClassCLI)

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()

	require.Eventually(t, func() bool {
		select {
		case <-queryStarted:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond, "DuckDB query starts")
	assert.Never(t, func() bool {
		select {
		case <-queryReturned:
			return true
		default:
			return false
		}
	}, 60*time.Millisecond, 5*time.Millisecond,
		"marked query survives the 20ms ordinary query ceiling")

	cancel()
	select {
	case <-queryReturned:
		require.Error(t, <-queryErr, "DuckDB returns cancellation")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "DuckDB continued after marked HTTP cancellation")
	}
	require.Error(t, <-requestDone, "client observes cancellation")
}
```

Keep `TestHandleQueryEnforcesQueryTimeout` unchanged to prove unmarked traffic
still receives `query_timeout`. If the marked integration test is flaky or
does not observe `queryReturned` within five seconds on supported builders,
stop this task and leave `/api/v1/query` bounded for marked requests; do not
claim raw-SQL acceptance.

- [ ] **Step 6: Run the focused server and query tests**

Run:

```bash
gofmt -w internal/api/server.go internal/api/server_test.go \
  internal/api/middleware_test.go internal/api/handlers_test.go
go test -tags "fts5 sqlite_vec" ./internal/query \
  -run TestQuerySQLHonorsContextCancellation -count=1
go test -tags "fts5 sqlite_vec" ./internal/api \
  -run 'Test(TimeoutMiddleware|CLIRequestDuration|InvalidCLI|ProxiedCLI|HandleQueryEnforces|MarkedCLIQuery)' \
  -count=1
```

Expected: all pass; the marked query remains active beyond 20ms and the actual
DuckDB call returns within five seconds of HTTP cancellation.

- [ ] **Step 7: Commit the authenticated server policy**

Invoke `kenn:commit`, then:

```bash
git add internal/api/server.go internal/api/server_test.go \
  internal/api/middleware_test.go internal/api/handlers_test.go
git commit -m "fix: honor authenticated daemon CLI request duration" \
  -m "CLI intent must be represented independently of route names, but only existing trusted authentication modes may opt into caller-governed duration. Preserve browser and ordinary API ceilings and prove that canceling marked raw SQL interrupts DuckDB itself."
```

---

### Task 3: Decouple Daemon Authentication and Statistics from Archive-Wide Timeouts

**Files:**

- Modify: `cmd/msgvault/cmd/store_resolver.go`
- Modify: `cmd/msgvault/cmd/store_resolver_test.go`
- Modify: `cmd/msgvault/cmd/stats.go`
- Modify: `cmd/msgvault/cmd/stats_test.go`

**Interfaces:**

- Preserves: `localDaemonAuthProbeTimeout = 2 * time.Second`.
- Changes: the probe target from `/api/v1/stats` to authenticated `/api/v1/health`.
- Changes: global `runStats` to call `GetCLIStats(cmd.Context(), "", "")`.

- [ ] **Step 1: Write the probe regression tests**

Update keyed probe fixtures in `store_resolver_test.go` so
`/api/v1/health` validates `X-Api-Key` and returns `{"status":"ok"}` while
`/api/v1/stats` either records an unexpected call or blocks.

Add:

```go
func TestProbeLocalDaemonAuthDoesNotWaitForStats(t *testing.T) {
	var statsCalled atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "secret", r.Header.Get("X-Api-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/api/v1/stats", func(w http.ResponseWriter, _ *http.Request) {
		statsCalled.Store(true)
		time.Sleep(3 * localDaemonAuthProbeTimeout)
		w.WriteHeader(http.StatusOK)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	rt := daemonRuntimeForHTTPServer(t, server, daemonAPIKeyFingerprint("secret"))
	c := lifecycleTestConfig(t.TempDir())
	c.Server.APIKey = "secret"

	start := time.Now()
	require.NoError(t, probeLocalDaemonAuth(context.Background(), rt, c))
	assert.Less(t, time.Since(start), localDaemonAuthProbeTimeout)
	assert.False(t, statsCalled.Load(), "auth probe never performs archive statistics")
}
```

Extract
`daemonRuntimeForHTTPServer(t *testing.T, server *httptest.Server, authFingerprint string) *DaemonRuntime`
from the repeated host/port runtime fixture code in the same test file. It
returns a runtime with the current PID, parsed TCP host/port, API version, and
the supplied authentication fingerprint.

Add a slow-health case using a test-only 50ms parent context deadline and
assert `context deadline exceeded` is wrapped as a probe failure. Retain the
existing stale-fingerprint short circuit and actionable restart-message tests.

- [ ] **Step 2: Make global and scoped stats tests require the CLI route**

Change `statsHTTPDaemon` so `/api/v1/stats` fails the test when used by the
command. Make `/api/v1/cli/stats` return the existing global response when both
query parameters are empty, and the existing scoped response when
`collection=Important`.

Both command tests must assert:

```go
assert.Equal(t, int32(1), statsRequests.Load(), "exactly one CLI stats request")
```

The output text remains byte-for-byte unchanged.

- [ ] **Step 3: Run the command tests and confirm the route failures**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run 'Test(ProbeLocalDaemonAuth|OpenHTTPStoreUsesServerAPIKey|OpenHTTPStoreRejectsLocalDaemon|StatsCommand_)' \
  -count=1
```

Expected: probe fixtures report `/api/v1/stats`, and unscoped stats still call
the forbidden general statistics route.

- [ ] **Step 4: Switch the probe and global statistics call**

In `probeLocalDaemonAuth`, change only the request path:

```go
req, err := http.NewRequestWithContext(
	probeCtx,
	http.MethodGet,
	url+"/api/v1/health",
	nil,
)
```

Keep the two-second context, probe header, API key, response limit, status
handling, and restart guidance.

Collapse global/scoped routing in `runStats` around the context-aware method:

```go
resp, err := s.GetCLIStats(cmd.Context(), statsAccount, statsCollection)
if err != nil {
	logger.Warn("stats failed", "error", err.Error())
	return fmt.Errorf("get stats: %w", err)
}
dbStats := resp.Stats
```

Preserve the existing global labels and output, and keep the scoped label and
empty-collection behavior. Remove the obsolete context-free `GetStats` call
from this command, not the compatibility method from `daemonclient`.

- [ ] **Step 5: Format, test, and commit the probe/statistics fix**

Run:

```bash
gofmt -w cmd/msgvault/cmd/store_resolver.go \
  cmd/msgvault/cmd/store_resolver_test.go cmd/msgvault/cmd/stats.go \
  cmd/msgvault/cmd/stats_test.go
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run 'Test(ProbeLocalDaemonAuth|OpenHTTPStore|StatsCommand_)' -count=1
```

Expected: all pass, slow stats is never touched by daemon reuse, slow health is
still bounded, and both stats modes use `/api/v1/cli/stats`.

Invoke `kenn:commit`, then:

```bash
git add cmd/msgvault/cmd/store_resolver.go \
  cmd/msgvault/cmd/store_resolver_test.go cmd/msgvault/cmd/stats.go \
  cmd/msgvault/cmd/stats_test.go
git commit -m "fix: keep daemon readiness probes cheap" \
  -m "Authentication discovery must remain prompt without triggering archive-wide counts, while user-requested statistics should inherit the command context and CLI duration policy."
```

---

### Task 4: Route Every Production Daemon Client Through CLI Mode

**Files:**

- Modify: `cmd/msgvault/cmd/store_resolver.go`
- Modify: `cmd/msgvault/cmd/store_resolver_test.go`
- Modify: `cmd/msgvault/cmd/backup.go`
- Create: `cmd/msgvault/cmd/backup_daemon_test.go`

**Interfaces:**

- Produces: `func newDaemonCLIClient(ctx context.Context, cfg daemonclient.Config) (*daemonclient.Client, error)`.
- Changes: `openRemoteStoreWithTimeout` to `openRemoteStore(ctx context.Context)`.
- Changes: `newBackupFreezer()` to `newBackupFreezer(ctx context.Context)`.
- Consumes: `daemonclient.RequestModeCLI` and `daemonclient.Config.Context`.

- [ ] **Step 1: Write production-construction tests**

Replace `TestOpenHTTPStoreUsesLongTimeoutForConfiguredRemote` with
`TestOpenHTTPStoreUsesCLIModeForConfiguredRemote`. Use an `httptest.Server`
whose authenticated health handler records the marker, then assert:

```go
assert.Zero(t, st.Timeout(), "configured remote operations use caller duration")
_, err = st.GetHealth(context.Background())
require.NoError(t, err, "GetHealth")
assert.Equal(t, apiprotocol.ClientClassCLI, marker.Load())
```

Add a local-daemon equivalent that passes a cancelable context to
`OpenHTTPStore`, calls the context-free `GetStats`, cancels the root context,
and observes the server request context close. This proves the command context
stored by the production constructor, rather than only direct `New` coverage.

Create `cmd/msgvault/cmd/backup_daemon_test.go` with a runtime record pointing
to an `httptest.Server`:

```go
func TestNewBackupFreezerUsesCommandContextAndCLIMode(t *testing.T) {
	var marker atomic.Value
	requestCanceled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/backup/freeze/begin", r.URL.Path)
		marker.Store(r.Header.Get(apiprotocol.ClientClassHeader))
		<-r.Context().Done()
		close(requestCanceled)
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	withStoreResolverConfig(t, lifecycleTestConfig(dataDir))
	rt := daemonRuntimeForHTTPServer(t, srv, daemonAPIKeyFingerprint(""))
	_, err := daemonRuntimeStore(dataDir).Write(rt.Record)
	require.NoError(t, err, "write daemon runtime")

	ctx, cancel := context.WithCancel(context.Background())
	freezer, closeFreezer, err := newBackupFreezer(ctx)
	require.NoError(t, err, "newBackupFreezer")
	t.Cleanup(closeFreezer)

	done := make(chan error, 1)
	go func() {
		done <- freezer.Begin(ctx)
	}()
	require.Eventually(t, func() bool {
		return marker.Load() != nil
	}, 2*time.Second, 10*time.Millisecond, "freeze request starts")
	cancel()

	require.Eventually(t, func() bool {
		select {
		case <-requestCanceled:
			return true
		default:
			return false
		}
	}, 2*time.Second, 10*time.Millisecond)
	assert.Equal(t, apiprotocol.ClientClassCLI, marker.Load())
	require.Error(t, <-done, "freeze request canceled")
}
```

Reuse the runtime-fixture helper extracted in Task 3 rather than duplicating
host/port parsing.

- [ ] **Step 2: Run the construction tests and confirm failures**

Run:

```bash
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run 'Test(OpenHTTPStoreUsesCLIMode|OpenHTTPStoreRootContext|NewBackupFreezerUses)' \
  -count=1
```

Expected: the renamed helper/signature is missing, current constructors retain
30-minute or 30-second whole-request timeouts, and backup lacks the command
context and marker.

- [ ] **Step 3: Centralize CLI-mode construction**

In `store_resolver.go`, add:

```go
func newDaemonCLIClient(
	ctx context.Context,
	clientConfig daemonclient.Config,
) (*daemonclient.Client, error) {
	clientConfig.Context = ctx
	clientConfig.RequestMode = daemonclient.RequestModeCLI
	return daemonclient.New(clientConfig)
}
```

Use it for both `OpenHTTPStore` branches. Replace
`openRemoteStoreWithTimeout(timeout)` with:

```go
func openRemoteStore(ctx context.Context) (*daemonclient.Client, error) {
	st, err := newDaemonCLIClient(ctx, daemonclient.Config{
		URL:           cfg.Remote.URL,
		APIKey:        cfg.Remote.APIKey,
		AllowInsecure: cfg.Remote.AllowInsecure,
	})
	if err != nil {
		return nil, err
	}
	st.SetBusyNotifier(reportDaemonBusyWait)
	return st, nil
}
```

The local branch passes `ctx`, URL, server API key, and
`AllowInsecure: true`, with no `Timeout`.

- [ ] **Step 4: Put backup freeze coordination on the same construction path**

Change the call in `runBackupCreateLocal` to:

```go
freezer, closeFreezer, err := newBackupFreezer(cmd.Context())
```

Change the helper signature and constructor:

```go
func newBackupFreezer(ctx context.Context) (backup.FreezeCoordinator, func(), error) {
	// Existing runtime resolution and error remain unchanged.
	client, err := newDaemonCLIClient(ctx, daemonclient.Config{
		URL:           urlFromDaemonRuntime(rt),
		APIKey:        cfg.Server.APIKey,
		AllowInsecure: true,
	})
	// Existing error wrapping and return remain unchanged.
}
```

This leaves exactly one direct production `daemonclient.New` call, inside
`newDaemonCLIClient`; every Cobra, TUI, MCP, raw-download, streaming, and backup
request inherits the same request editor and root context.

- [ ] **Step 5: Format, test, and commit production wiring**

Run:

```bash
gofmt -w cmd/msgvault/cmd/store_resolver.go \
  cmd/msgvault/cmd/store_resolver_test.go cmd/msgvault/cmd/backup.go \
  cmd/msgvault/cmd/backup_daemon_test.go
go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd \
  -run 'Test(OpenHTTPStore|NewBackupFreezer)' -count=1
```

Expected: all local, remote, daemon-autostart, key mismatch, root cancellation,
and backup construction tests pass with zero operation timeout and the marker.

Invoke `kenn:commit`, then:

```bash
git add cmd/msgvault/cmd/store_resolver.go \
  cmd/msgvault/cmd/store_resolver_test.go cmd/msgvault/cmd/backup.go \
  cmd/msgvault/cmd/backup_daemon_test.go
git commit -m "fix: apply daemon CLI policy to every command client" \
  -m "All command surfaces, including configured remotes and backup freeze coordination, must share the same caller-governed duration and cancellation behavior so new client methods cannot silently return to fixed operation deadlines."
```

---

### Task 5: Record the Audit and Run Repository-Wide Acceptance Gates

**Files:**

- Create: `docs/internal/daemon-cli-request-audit.md`
- Modify: `docs/changelog.md`
- Modify: `docs/superpowers/specs/2026-07-25-daemon-cli-timeout-hardening-design.md`

**Interfaces:**

- Consumes: the completed client, server, probe, stats, and production-construction behavior.
- Produces: a durable audit explaining why all current command interaction families inherit the policy.

- [ ] **Step 1: Verify the production construction and background-context inventory**

Run:

```bash
rg -n 'daemonclient\\.New\\(' cmd/msgvault/cmd --glob '*.go' --glob '!**/*_test.go'
rg -n 'context\\.(Background|T[O]DO)\\(\\)' internal/daemonclient \
  --glob '*.go' --glob '!**/*_test.go'
rg -n 'OpenHTTPStore\\(' cmd/msgvault/cmd --glob '*.go' --glob '!**/*_test.go'
```

Expected:

- the only production `daemonclient.New` call is inside `newDaemonCLIClient`;
- no legacy adapter, manifest saver, or generated busy retry uses a background
  context;
- all ordinary Cobra, TUI, and MCP entry points continue using
  `OpenHTTPStore` with their command context.

The remaining intentional server-side backgrounds in
`internal/api/operation_gate.go` and the asynchronous search-index build are
not HTTP client requests and must not be mechanically changed.

- [ ] **Step 2: Write the durable interaction audit**

Create `docs/internal/daemon-cli-request-audit.md` with this table:

```markdown
# Daemon CLI request audit

| Interaction family | Production construction | HTTP form | Cancellation source | Duration policy |
| --- | --- | --- | --- | --- |
| Cobra archive commands | `OpenHTTPStore(cmd.Context())` | generated JSON | explicit method context or client root context | authenticated CLI |
| TUI | `OpenHTTPStore(ctx)` | generated JSON and raw downloads | TUI command context | authenticated CLI |
| MCP | `OpenHTTPStore(cmd.Context())` | generated JSON, search, attachments, manifests | MCP command context | authenticated CLI |
| Streaming maintenance | `OpenHTTPStore(cmd.Context())` | NDJSON streaming | method context; gate waits use the same root | authenticated CLI |
| Raw SQL and aggregates | `OpenHTTPStore(cmd.Context())` | generated JSON | method context through handler to DuckDB `QueryContext` | authenticated CLI; DuckDB cancellation integration-tested |
| Legacy store adapters | client returned by `OpenHTTPStore` | generated JSON | stored root command context | authenticated CLI |
| Backup freeze begin/end | `newDaemonCLIClient(cmd.Context(), ...)` | generated JSON | backup command context | authenticated CLI |
| Local authentication probe | direct bounded `http.Request` | authenticated health JSON | two-second probe context | bounded liveness |
| Browser and third-party API | outside internal CLI client | Huma/API | request context | existing bounded path policy |

Follow the table with the central invariants:

- the request editor marks generated, raw, and streaming calls;
- timeout eligibility uses `requestAuthentication`;
- CLI mode clones but does not replace the transport;
- authenticated CLI and legacy long paths share read/write deadline clearing;
- unmarked raw SQL retains `QueryEndpointTimeout`;
- all three production daemon-client creation paths converge on one helper.
```

- [ ] **Step 3: Update the changelog and design status**

Replace `No notable changes yet.` under `## Unreleased` in
`docs/changelog.md` with:

```markdown
**Bug fixes**

- Daemon-backed CLI, TUI, MCP, statistics, and backup operations now wait for
  completion or caller cancellation instead of failing at fixed HTTP or server
  deadlines on slow storage. Local daemon authentication uses the lightweight
  authenticated health endpoint, while connection setup, browser traffic, and
  ordinary API clients retain protective timeouts.
```

In the design document, change the status to:

```markdown
Implemented and acceptance-tested on 2026-07-25.
```

Only make that status change after the raw-SQL test and all checks below pass.

- [ ] **Step 4: Run formatting, targeted tests, vet, the full suite, and lint**

Run:

```bash
gofmt -w internal/apiprotocol/client.go internal/daemonclient/client.go \
  internal/daemonclient/errors.go internal/daemonclient/store_adapter.go \
  internal/daemonclient/cli.go internal/daemonclient/client_test.go \
  internal/daemonclient/store_adapter_test.go internal/api/server.go \
  internal/api/server_test.go internal/api/middleware_test.go \
  internal/api/handlers_test.go cmd/msgvault/cmd/store_resolver.go \
  cmd/msgvault/cmd/store_resolver_test.go cmd/msgvault/cmd/stats.go \
  cmd/msgvault/cmd/stats_test.go cmd/msgvault/cmd/backup.go \
  cmd/msgvault/cmd/backup_daemon_test.go
go test -tags "fts5 sqlite_vec" ./internal/apiprotocol ./internal/daemonclient \
  ./internal/query ./internal/api ./cmd/msgvault/cmd -count=1
go vet -tags "fts5 sqlite_vec" ./...
make test
make lint
```

Expected: every command exits zero. Do not mark the design implemented or
claim completion if the marked DuckDB cancellation test, full suite, vet, or
lint fails.

- [ ] **Step 5: Review the public diff for content hygiene**

Run:

```bash
git diff --check
git diff --stat origin/main...HEAD
git diff origin/main...HEAD -- docs internal cmd
```

Expected: no whitespace errors, credentials, private downstream project names,
unrelated edits, OpenAPI header additions, flags, or configuration changes.

- [ ] **Step 6: Commit the audit and release note**

Invoke `kenn:commit`, then:

```bash
git add docs/internal/daemon-cli-request-audit.md docs/changelog.md \
  docs/superpowers/specs/2026-07-25-daemon-cli-timeout-hardening-design.md
git commit -m "docs: record daemon CLI timeout guarantees" \
  -m "The timeout fix spans multiple request forms and command surfaces. Preserve the construction, authentication, cancellation, and liveness invariants in one audit so later client additions can be checked without rebuilding the original investigation."
```

- [ ] **Step 7: Confirm the final branch state**

Run:

```bash
git status --short
git log --oneline --decorate -7
```

Expected: the working tree is clean and the branch contains the five
implementation commits after the approved design/spec commits.
