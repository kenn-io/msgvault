# Daemon CLI Timeout Hardening Design

## Status

Implemented and acceptance-tested on 2026-07-25.

## Problem

msgvault routes archive access from the CLI, TUI, and MCP server through the
daemon. That boundary currently has several independent fixed deadlines:

- a two-second local-daemon authentication probe;
- a whole-request timeout on the shared daemon HTTP client;
- a 60-second default server request timeout;
- a 120-second server timeout for raw SQL queries; and
- a path allowlist for a subset of streaming or known-long CLI operations.

These layers disagree about which requests may be slow. In particular, the
authentication probe calls the archive-wide statistics endpoint, and several
CLI operations that scan a large archive still use ordinary API endpoints.
On modest NAS hardware, legitimate work can therefore be canceled even though
the daemon is healthy and the user is willing to wait.

The path allowlist also makes the policy fragile: each new CLI endpoint must be
remembered in a separate timeout switch, and CLI commands that intentionally
reuse a general API endpoint cannot be represented accurately by path alone.

## Goals

- Let explicit CLI-originated archive operations run until they complete or
  the caller cancels them.
- Apply the same behavior to local and configured-remote daemons.
- Cover the CLI, TUI, MCP server, backup coordination, current CLI-compatible
  endpoints, and general API endpoints invoked by those clients.
- Preserve protective deadlines for browser sessions and ordinary API clients.
- Keep connection, authentication, discovery, and lifecycle liveness probes
  short so an unreachable daemon still fails promptly.
- Make the policy apply automatically to future daemon requests made through
  msgvault's internal CLI client.
- Preserve cancellation across every client and handler boundary.

## Non-goals

- Removing timeouts from browser or third-party API traffic.
- Removing the daemon's concurrency and operation gates.
- Guaranteeing behavior through third-party reverse proxies, load balancers,
  or network devices that impose their own deadlines.
- Optimizing every slow archive query in this change. Query optimization and
  cached statistics remain useful follow-up work, but correctness must not
  depend on a particular disk speed.
- Making daemon discovery wait forever when no usable process is listening.

## Considered Approaches

### 1. Request-origin classification

The internal daemon client marks requests as CLI-originated. The daemon honors
that classification only for API-key-authenticated or trusted keyless-loopback
requests, then removes active-operation read, execution, and write deadlines.

This is the selected approach. It represents user intent directly, covers
general endpoints used by CLI commands, and applies to future client methods
without expanding a route allowlist.

### 2. CLI-specific endpoint duplication

Every CLI command could use a route under `/api/v1/cli/`, and the server could
make that prefix unbounded. This offers obvious path separation, but it would
duplicate general endpoints such as search, aggregates, raw SQL, message
details, and statistics. It would also preserve the same maintenance hazard:
new commands could accidentally use the bounded endpoint.

### 3. Raise global server deadlines

The daemon could replace 60 and 120 seconds with a much larger value. This is
simple but still fails on sufficiently slow hardware, and it weakens resource
protection for browser and ordinary API requests. It does not encode the
user's explicit willingness to wait.

## Selected Architecture

### Request classification

Requests made by msgvault's internal daemon client carry an internal protocol
header whose value identifies the request as CLI-originated. The header name
and value are shared constants rather than duplicated string literals.

The timeout middleware recognizes the marker only when the request's effective
authentication mode is:

- `api_key`, for local keyed daemons and configured remote daemons; or
- `loopback`, for the supported keyless local-daemon mode.

Browser-session authentication does not opt into CLI timeout behavior, even if
a browser sends the marker. Missing or invalid authentication also does not opt
in. Authentication and authorization middleware continue to make the final
access decision; the marker grants no endpoint permission and bypasses no
operation gate, rate limit, CSRF check, or validation.

The timeout middleware must obtain this mode from the same
`requestAuthentication` classification used by access-control middleware. It
must not reproduce API-key, loopback, proxy, forwarded-header, or browser
session logic in a second classifier. That shared classification is the single
source of truth for both authorization and eligibility for CLI duration
policy.

Possession of a valid API key already permits invoking existing unbounded
maintenance endpoints. The marker therefore changes request-duration policy,
not the authorization model.

### Client deadlines

The internal client gains an explicit CLI request mode. In that mode:

- generated and raw requests include the CLI-origin header;
- the whole-request `http.Client.Timeout` is disabled so response-header and
  response-body time are governed by the caller's context;
- bounded transport-level connection establishment and TLS handshake deadlines
  are retained, including for configured remote daemons, so a black-holed or
  unreachable host fails promptly; and
