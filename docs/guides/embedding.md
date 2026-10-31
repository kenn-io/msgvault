---
title: Go library
description: Open archives, run synchronization, and serve message reads from Go.
---

`go.kenn.io/msgvault/pkg/archive` exposes archive operations to Go programs.
Use it in an import tool, a local application, or a service that needs access to
msgvault data. SQLite and PostgreSQL use the same message model and read API.

Callers supply database locations, credentials, and importer options explicitly.
The package does not discover personal configuration or start a daemon. Calls
accept a context; the caller controls cancellation, scheduling, and shutdown.

This package is newer than the latest release.

For clients of a hosted message MCP endpoint, the
[external credential provider](credential-providers.md) package validates access
obtained from a trusted local helper. Hosts still own approval and scoped reads.

## Run scoped MCP reads

`go.kenn.io/msgvault/pkg/mcp.Reader` runs native tools against a caller-owned
archive without a daemon or listener. It supports `get_message`,
`list_messages`, `list_thread`, `search_metadata`, `search_message_bodies`,
`get_stats`, and `get_attachment`. The application owns member authentication,
current permissions, and its MCP transport.

Open an archive as described below, then create one reader for its lifetime:

```go
reader, err := msgvaultmcp.NewReader(messages, msgvaultmcp.ReaderOptions{
    AttachmentsDir: attachmentDirectory,
})
if err != nil {
    return err
}
defer reader.Close()

tool, err := msgvaultmcp.ReadTool("list_messages")
if err != nil {
    return err
}
sdkmcp.AddTool(server, tool, func(ctx context.Context, req *sdkmcp.CallToolRequest,
    args map[string]any) (*sdkmcp.CallToolResult, any, error) {
    // Application-owned admission returns current source IDs and a release
    // function. Hold that authority fence through the entire operation.
    sourceIDs, release, err := admitRead(ctx)
    if err != nil {
        return nil, nil, err
    }
    defer release()
    return reader.Call(ctx, sourceIDs, req, args)
})
```

Here `msgvaultmcp` is `go.kenn.io/msgvault/pkg/mcp`, and `sdkmcp` is
`github.com/modelcontextprotocol/go-sdk/mcp`. Use the SDK's generic `AddTool`
so it validates both arguments and output against the native schemas. The
reader preserves native paging, body windows, search, errors and attachment
chunk semantics. It does not accept arbitrary query engines or execute other
tools just because they appear in the catalog.

Supply current source IDs on **every call**. Nil and empty scopes permit no
sources. Source scope is applied inside native SQL to account resolution,
references, lists, search results, counts and attachment metadata before bytes
are read. Arguments can narrow that scope. This API grants whole sources; it
cannot authorize selected messages within a source.

The reader also supports the PostgreSQL-only `CGO_ENABLED=0` library profile.
Each call uses one native read snapshot. The application must keep its own
permission/admission transaction valid until the call returns. Cached
attachment continuations recheck the current call's source scope and content
hash before returning bytes. A previous successful chunk grants no access to
later chunks. An empty `AttachmentsDir` leaves attachment bytes unavailable;
it does not discover local configuration. Stop and drain calls before closing
the reader, then close the archive. Closing the reader releases cached bytes
and leaves the caller-owned archive open.

### Reuse other read definitions

`ReadTool(name)` returns an independent copy of a native base read definition,
including nested schemas and annotations. Schemas use `map[string]any`.
Unknown names, writes, and tools requiring optional backends (including
`query_sql` and `search_documents`) return an error. A definition alone does
not register a handler, grant access or make a tool executable through Reader.
For tools outside Reader's supported subset, the caller supplies execution.
Do not treat `archive.WithSourceScope` as a general permission boundary for
arbitrary query engines or handlers; its HTTP contract remains restricted to
the operations listed below.

## Open an archive

For SQLite, run schema setup before opening the archive:

```go
if err := archive.SetupSQLite(ctx, "archive.db"); err != nil {
    return err
}
messages, err := archive.OpenSQLite(ctx, "archive.db")
if err != nil {
    return err
}
defer messages.Close()
```

Setup creates or upgrades the schema. Open validates an initialized archive
without running schema DDL. It fails if the file does not exist and never
creates the file or its directory. Stop and drain operations before closing it.
SQLite uses msgvault's normal CGO build with the `fts5 sqlite_vec` build tags.

