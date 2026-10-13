# Native Events upgrade fixtures

These archives contain only synthetic identities, messages and generated test keys.
They were exported by the pre-resource-lifetime Store and daemon after a native MCP
SDK subscription, a successfully signed delivery, and a failed delivery queued for
retry. They preserve actual old schema and data, rather than rebuilding old state
with the current schema. `provenance.json` records the original commits and compressed
archive digests. Each backend has its own generated key and manifest.

The tests restore a private copy, run the current schema upgrade and configure the
Events runtime. They compare durable delivery state, including exact pending bytes,
subscription identity and encrypted signing-key revisions. Read assertions use the
fixture's captured observation time; they do not extend receipt expiry. HTTP signature
headers are generated per attempt and are not persisted in the fixture. The native
SDK regression reads the original receipt and every message page through the daemon,
then retries the pending occurrence against an independent HTTPS signature verifier.
An injected service clock observes the original fixture lifetime without changing its
expiry. A failed attempt followed by acknowledgement must retain the payload and event
ID while producing a fresh timestamp and signature.

SQLite uses a native VACUUM export. PostgreSQL uses a native pg_dump export and requires
`psql` on PATH and permission to create a disposable database on the synthetic test
server selected by `MSGVAULT_TEST_DB`. The helper restores only into its newly created
database and drops it on cleanup. It never replaces the control database's schema.
Use a PostgreSQL client that understands the dump's `\restrict` directive.

`generate_test.go.txt` is the export harness. To regenerate on a pre-resource revision,
map `internal/mcp/events_integration_test.go` to that file with a Go overlay, set
`MSGVAULT_LEGACY_FIXTURE_DIR` to a new private output directory, and run
`go test -timeout 60m -tags "fts5 sqlite_vec" -overlay <overlay.json> ./internal/mcp -run '^TestPR3LegacyFixtureProduction$' -count=1`.
For PostgreSQL also set the synthetic `MSGVAULT_TEST_DB` and `MSGVAULT_PG_DUMP` to the
native pg_dump executable. The harness resolves the Store's actual test database,
not the control database. It joins the delivery worker before export. Compress the
export, retain the generated manifest/key, and update provenance only after verifying
the restored old state.
