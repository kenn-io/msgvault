# Final review fixes

## Scope

This wave addresses the five verified final-review findings and the two
closure-review durability defects without changing the reviewed release gates.

### Shared logical-activity reduction

- The builder now evaluates `LogicalActivitySQL` once into a temporary narrow,
  tagged relation.
- Identity, domain, relationship-daily, and anchored relationship rollups all consume
  that relation.
- The relation is dropped on success and error.
- A real DuckDB build test makes every logical-activity input unavailable
  immediately after materialization, proves all four consumers still finish,
  and verifies that no temporary table remains.

### Exact post-build validation

- Validation now checks the exact ordered names and DuckDB-visible types for
  all eight identity-index datasets.
- It proves a one-to-one correspondence between cached messages and entry
  facts, uniqueness of domain-rollup keys, and the daily-row raw-count,
  event-date, modality, timestamp, and exact aggregate-total invariants.
- Real Parquet mutation tests prove each new class of corruption is rejected.
- Empty partitioned fact and edge datasets now use an
  `occurred_year=0/empty.parquet` partition so hive partition inference has the
  same `BIGINT` type as non-empty builds.

### Rollback-safe cache publication

- Full, incremental, and derived publication use one transaction journal.
- Replaced datasets are moved to private staging backups; appended shards and
  replacements are tracked and reversed in last-applied-first order.
- The prior commit marker remains intact until all data moves, fingerprinting,
  state encoding, and staged-marker preparation succeed.
- Fault-injection tests compare exact pre/post cache snapshots for pre-move,
  mid-full-move, later-incremental-move, and marker-preparation failures.
- Integration tests prove a failed publication remains readable and a retry
  neither duplicates nor exposes uncommitted messages.
- A versioned, fsynced sibling journal now survives SIGKILL independently of
  transient build staging. Recovery rolls pre-marker crashes back and
  marker-committed crashes forward before readiness, cleanup, or serve.
- Thirteen real subprocess kill/restart cases cover journal, backup, data, and
  marker phases across full, incremental, and derived publication.
- Recovery durably records a rollback-complete phase only after restoring and
  syncing the expected old marker state. A restart verifies that live state and
  resumes cleanup without depending on a preserved marker already eligible for
  deletion.
- Eleven additional real subprocess kill/restart cases cover recovery after
  marker restoration or intended absence, durable rollback, and every
  backup/marker/journal/root cleanup boundary for rollback and forward
  finalization. Each committed-cache case verifies an exact old-or-new
  snapshot, `CacheReady`, and zero transaction or staging residue.
- Regular publication files are synced through writable handles. Non-Windows
  systems retain real directory fsyncs; Windows uses a documented directory
  namespace boundary because `FlushFileBuffers` rejects directory handles,
  matching the existing config-publication contract.

### Exact decay validation

- Validation recomputes all three decayed relationship components from daily
  rows with the production anchor/clamp/day formula and explicit absolute and
  relative tolerances.
- NULL, nonfinite, negative, and invalid-mask components fail closed; all
  decomposition comparisons are NULL-safe.
- Real Parquet mutations prove NULL and incorrect decay values, NaN, negative
  units, NULL counts, and invalid masks are rejected.

### Machine-readable benchmark evidence

- The cold benchmark covers landing relationships, unfiltered people and
  domains, broad source-only people, date-window relationships, and the
  existing selective people and relationships workloads.
- Every operation records every statement's operator tree plus rows scanned,
  rows returned, spill bytes, and per-dataset/operator totals in a versioned
  JSON profile embedded in the shell harness's single JSON result.
- The profiling artifact lives outside DuckDB's temp directory and a regression
  test proves profile bytes cannot be misreported as spill.
- The 250 ms, 1 s, build-time, peak-builder-RSS, interactive-RSS, settled-RSS,
  and spill-reporting policies are unchanged.
- The retained production run also exposed and closed three representative
  query-shape costs without relaxing those gates:
  - source-only people requests aggregate exact nested per-source rollups;
  - pure UTC-midnight relationship date windows aggregate the flat exact
    `relationship_daily` dataset;
  - participant/domain edge filters first materialize at most 10,000 exact
    fact IDs, with saturation falling back to the generic logical reduction.
- The attempted nested daily-list representation was rejected after a real
  1.5 GiB builder OOM. The accepted flat daily dataset builds in a spillable
  grouped operation and supplies compact anchored rollups.

### Authoritative builder policy

- The design and plan consistently specify the measured production builder
  policy: `memory_limit='1536MB'`, at most two threads, and an 8 GiB staging
  spill limit.
- They retain the evidence and review requirement before changing that policy.

## TDD evidence

The new tests were observed failing before their production changes:

- shared reuse failed when inputs were removed after the first reduction;
- schema, missing-fact, duplicate-domain, and daily-decomposition corruptions
  were accepted by the old validator;
- full and incremental snapshot tests showed partial publication after injected
  move/state failures;
- the recovery subprocess exited without reaching a durable marker-restored
  checkpoint, reproducing the missing rollback-complete transition;
- benchmark spill accounting returned 300 bytes when only 200 bytes were
  genuine temp data.

Additional equivalence tests compare source-only, date-window, and bounded
edge-candidate paths with the generic logical reduction. A scale-only failure
also demonstrated that DuckDB allocator-flush controls did not reduce retained
RSS; those experimental controls were fully reverted before the accepted
query-shape fix.

## Final verification

The final source state passed:

- `gofmt` and `git diff --check`;
- `go vet -tags "fts5 sqlite_vec" ./...`;
- `make test`;
- `make lint-ci`;
- `make docs-check`;
- `shellcheck scripts/benchmark-relationships-index.sh`;
- `bash -n scripts/benchmark-relationships-index.sh`.
- Windows durability helper and test binaries cross-compiled for both amd64
  and arm64. The full command test binary cannot be cross-linked on this host
  because its existing DuckDB/sqlite-vec CGO chain requires target SQLite
  headers and libraries.

The clean production-scale gate passed at 2,500,000 messages, 75,000
participants, and 6,000,000 participant edges for implementation commit
`dad5514dde5b17c2c96abec144d37452889916f3`:

- full cache build: 13.54 s, 2,201,174,016-byte builder peak RSS;
- cold relationships: 80.053 ms;
- source-only people: 209.114 ms;
- date-window relationships: 55.521 ms;
- selective people/relationships: 101.556/92.014 ms;
- peak/settled daemon RSS growth: 206,960/206,960 KiB;
- peak DuckDB spill: 0 bytes.

Machine-readable profiling reported two statements and 52,182,769 aggregate
scanned rows for selective people, and two statements and 52,032,769 rows for
selective relationships. Unlike the superseded evidence, these totals include
candidate preselection. The exact 134,079-byte artifact retains every operator
tree and per-dataset/operator total. Its SHA-256 is
`f5b81ae34d36e25fdc4afd96c2e359e7a457d1ef7a9f09c788359b6f025e28e4`.

## Remaining concerns

None known in the implementation. Diagnostic scale runs that failed because of
the rejected nested-list shape or unchanged allocator high-water behavior are
not retained as release evidence, and no threshold was weakened.