For PostgreSQL, supply a URL or libpq keyword DSN and an optional schema:

```go
options := archive.PostgreSQL{
    URL:                databaseURL,
    Schema:             "message_archive",
    MaxOpenConnections: 8,
}
if err := archive.Setup(ctx, options); err != nil {
    return err
}
messages, err := archive.Open(ctx, options)
if err != nil {
    return err
}
defer messages.Close()
```

An empty Schema preserves the connection's search path. Setup requires schema
ownership. It creates a missing schema, which needs `CREATE` on the database;
the owner of a schema created in advance does not need that privilege. Open can
use a separate role granted `USAGE` on the schema, `SELECT`, `INSERT`,
`UPDATE`, and `DELETE` on its tables, and `USAGE`, `SELECT` on its sequences.
A missing or incompatible schema fails without trying to upgrade it. Serialize
setup with other upgrades.

Each running sync or purge holds one connection for its session advisory lock
and needs another for its work. `MaxOpenConnections` 0 keeps msgvault's default
pool of 25 connections. Otherwise, set it to at least the number of concurrent
syncs and purges plus one, with room for readers. Open rejects 1 and negative
values. Transaction poolers do not
preserve the lock. Database connections may contain credentials and must not
be logged.

Normal builds retain the native database backends. PostgreSQL-only programs can
also build without CGO; that optional profile has no SQLite or DuckDB driver.

## Serve the API

`archive.NewServer` exposes the existing msgvault API with its full server
options. Supply an archive's `Store()` and `QueryEngine()`, plus any additional
services the application uses. No listener starts until the caller starts one.

```go
api := archive.NewServer(archive.ServerOptions{
    Store:  messages.Store(),
    Engine: messages.QueryEngine(),
})
defer func() {
    if err := api.Shutdown(ctx); err != nil {
        log.Printf("stop archive API: %v", err)
    }
}()
handler := api.Handler(authorize)
```

`Handler` registers the same operations as the daemon and calls the supplied
authorization function with each operation's ID, method, and path. The caller
chooses which operations to expose. A nil callback adds no authorization.
Applications own authentication, browser origin checks, and CSRF policy.
The OpenAPI document and schema routes, such as `/openapi.json`, do not pass
through the callback; wrap or mount the handler to control them.

Without a source scope, an admitted operation acts as the archive owner and
does not also require daemon credentials. `/api/session` reports its
authentication mode as `caller`. Owner-only operations follow the callback alone:

- Daemon shutdown calls the server's shutdown callback without the daemon's
  shutdown token.
- Backup freeze skips the daemon's same-host check.
- Agent token management does not require the owner API key.
- Every admitted request shares one owner identity, including for Codex login
  sessions.

Authorized operations then pass through the configured `OperationGate`, so
rejected requests never wait on archive work. The handler also applies
`RequestTimeout`, request IDs, panic recovery, and default
`Cache-Control: no-store` headers. Unlike the daemon, it ignores the CLI
client-class header: no request header lifts `RequestTimeout` or the host
server's connection deadlines. `Router()` is also available when the daemon's
transport middleware is wanted.

The existing `pkg/client` works with this handler. Availability of analytics,
attachments, scheduling, and other services follows the supplied server options
and storage backend. Unrestricted requests have no separate feature allowlist.
Stop requests and call `Shutdown` before closing the archive.

`Archive.Store()` also exposes the underlying storage operations directly for
Go callers, without requiring HTTP.

### Restrict reads to authorized sources

An application that shares an archive can restrict each request to sources its
caller may read. Resolve those source IDs through the application's own access
policy, then attach them before calling the handler:

```go
// authorizedSourceIDs comes from the application's authorization decision.
ctx := archive.WithSourceScope(r.Context(), authorizedSourceIDs)
handler.ServeHTTP(w, r.WithContext(ctx))
```

These are archive `int64` source IDs, not provider account names or identifiers.
The function copies the IDs. Passing nil or an empty slice authorizes no
sources; omitting the scope keeps the existing unrestricted behavior. Query
parameters and request headers cannot create or widen this scope. Account and
collection filters narrow the authorized source set. A collection with no
authorized member returns the same error as a missing collection.

