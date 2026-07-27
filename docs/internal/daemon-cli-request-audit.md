# Daemon CLI request audit

## Request policy

The shared daemon-client request editor marks generated JSON, raw downloads,
and NDJSON streams with `X-Msgvault-Client-Class: cli`. The server applies the
CLI duration policy only after the request authenticates by API key or trusted
loopback; a browser session cannot opt into it.

For marked, authenticated routes whose complete production path is
request-cancellable, the server clears its absolute read/write deadlines. The
caller still owns cancellation: Cobra, TUI, and MCP clients all construct their
HTTP store from their root context, and canceling that context cancels the
request and its request-owned work.

Unmarked requests retain the ordinary request timeout. Unmarked raw SQL also
retains `QueryEndpointTimeout`. Legacy long-route behavior is unchanged.

## Request-owned cancellation inventory

| Routes | Request-owned production work | Evidence/status |
| --- | --- | --- |
| `GET /cli/stats` | Global and scoped SQL statistics, scope resolution, database-size query | Global handler cancellation regression plus scoped production-adapter cancellation regression |
| `POST /cli/init-db` | Startup migrations and response statistics | Production-adapter blocked-database cancellation regression |
| `GET /cli/accounts` | Source listing and per-source message counts | Production-adapter blocked-database cancellation regression |
| `GET /cli/collections`, `GET /cli/collection` | Collection listing, membership hydration, and message counts | Context-aware store/adapter path; list route has production-adapter cancellation coverage |
| `POST /cli/account` | Account resolution and display-name update | Production-adapter blocked-database cancellation regression |
| `POST /cli/collections`, `PATCH /cli/collections/{name}/sources`, `DELETE /cli/collections/{name}/sources`, `DELETE /cli/collections/{name}` | Account resolution and collection mutation | Production-adapter blocked-database cancellation regressions |
| `GET /cli/identities` | Scope resolution, source listing, and identity listing | Production-adapter blocked-database cancellation regression |
| `POST /cli/delete-deduped/plan`, `POST /cli/delete-deduped` | Plan counts, optional SQLite backup, and destructive SQL | Context-aware count/backup/delete path plus production-adapter cancellation regressions; execute retains the protective ceiling below until backup cancellation is proven |
| `POST /cli/rebuild-fts` | FTS schema maintenance and batched rebuild | Fully context-aware store maintenance path plus production-adapter cancellation regression |
| `GET /cli/search` | Scope resolution and foreground search | Request context reaches production search SQL; the synchronous quick index probe remains a protective-ceiling exception and the full first-search check/backfill is deliberately detached maintenance |
| `POST /cli/sync`, `POST /cli/sync-full`, `POST /cli/verify`, `POST /cli/repair-encoding`, `POST /cli/run` | Streaming runner, operation-gate wait, subprocess/network/database work | Existing runner interfaces take the request context and streaming cancellation tests cover their specialized paths |

The production `storeAPIAdapter` is statically asserted to implement the
complete context-aware CLI store extension. A compatibility fallback remains
for older in-process test/embedding stores; cancellation claims above describe
the production adapter, not that fallback.

SQLite database size is read through context-aware `PRAGMA page_count` and
`PRAGMA page_size` queries. It reports the logical allocated size of the main
database, including committed pages still represented in WAL, but excludes WAL
framing and `-wal`/`-shm` sidecar overhead. This replaces the prior
noninterruptible main-file `stat` and keeps `/cli/stats` and `/cli/init-db`
fully request-cancellable.

## Protective-ceiling exceptions

The routes below retain a generous `DaemonLongRequestTimeout` (30 minutes) even
when marked. Each still receives caller cancellation, but some production phase
uses filesystem, planner, MIME/decompression, or synchronous cache work that
cannot yet be interrupted at every point. The method is part of the inventory
because identity reads and mutations share a path but not the same risk.

| Method and route | Noninterruptible phase |
| --- | --- |
| `GET /cli/cache-stats` | Analytics-cache filesystem inspection |
| `POST /cli/build-cache` | Cache build lock/publication and filesystem work |
| `POST /cli/add-calendar/plan` | Planner/config and provider preflight work |
| `POST /cli/delete-staged/plan` | Deletion planner and attachment/filesystem inspection |
| `POST /cli/deletion-manifests` | Atomic manifest filesystem publication |
| `POST /cli/embeddings/plan` | Vector/cache planner and filesystem inspection |
| `GET /cli/message` | Message materialization and MIME processing |
| `GET /cli/message/raw` | Raw-message decompression and response streaming |
| `GET /cli/attachment` | Packed/loose attachment filesystem reads and streaming |
| `GET /cli/search` | Synchronous context-free quick FTS-index probe |
| `POST /cli/deduplicate/plan` | Deep duplicate scan and planner work |
| `POST /cli/delete-deduped` | Optional SQLite backup; cleanup of a canceled partial target is not yet proven |
| `POST /cli/identities` | Synchronous post-mutation identity-cache refresh |
| `DELETE /cli/identities` | Synchronous post-mutation identity-cache refresh |

`TestMarkedCLIProtectiveCeilingInventory` pins this allowlist and its
method-sensitive middleware behavior. Routes should leave this table only after
their whole production path has an end-to-end cancellation regression.

## Deliberately detached work

- The first CLI search may start one background FTS completeness
  probe/backfill. It uses a cancellation-detached context because it is
  daemon-owned repair work shared by later searches; the foreground search
  remains request-cancellable.
- A successful account-identity mutation may schedule a full analytics-cache
  rebuild after its synchronous refresh. That rebuild is daemon-owned and
  intentionally outlives the HTTP request; the mutation itself and its
  synchronous database work remain request-cancellable.

## Client construction

| Interaction family | Production construction | Cancellation source |
| --- | --- | --- |
| Cobra archive commands | `OpenHTTPStore(cmd.Context())` | Root command context |
| TUI | `OpenHTTPStore(ctx)` | TUI command context |
| MCP | `OpenHTTPStore(cmd.Context())` | MCP command context |
| Streaming maintenance | Same HTTP store plus NDJSON methods | Method/root context |
| Raw SQL and aggregates | Same HTTP store plus generated/raw methods | Method/root context through DuckDB `QueryContext` |
| Backup freeze begin/end | `newDaemonCLIClient(cmd.Context(), ...)` | Backup command context |
| Local authentication probe | Direct bounded request | Two-second probe context |