- streaming and non-streaming methods use the same cancellation semantics.

Ordinary bounded construction remains available for liveness checks and tests.
All production constructors used by CLI commands are audited and placed in CLI
mode, including the normal `OpenHTTPStore` path and backup freeze coordination.
Existing bounded-client coverage, including the intent of
`TestNew_DefaultTimeout`, is retained and adapted to distinguish bounded
construction from CLI mode rather than deleted.

The CLI request context supplied to `OpenHTTPStore` becomes the fallback
context for legacy adapter methods whose required interfaces do not accept a
context. New and already-context-aware methods continue to use their explicit
caller context. This removes uncancelable `context.Background()` requests
without forcing unrelated public interface changes.

### Server deadlines

For an authenticated CLI-originated request, the server:

- does not add the standard request-context deadline;
- clears the active connection's read deadline after headers are available, so
  a large CLI request body is not cut off by the server's general
  `ReadTimeout`;
- clears the active connection's write deadline, so a response is not severed
  by the server's general `WriteTimeout`; and
- preserves the incoming request context so disconnects and Ctrl+C propagate
  cancellation.

The connection must still deliver request headers within the server's normal
read deadline. This is a connectivity/liveness boundary, not a bound on
archive work.

Unmarked traffic retains the current policy:

- the normal server request timeout for ordinary endpoints;
- the dedicated raw-query timeout;
- existing explicitly unbounded maintenance routes; and
- the server's normal connection deadlines.

The existing long-route behavior is implemented through the same deadline
clearing helper so marked CLI requests and explicitly long API routes do not
drift apart.

### Raw SQL cancellation safety

Raw SQL is the highest-risk request to make unbounded because one pathological
DuckDB query can consume every core for minutes. CLI raw SQL may bypass the
120-second server ceiling only after an integration test demonstrates that
canceling the marked HTTP request interrupts the underlying query engine work,
not merely the handler's wait for a result.

The production DuckDB path already uses `QueryContext`, and its engine-level
cancellation test proves the driver can interrupt a long cross join. The new
test must cover the complete marked request path through the daemon handler and
observe prompt query termination after cancellation. If that proof cannot be
made reliable on supported platforms, CLI raw SQL retains the 120-second
ceiling while the rest of the CLI policy proceeds.

### Local daemon authentication probe

The local-daemon authentication probe keeps its two-second deadline but calls
the existing authenticated `/api/v1/health` endpoint instead of
`/api/v1/stats`.

Health verifies the configured API key without archive-wide count queries.
The probe remains deliberately bounded because it answers whether a discovered
daemon is reachable and accepts the active credentials; it does not perform
archive work.

The runtime authentication fingerprint remains the first mismatch check.
Existing actionable restart guidance for rejected or stale keys is preserved.

### CLI interaction audit

The implementation audit covers every production construction and request path
used by:

- ordinary Cobra commands opened through `OpenHTTPStore`;
- streaming sync, import, repair, rebuild, verify, and generic CLI-run
  operations;
- statistics, cache inspection, search, aggregates, raw SQL, message display,
  exports, deletions, deduplication, identities, collections, and account
  management;
- TUI query and action adapters;
- MCP query, search, attachment, and deletion-manifest adapters; and
- backup freeze begin/end coordination.

For each interaction, the audit checks:

1. the request carries CLI-origin classification;
2. no whole-operation client timeout remains;
3. the caller's context reaches the HTTP request;
4. the server handler passes the request context into every potentially
   blocking database, filesystem, or query-engine operation; a missing
   context-aware method must be added or the operation must retain its existing
   safety ceiling until interruption is demonstrably supported;
5. streaming responses flush progress and remain cancelable; and
6. operation-gate retries stop when the caller context is canceled.

Global `msgvault stats` is routed through the context-aware CLI statistics
method, matching scoped statistics, rather than the legacy context-free store
adapter.

Any context-free adapter method that must remain for interface compatibility
uses the client's root command context instead of `context.Background()`.

## Request Flow

1. A Cobra command starts with its signal-aware command context.
2. `OpenHTTPStore` discovers or starts the local daemon, or selects the
   configured remote daemon.
3. The bounded authenticated health probe validates a reused local daemon.
4. The returned daemon client is configured in CLI request mode and retains
   the command context.
5. Each daemon request carries the CLI-origin marker and an API key when
   configured.
6. The server classifies the request's authentication mode.
7. Authenticated CLI requests keep the caller context without fixed operation
   deadlines; other requests receive the existing path-based deadlines.