Supply the native `Archive.Store()` in `ServerOptions.Store`. Scoped reads use
one database snapshot and the native SQL query engine. Source predicates apply
before totals, pagination, message bodies, conversation windows, and attachment
metadata are read. Account discovery includes only authorized sources and
their counts. A merge target outside the scope is omitted from
`merged_into_source_id`. Objects outside the scope return the same not-found
response as missing objects. The archive releases its read snapshot before
response writes, so slow response delivery does not occupy database connections.

At most two scoped or agent read snapshots are open at once, which leaves
database connections for synchronization and unrestricted reads. Additional
scoped requests wait for a slot. A request whose context ends while waiting
returns 503 with `source_scope_busy`.

Scoped requests support these operations:

| Operation | Route |
|---|---|
| `listMessages` | `GET /api/v1/messages` |
| `searchMessages` | `GET /api/v1/search` with default or `mode=fts` |
| `getMessage` | `GET /api/v1/messages/{id}` |
| `getConversation` | `GET /api/v1/conversations/{id}` |
| `getAttachment` | `GET /api/v1/attachments/{id}` (metadata) |
| `listCLIAccounts` | `GET /api/v1/cli/accounts` |

Other operations return 403 under a source scope. These include attachment
downloads, inline MIME parts, and vector or hybrid search. Message bodies are
returned, but images that reference inline parts do not load under a scope. A custom store that cannot provide native
scoped reads returns 503. The operation callback still chooses which supported
operations the application admits. Source scopes apply to `Handler`; they do
not turn arbitrary `Store` or `QueryEngine` calls into an authorized API.

The application owns authentication and authorization lifetime. If its decision
uses a transaction or lock to prevent concurrent access changes, keep that
transaction or lock until `ServeHTTP` returns. A successful authorization
callback alone does not hold it open during the read. The archive's read
snapshot does not replace that application-owned transaction.

## Run synchronization

The package currently exposes the existing Slack and Discord importers through
`SyncSlack` and `SyncDiscord`. Other source types remain available through the
standalone CLI; their stored data is readable through this library.

`SlackOptions` and `DiscordOptions` are the native importer options. They expose
conversation filters, history repair, attachment storage, progress callbacks,
and provider-specific controls. The library does not impose an application
scope allowlist or force public-channel-only collection.

`InspectSlack` reports authenticated identity and granted scopes;
`SlackChannels` includes conversation-kind metadata. `InspectDiscord` verifies
bot access to a guild, and `DiscordChannels` includes permission overwrites.
Applications can use that evidence to apply their own credential and selection
rules before calling an importer.

A zero source ID uses normal source discovery. To continue an existing source
with replacement credentials, use `BindSlack` with that source ID and the same
workspace, then sync with the returned ID. Stop the old synchronization first.
The source and message identities remain stable. Messages still count as sent
by "me" only when the source's original user sent them. If the replacement
credential belongs to a different user, the first sync rebuilds that user's
progress with a full history walk. `BindDiscord` reuses a source by guild ID.

`Archive.Progress` returns the last run and saved provider checkpoints without
starting a run. Provider access and retention still determine what can be
collected; a completed run does not establish historical completeness.

Exact Slack `ChannelIDs` selection scopes reply searches to each selected
channel; selected DMs and group DMs audit their own threads. A non-nil empty
selection collects nothing; nil keeps normal importer selection. A selected
channel that the credential user can no longer list is skipped and returned in
the summary's `UnavailableChannels`.

Discord's optional `PublicChannels` selection excludes private threads and
refreshes all archived-thread pages of selected parents on each run. This adds
catalog requests for large histories, while message pagination remains
resumable. A selected parent that the guild no longer lists is reported as a
non-fatal `unknown_channel` catalog issue. Use the normal guild configuration
when that restriction is not wanted.

## Remove archived data

`PurgeChannel` removes a channel and its retained threads. `PurgeSource` removes
a source and its retained message data. Both serialize deletion with source
synchronization and refuse a source with a queued or running daemon sync
operation. They remove associated bodies, raw payloads, metadata, search
entries, and packed attachment mappings no other message references, and they
advance the derived-data revision so analytics caches rebuild without the
deleted rows. Channel deletion invalidates its saved import coverage so an
explicit full import can restore it.

Deletion does not prevent a future import from collecting the same provider data.
Callers decide whether to change selection or allow reimport. Attachment files,
external indexes, backups, and copies already read are managed separately.
