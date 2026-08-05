# Change-feed Request Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prevent browser-origin and high-rate loopback requests from repeatedly taking SQLite's writer lock through the message change feed.

**Architecture:** Wrap only `GET /api/v1/messages/changes` with a guard that runs after request-security classification and before the handler. The guard rejects cross-site browser metadata in keyless mode, then applies a dedicated per-IP token bucket to every authentication mode.

**Tech Stack:** Go `net/http`, `golang.org/x/time/rate`, Huma route registration, testify.

## Global Constraints

- Keep the endpoint as GET and leave its generated API contract unchanged.
- Limit each client IP to two requests per second with a burst of four.
- No authentication mode or loopback address bypasses the dedicated limiter.
- Accept `Sec-Fetch-Site: same-origin`, `Sec-Fetch-Site: none`, and an absent header; reject every other present value in keyless loopback mode.
- Preserve cross-origin API-key access and headerless CLI access.
- New or modified Go tests must use testify.
- All Go test commands use `-tags "fts5 sqlite_vec"`.

---

### Task 1: Pin the route's security boundary

**Files:**
- Create: `internal/api/change_feed_guard_test.go`
- Modify: `internal/api/changes_test.go`

**Interfaces:**
- Consumes: `NewServerWithOptions`, `Server.Router`, `stubChangedMessageLister`, `ErrorResponse`.
- Produces: behavior tests for the route wrapper; no production interface.

- [ ] **Step 1: Add a counting store and request helper**

Add a `calls int` field to `stubChangedMessageLister` and increment it in
`ListChangedMessages`. In `change_feed_guard_test.go`, build a server around
that store and issue real router requests with controllable `Origin`,
`Sec-Fetch-Site`, `RemoteAddr`, and `X-Api-Key` headers.

- [ ] **Step 2: Add browser-origin table tests**

Cover these literal cases in keyless mode:

```go
tests := []struct {
	name, origin, fetchSite string
	wantStatus, wantCalls   int
}{
	{"cross-origin Origin", "https://evil.example", "", http.StatusForbidden, 0},
	{"cross-site metadata", "", "cross-site", http.StatusForbidden, 0},
	{"same-site metadata", "", "same-site", http.StatusForbidden, 0},
	{"same origin", "http://example.com", "", http.StatusOK, 1},
	{"same-origin metadata", "", "same-origin", http.StatusOK, 1},
	{"navigation metadata", "", "none", http.StatusOK, 1},
	{"headerless client", "", "", http.StatusOK, 1},
}
```

Assert rejected requests return `cross_origin_loopback` and never call the
store. Add a separate API-key case showing a cross-origin request reaches the
store.

- [ ] **Step 3: Add the non-bypass rate-limit test**

Send five immediate requests from `127.0.0.1:1234`. Assert the first four are
200, the fifth is 429 with `Retry-After`, and the store was called exactly four
times.

- [ ] **Step 4: Run tests to verify RED**

Run:

```bash
go test -timeout 20m -tags "fts5 sqlite_vec" ./internal/api -run '^TestChangeFeedGuard_' -count=1
```

Expected: browser-origin and fifth-request assertions fail because the route
currently has no dedicated guard.

### Task 2: Implement the guard and limiter lifecycle

**Files:**
- Create: `internal/api/change_feed_guard.go`
- Modify: `internal/api/routes.go`
- Modify: `internal/api/server.go`
- Modify: `internal/api/middleware.go`
- Modify: `internal/api/changes_test.go`
- Modify: `docs/api-server.md`

**Interfaces:**
- Consumes: `Server.requestAuthentication`, `securityFromRequest`,
  `ambientOriginAllowed`, `RateLimiter.Allow`, and `clientIP`.
- Produces: `func (s *Server) changeFeedGuard(http.HandlerFunc) http.HandlerFunc`
  and `func writeRateLimitExceeded(http.ResponseWriter)`.

- [ ] **Step 1: Add the route wrapper**

Create `change_feed_guard.go` with production constants and the guard:

```go
const (
	changeFeedRequestsPerSecond = 2
	changeFeedRequestBurst      = 4
)

func (s *Server) changeFeedGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.requestAuthentication(r).Mode == AuthModeLoopback &&
			s.crossOriginChangeFeedRequest(r) {
			writeError(w, http.StatusForbidden, "cross_origin_loopback",
				"Keyless loopback change-feed requests must be same-origin; configure an API key for cross-origin access")
			return
		}
		if !s.changesRateLimiter.Allow(clientIP(r)) {
			writeRateLimitExceeded(w)
			return
		}
		next(w, r)
	}
}
```

`crossOriginChangeFeedRequest` must reject an Origin mismatch and any present
Fetch Metadata value other than `same-origin` or `none`. Multiple or malformed
values fail closed.

- [ ] **Step 2: Wire lifecycle and route registration**

Add `changesRateLimiter *RateLimiter` to `Server`. Initialize it in
`setupRouter` with `(2, 4)`, close it in `Shutdown`, and register
`s.changeFeedGuard(s.handleMessageChanges)` for `listChangedMessages`.

Extract the shared 429 body/header writer from `RateLimitMiddleware` into
`writeRateLimitExceeded` so both limiters produce the same published error.

- [ ] **Step 3: Keep unrelated feed tests deterministic**

In `newChangesServer`, replace the production limiter with a high-capacity
test limiter and close both the old and replacement limiter with `t.Cleanup`.
Security tests construct their own server and retain the production `(2, 4)`
limiter.

- [ ] **Step 4: Document the polling ceiling**

Update the change-feed polling guidance in `docs/api-server.md` to state that
the endpoint is limited to two requests per second per client IP with a burst
of four and that clients should honor 429 `Retry-After`.

- [ ] **Step 5: Run focused tests to verify GREEN**

Run:

```bash
go test -timeout 20m -tags "fts5 sqlite_vec" ./internal/api -run '^(TestChangeFeedGuard_|TestChangesEndpoint_)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the implementation**

Stage only the production code, tests, and API documentation and commit with a
rationale-focused `fix(api): protect change-feed polling` message.

### Task 3: Remove temporary workflow documents and verify publication

**Files:**
- Delete: `docs/superpowers/specs/2026-08-05-change-feed-request-guard-design.md`
- Delete: `docs/superpowers/plans/2026-08-05-change-feed-request-guard.md`

**Interfaces:**
- Consumes: completed Tasks 1-2.
- Produces: final public branch with no temporary spec or plan.

- [ ] **Step 1: Delete the temporary documents**

Use `apply_patch` to remove both files after implementation is green.

- [ ] **Step 2: Run full verification**

Run `TMPDIR=/tmp make test`, `make lint-ci`, `go vet ./...`, and
`make docs-check`. Require every command to exit zero.

- [ ] **Step 3: Scrub public surfaces**

Scan the final diff, unpushed commit history, commit messages, and PR body with
the private-terms denylist and structural path/identity heuristics. Require zero
hits.

- [ ] **Step 4: Commit, push, and close the review loop**

Commit the document deletion, push HEAD to
`danshapiro/msgvault:pr/messages-changes-feed`, respond to the roborev comment
with the implementation commit, and monitor all PR checks through completion.
