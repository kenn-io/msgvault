---
title: Go library
description: Open archives, run synchronization, and serve message reads from Go.
---

Use `go.kenn.io/msgvault/pkg/archive` to open msgvault archives, run syncs, and
read messages from your Go program. It works for import tools, local
applications, and services, and both SQLite and PostgreSQL share the same
message model and read API.

You pass database locations, credentials, and importer options directly to the
package. It does not discover personal configuration or start a daemon. Calls
take a `context.Context`, and you control cancellation, scheduling, and
shutdown.

If you are building a client for a hosted message MCP endpoint, the
[external credential provider](credential-providers.md) package validates access
obtained from a trusted local helper. The hosting service still owns approval
and scoped reads.

## Run scoped MCP reads

`go.kenn.io/msgvault/pkg/mcp.Reader` runs native MCP tools directly against an
archive you own, without a daemon or network listener. It supports
`get_message`, `list_messages`, `list_thread`, `search_metadata`,
`search_message_bodies`, `get_stats`, and `get_attachment`. Your application owns
member authentication, current permissions, and the MCP transport.

Open an archive as shown in [Open an archive](#open-an-archive), then create one
reader for its lifetime:

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

In this example, `msgvaultmcp` is `go.kenn.io/msgvault/pkg/mcp` and `sdkmcp` is
`github.com/modelcontextprotocol/go-sdk/mcp`. Use the SDK's generic `AddTool`
so it validates both arguments and output against the native schemas. The reader
keeps native paging, body windows, search, errors, and attachment chunk
semantics intact. It does not accept arbitrary query engines or execute other
tools only because they appear in the catalog.

Pass current source IDs on **every call**. A nil or empty slice permits no
sources. The reader applies source scope inside native SQL to account
resolution, references, lists, search results, counts, and attachment metadata
before reading any bytes. Tool arguments can narrow that scope further. This API
authorizes whole sources; it cannot grant access to selected messages inside a
source.

The reader also supports the PostgreSQL-only `CGO_ENABLED=0` library profile.
Each call runs against one native read snapshot, and your application must keep
its own permission or admission transaction valid until the call returns. Cached
attachment continuations recheck the current call's source scope and content
hash before returning bytes. A previous successful chunk grants no access to
later chunks. Leaving `AttachmentsDir` empty makes attachment bytes unavailable
rather than discovering local configuration. Stop and drain in-flight calls
before you close the reader, and then close the archive. Closing the reader
releases cached bytes while leaving your archive open.

### Reuse other read definitions

Call `ReadTool(name)` to get an independent copy of a native base read
definition, including nested schemas and annotations. Schemas use
`map[string]any`. Unknown names, write tools, and tools that need optional
backends, including `query_sql` and `search_documents`, return an error.

A definition on its own does not register a handler, grant access, or make a
tool runnable through `Reader`. For tools outside `Reader`'s supported subset,
you provide the execution handler. Do not treat `archive.WithSourceScope` as a
general permission boundary for arbitrary query engines or handlers. Its HTTP
contract applies only to the operations listed in
[Restrict reads to authorized sources](#restrict-reads-to-authorized-sources).

## Open an archive

For SQLite, run schema setup before you open the archive:

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

`SetupSQLite` creates or upgrades the schema. `OpenSQLite` validates an
initialized archive without running schema DDL. If the file does not exist,
`OpenSQLite` fails without creating the file or its directory. Stop and drain
all operations before closing the archive. SQLite uses msgvault's normal CGO
build with the `fts5 sqlite_vec` build tags.

For PostgreSQL, pass a URL or libpq keyword DSN and an optional schema name:

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

Leaving `Schema` empty preserves the connection's search path. `Setup` requires
schema ownership. When the schema does not exist yet, `Setup` creates it, which
requires `CREATE` on the database; the owner of a schema created in advance does
not need that privilege. `Open` can use a separate role that has `USAGE` on the
schema, `SELECT`, `INSERT`, `UPDATE`, and `DELETE` on its tables, and `USAGE`
and `SELECT` on its sequences. If the schema is missing or incompatible, `Open`
fails without trying to upgrade it. Serialize `Setup` with other upgrades.

Each running sync or purge holds one connection for its session advisory lock
and needs a second connection for its work. Setting `MaxOpenConnections` to `0`
keeps msgvault's default pool of 25 connections. Otherwise, set it to at least
the number of concurrent syncs and purges plus one, with room left for readers.
`Open` rejects `1` and negative values. Transaction poolers do not preserve
session advisory locks. Database connections can contain credentials; do not
log them.

Normal builds include the native database backends. PostgreSQL-only programs can
also build without CGO; that optional profile omits the SQLite and DuckDB
drivers.

## Serve the API

`archive.NewServer` exposes the existing msgvault HTTP API with its full server
options. Pass an archive's `Store()` and `QueryEngine()`, along with any
additional services your application uses. No network listener starts until you
start one.

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

`Handler` registers the same operations as the daemon and invokes your
authorization callback with each operation's ID, method, and path. You decide
which operations to expose; passing a nil callback adds no authorization. Your
application owns authentication, browser origin checks, and CSRF policy. The
OpenAPI document and schema routes, such as `/openapi.json`, bypass the
callback; wrap or mount the handler to control access to them.

Without a source scope, an admitted operation runs as the archive owner and does
not require daemon credentials. `/api/session` reports its authentication mode
as `caller`. Owner-only operations follow your callback alone:

- Daemon shutdown calls the server's shutdown callback without the daemon's
  shutdown token.
- Backup freeze skips the daemon's same-host check.
- Agent token management does not require the owner API key.
- Every admitted request shares one owner identity, including for Codex login
  sessions.

Authorized operations pass through the configured `OperationGate`, so rejected
requests never wait on archive work. The handler also applies `RequestTimeout`,
request IDs, panic recovery, and default `Cache-Control: no-store` headers.
Unlike the daemon, it ignores the CLI client-class header, and no request header
lifts `RequestTimeout` or your host server's connection deadlines. If you want
the daemon's transport middleware as well, use `Router()`.

The existing `pkg/client` works against this handler. Analytics, attachments,
scheduling, and other services are available according to the server options and
storage backend you supply; unrestricted requests have no separate feature
allowlist. Stop incoming requests and call `Shutdown` before you close the
archive.

If you want to access storage directly in Go without HTTP, call
`Archive.Store()`.

### Restrict reads to authorized sources

If your application shares an archive across users, you can restrict each
request to the sources its caller is allowed to read. Resolve those source IDs
through your own access policy, then attach them to the request context before
calling the handler:

```go
// authorizedSourceIDs comes from the application's authorization decision.
ctx := archive.WithSourceScope(r.Context(), authorizedSourceIDs)
handler.ServeHTTP(w, r.WithContext(ctx))
```

These IDs are archive `int64` source IDs, not provider account names or
identifiers, and `WithSourceScope` copies the slice. Passing nil or an empty
slice authorizes no sources, while omitting the scope keeps the existing
unrestricted behavior. Query parameters and request headers cannot create or
widen this scope. Account and collection filters narrow the authorized source
set, and a collection with no authorized member returns the same error as a
missing collection.

Pass the native `Archive.Store()` in `ServerOptions.Store`. Scoped reads run on
one database snapshot using the native SQL query engine, applying source
predicates before reading totals, pagination, message bodies, conversation
windows, and attachment metadata. Account discovery returns only authorized
sources and their counts, and omits any merge target outside the scope from
`merged_into_source_id`. Objects outside the scope return the same not-found
response as missing objects. The archive releases its read snapshot before
writing the response, so slow clients do not hold database connections open.
Message export is the one exception: it streams from its snapshot and holds it
until the stream finishes.

At most two scoped or agent read snapshots can be open at once, leaving database
connections available for synchronization and unrestricted reads. Additional
scoped requests wait for a slot. If a request's context ends while waiting, it
returns 503 with `source_scope_busy`.

Scoped requests support these operations:

| Operation | Route |
|---|---|
| `listMessages` | `GET /api/v1/messages` |
| `searchMessages` | `GET /api/v1/search` with default or `mode=fts` |
| `getMessage` | `GET /api/v1/messages/{id}` |
| `getConversation` | `GET /api/v1/conversations/{id}` |
| `getAttachment` | `GET /api/v1/attachments/{id}` (metadata) |
| `getMeetingRecord` | `GET /api/v1/meetings/record` |
| `suggestMeetingLinks` | `GET /api/v1/meetings/suggestions` |
| `listCLIAccounts` | `GET /api/v1/cli/accounts` |
| `getCLIStats` | `GET /api/v1/cli/stats` |
| `searchCLI` | `GET /api/v1/cli/search` |
| `getCLIMessage` | `GET /api/v1/cli/message` |
| `getCLIMessageThread` | `GET /api/v1/cli/message/thread` |
| `exportCLIMessages` | `GET /api/v1/cli/messages/export` |
| `getHealth` | `GET /api/v1/health` |

The CLI operations back `list-accounts`, `stats`, `search`, `show-message`,
`show-thread`, and `export-messages` against a remote, so a scoped caller can
run those commands. Their statistics, results, and export records cover only
authorized sources, and an export source selector outside the scope returns 404.
An export `person_id` that does not exist returns an empty export, matching one
with no messages in authorized sources. `getHealth` returns the public health
view plus the API schema version.

All other operations return 403 under a source scope, including attachment
downloads, inline MIME parts, and vector or hybrid search. Message bodies are
returned, though images that reference inline parts do not load under a scope. A
custom store that cannot provide native scoped reads returns 503. Your operation
callback still decides which supported operations to admit. Source scopes apply
only to `Handler`; they do not turn arbitrary `Store` or `QueryEngine` calls
into an authorized API.

Your application owns authentication and authorization lifetime. If your
authorization check relies on a transaction or lock to prevent concurrent access
changes, hold that transaction or lock until `ServeHTTP` returns. Returning
success from the authorization callback does not hold it open during the read,
and the archive's read snapshot does not replace your application-owned
transaction.

## Run synchronization

The package exposes the Slack and Discord importers through `SyncSlack` and
`SyncDiscord`. Other source types remain available through the standalone CLI,
and you can read their stored data through this library.

`SlackOptions` and `DiscordOptions` expose the native importer options:
conversation filters, history repair, attachment storage, progress callbacks,
and provider-specific controls. The library does not impose an application scope
allowlist or force public-channel-only collection.

You can inspect provider credentials and channels before starting a sync:

- `InspectSlack` reports the authenticated identity and granted scopes, and
  `SlackChannels` includes conversation-kind metadata.
- `InspectDiscord` verifies bot access to a guild, and `DiscordChannels`
  includes permission overwrites.

Use that information to enforce your own credential and channel selection rules
before calling an importer.

Passing a zero source ID uses normal source discovery. To continue an existing
Slack source with replacement credentials, stop the old sync, call `BindSlack`
with that source ID and the same workspace, and sync with the returned ID.
Source and message identities stay the same, and messages still count as sent by
"me" only when the source's original user sent them. If the replacement
credential belongs to a different user, the first sync rebuilds that user's
progress with a full history walk. `BindDiscord` reuses a source by guild ID.

Call `Archive.Progress` to read the last run and saved provider checkpoints
without starting a sync. Provider access and retention still determine what can
be collected; a completed run does not prove historical completeness.

In Slack, setting `ChannelIDs` scopes reply searches to each selected channel,
and selected DMs and group DMs audit their own threads. A non-nil empty slice
collects nothing, while nil keeps normal importer selection. If the credential
user can no longer list a selected channel, the importer skips it and reports it
in the summary's `UnavailableChannels`.

In Discord, the optional `PublicChannels` selection excludes private threads and
refreshes all archived-thread pages of selected parents on each run. That adds
catalog requests for large histories, while message pagination stays resumable.
If the guild no longer lists a selected parent, the importer reports a non-fatal
`unknown_channel` catalog issue. Use the normal guild configuration when you do
not want that restriction.

## Remove archived data

Call `PurgeChannel` to delete a channel and its retained threads, or
`PurgeSource` to delete a source and its retained message data. Both serialize
deletion with source synchronization and refuse a source that has a queued or
running daemon sync operation. They remove associated bodies, raw payloads,
metadata, search entries, and packed attachment mappings that no other message
references, and advance the derived-data revision so analytics caches rebuild
without the deleted rows. Deleting a channel also invalidates its saved import
coverage so an explicit full import can restore it.

Deletion does not stop a future import from collecting the same provider data.
Your application decides whether to change selection or allow reimport. You
manage attachment files, external indexes, backups, and copies already read
separately.

## Search messages by meaning

A PostgreSQL host can supply `PostgreSQL.Embeddings` with a `*embedclient.Client`, model name, and vector dimension. Build msgvault with the `pgvector` tag and install the PostgreSQL `vector` extension in a shared schema before running Setup. Run `archive.Setup` as the schema owner, then open the archive with the runtime role and the same settings. Set `Model`, `Dimension`, `DocumentPrefix`, and `QueryPrefix` to match the supplied client. Run Setup again when the model, dimension, or either prefix changes.

Call `Archive.Embed` after synchronization and on the host's schedule. New messages are indexed on the next call. Edited messages are revisited by the existing backstop, at most once every 24 hours while calls continue. The call builds or updates the message index; stop it before closing the archive. Construct the API with `Archive.NewServer` to use the index. Search with `mode=vector` for meaning, or `mode=hybrid` for words and meaning. `WithSourceScope` limits both modes to authorized sources.