8. Ctrl+C, process shutdown, or transport disconnect cancels the request and
   propagates through context-aware work.

## Error Handling and Observability

- A bounded liveness probe reports connection, deadline, and authentication
  failures as probe failures with the existing daemon restart guidance.
- A canceled CLI request reports cancellation rather than a misleading server
  query-timeout error.
- Ordinary API requests that exceed their budget retain structured
  `query_timeout` responses.
- Existing in-progress request warnings continue to report long work in the
  daemon log.
- Request completion logs retain duration and request ID. They may include the
  resolved request class, but must never log credentials or the API key.
- If the response controller cannot adjust deadlines, as with an in-memory
  test recorder, the server treats deadline clearing as best-effort and still
  preserves context behavior.

## Compatibility

- No command-line flags or configuration keys are added.
- The OpenAPI contract does not advertise the internal request-class header.
- Third-party API clients retain existing timeout behavior.
- Older daemons ignore the CLI header and retain their current deadlines; a
  newer CLI cannot make an older daemon unbounded.
- Older CLIs do not send the marker and retain the current behavior against a
  newer daemon.
- Existing explicitly long maintenance endpoints remain unbounded for
  compatible clients.

## Testing Strategy

All Go tests use testify and the required `fts5 sqlite_vec` build tags.

### Server policy tests

- With a deliberately tiny standard timeout, a marked keyless-loopback request
  remains active until the test releases it.
- A marked API-key-authenticated request behaves the same way.
- The equivalent unmarked request receives the existing timeout behavior.
- A browser-session request cannot opt into CLI timeout behavior.
- An invalid API key plus the CLI marker is rejected and does not invoke the
  protected handler.
- A proxied non-loopback request carrying the marker without a valid API key
  remains bounded and is rejected; forwarded headers cannot manufacture
  loopback or CLI trust.
- Canceling the caller context stops a marked request.
- The raw-query deadline remains effective for unmarked traffic and is removed
  for authenticated CLI traffic only after a canceled marked raw query is
  proven to interrupt the underlying DuckDB work promptly.
- Existing long routes and marked CLI requests share deadline-clearing
  behavior.

### Client policy tests

- CLI-mode generated requests carry the marker and API key.
- CLI-mode raw and streaming requests carry the same marker.
- CLI mode has no `http.Client.Timeout`; bounded mode retains its configured
  timeout.
- CLI mode retains bounded dial and TLS-handshake behavior while leaving
  response-header and response-body duration to the caller context.
- The existing default-timeout test is reworked to keep asserting bounded
  construction while separate coverage asserts unbounded CLI operation time.
- Canceling the command/root context cancels legacy context-free adapter
  requests.
- Busy-operation retries stop after caller cancellation.

### Authentication probe tests

- The probe requests `/api/v1/health`, not `/api/v1/stats`.
- Correct API-key and trusted keyless cases succeed.
- Rejected and stale authentication retain actionable errors.
- A deliberately slow stats handler cannot delay or fail daemon reuse.
- A slow health handler still fails at the bounded probe deadline.

### Command and audit regression tests

- Global and scoped stats use the context-aware CLI route.
- Representative non-streaming, streaming, raw-download, TUI, MCP, and backup
  requests receive CLI request mode.
- A route inventory test or equivalent centralized assertion prevents future
  CLI client construction from silently returning to bounded mode.

## Acceptance Criteria

- No archive operation initiated through msgvault's internal CLI client has a
  fixed client or server wall-clock deadline once its cancellation path is
  proven to interrupt the underlying work.
- Ctrl+C and caller-context cancellation still terminate those operations.
- Canceling marked CLI raw SQL demonstrably interrupts the underlying DuckDB
  query. Raw SQL retains its existing 120-second server ceiling until this
  criterion passes, and the hardening work is reported as incomplete if the
  proof cannot be made reliable.
- Local daemon discovery and authentication fail promptly when the daemon is
  unreachable or rejects credentials.
- Configured remote daemons retain bounded dial and TLS-handshake timeouts even
  though CLI operation time is caller-governed.
- Browser sessions and ordinary API clients retain existing protective
  deadlines.
- Timeout eligibility and endpoint authorization use the same
  `requestAuthentication` result, including its proxy and loopback rules.
- The two reported failure modes are covered: authentication no longer invokes
  statistics, and `msgvault stats` can run beyond the standard daemon timeout.
- The full repository test suite, formatting, vetting, and lint checks pass
  before the implementation commit.
