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
    URL: databaseURL,
    Schema: "message_archive",
    MaxOpenConnections: 8,
}
if err := archive.Setup(ctx, options); err != nil {
    return err
}
messages, err := archive.Open(ctx, options)
```

An empty Schema preserves the connection's search path. Setup requires schema
ownership. It creates a missing schema, which needs `CREATE` on the database;
the owner of a schema created in advance does not need that privilege. Open can use a separate role granted `USAGE`
on the schema, `SELECT`, `INSERT`, `UPDATE`, and `DELETE` on its tables, and
`USAGE`, `SELECT` on its sequences. A missing or incompatible schema fails
without trying to upgrade it. Serialize setup with other upgrades.

Each active source retains a connection for its session advisory lock. Leave
capacity for queries and writes when sizing the pool; transaction poolers do not
preserve that lock. The example pool size is illustrative, not a concurrency
limit. Database connections may contain credentials and must not be logged.

Normal builds retain the native database backends. PostgreSQL-only programs can
also build without CGO; that optional profile has no SQLite or DuckDB driver.

## Serve the API

`archive.NewServer` exposes the existing msgvault API with its full server
options. Supply an archive's `Store()` and `QueryEngine()`, plus any additional
services the application uses. No listener starts until the caller starts one.

```go
api := archive.NewServer(archive.ServerOptions{
    Store: messages.Store(),
    Engine: messages.QueryEngine(),
})
defer api.Shutdown(ctx)
handler := api.Handler(authorize)
```

`Handler` registers the same operations as the daemon and calls the supplied
authorization function with each operation's ID, method, and path. The caller
chooses which operations to expose. A nil callback adds no authorization.
Applications own authentication, browser origin checks, and CSRF policy. An
admitted operation does not also require daemon credentials; `/api/session`
reports its authentication mode as `caller`. Wrap or
mount it as needed to control HTTP metadata routes too. Authorized operations
then pass through the configured `OperationGate`, so rejected requests never
wait on archive work. The handler also applies `RequestTimeout`, request IDs,
panic recovery, and default `Cache-Control: no-store` headers. `Router()` is also
available when the daemon's transport middleware is wanted.

The existing `pkg/client` works with this handler. Availability of analytics,
attachments, scheduling, and other services follows the supplied server options
and storage backend. The library imposes no separate feature allowlist.
Stop requests and call `Shutdown` before closing the archive.

`Archive.Store()` also exposes the underlying storage operations directly for
Go callers, without requiring HTTP.

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
The source and message identities remain stable; principal-dependent progress
is rebuilt. `BindDiscord` reuses a source by guild ID.

`Archive.Progress` returns the last run and saved provider checkpoints without
starting a run. Provider access and retention still determine what can be
collected; a completed run does not establish historical completeness.

Exact Slack `ChannelIDs` selection scopes reply searches to each channel. A
non-nil empty selection collects nothing; nil keeps normal importer selection.
Discord's optional `PublicChannels` selection excludes private threads and
refreshes all archived-thread pages of selected parents on each run. This adds
catalog requests for large histories, while message pagination remains resumable.
Use the normal guild configuration when that restriction is not wanted.

## Remove archived data

`PurgeChannel` removes a channel and its retained threads. `PurgeSource` removes
a source and its retained message data. Both serialize deletion with source
synchronization and remove associated bodies, raw payloads, metadata, search
entries, and packed attachment mappings no other message references. Channel deletion invalidates its saved import coverage so an explicit
full import can restore it.

Deletion does not prevent a future import from collecting the same provider data.
Callers decide whether to change selection or allow reimport. Attachment files,
external indexes, backups, and copies already read are managed separately.
