# Relationship index benchmark

This is the reference release-gate result for the version-15 relationship
index. The harness builds an exact synthetic archive, builds the Parquet cache
in a bounded daemon-child process, runs a fresh-engine cold benchmark, starts
the production daemon, and samples its RSS and spill directory every 100 ms.

Run it from the repository root:

```bash
MSGVAULT_RELATIONSHIPS_SCALE_BENCH=1 \
  scripts/benchmark-relationships-index.sh
```

The command writes exactly one JSON result to stdout and exits nonzero when a
reviewed gate is exceeded. Diagnostic output is written to stderr only after a
failed build or benchmark.

## Reference candidate

- Date: 2026-07-27
- Commit: `dad5514dde5b17c2c96abec144d37452889916f3`
- Machine: Apple M5 Max, 18 cores, 128 GiB RAM
- OS: macOS 26.5.2 (25F84), Darwin arm64
- Go: 1.26.5
- DuckDB: 1.5.4 through `duckdb-go/v2` 2.10504.0
- Archive: 2,500,000 messages, 75,000 participants, and exactly
  6,000,000 sender-plus-recipient edges

## Results

| Measurement | Gate | Measured | Result |
| --- | ---: | ---: | --- |
| Full cache build | 25 s | 13.54 s | Pass |
| Builder peak RSS | 3 GiB | 2,201,174,016 bytes | Pass |
| Cold relationships | 250 ms | 80.053 ms | Pass |
| Cold people search | 250 ms | 39.799 ms | Pass |
| Cold domain search | 250 ms | 2.662 ms | Pass |
| Source-only people | 1 s | 209.114 ms | Pass |
| Date-window relationships | 1 s | 55.521 ms | Pass |
| Filtered people, provisional | 1 s | 101.556 ms | Pass |
| Filtered relationships, provisional | 1 s | 92.014 ms | Pass |
| Interactive peak RSS growth | 1.5 GiB | 206,960 KiB | Pass |
| Settled RSS growth after 5 s | 256 MiB | 206,960 KiB | Pass |
| Peak DuckDB spill | Reported | 0 bytes | — |

The gate thresholds are unchanged from the approved design. The builder used
the production 1,536 MB, two-thread policy; the daemon used the production
512 MB, four-thread policy.

The [exact harness output](relationships-index-benchmark-dad5514d.json) is
checked in separately because it contains the complete operator tree for every
statement. Its SHA-256 is
`f5b81ae34d36e25fdc4afd96c2e359e7a457d1ef7a9f09c788359b6f025e28e4`.

Filtered people and relationships each retain two statement profiles, proving
that candidate preselection is included. Their aggregate rows-scanned totals
are 52,182,769 and 52,032,769 respectively. The artifact also records
per-dataset/operator totals; for example, filtered relationships includes
20,000,000 `identity_entry_facts` rows, 31,457,692
`identity_direct_edges` rows, and 500,000 `identity_conversation_edges` rows.

The HTTP measurements come from the first requests issued to a newly started
daemon. The opt-in Go benchmark separately creates a fresh DuckDB engine for
each measured operation and records DuckDB profiling metrics for scanned rows
and spill bytes. The daemon also reports settled `duckdb_memory()` accounting
so retained process RSS can be distinguished from live DuckDB allocations.
