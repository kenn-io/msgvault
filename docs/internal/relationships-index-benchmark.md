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
- Commit: `ebf7725701dbb8b90f587b39450fd9e85a5c5421`
- Machine: Apple M5 Max, 18 cores, 128 GiB RAM
- OS: macOS 26.5.2 (25F84), Darwin arm64
- Go: 1.26.5
- DuckDB: 1.5.4 through `duckdb-go/v2` 2.10504.0
- Archive: 2,500,000 messages, 75,000 participants, and exactly
  6,000,000 sender-plus-recipient edges

## Results

| Measurement | Gate | Measured | Result |
| --- | ---: | ---: | --- |
| Full cache build | 25 s | 15.14 s | Pass |
| Builder peak RSS | 3 GiB | 2,227,453,952 bytes | Pass |
| Cold relationships | 250 ms | 63.839 ms | Pass |
| Cold people search | 250 ms | 38.206 ms | Pass |
| Cold domain search | 250 ms | 2.952 ms | Pass |
| Source-only people | 1 s | 212.120 ms | Pass |
| Date-window relationships | 1 s | 56.389 ms | Pass |
| Filtered people, provisional | 1 s | 101.698 ms | Pass |
| Filtered relationships, provisional | 1 s | 94.062 ms | Pass |
| Interactive peak RSS growth | 1.5 GiB | 206,848 KiB | Pass |
| Settled RSS growth after 5 s | 256 MiB | 206,896 KiB | Pass |
| Peak DuckDB spill | Reported | 0 bytes | — |

The gate thresholds are unchanged from the approved design. The builder used
the production 1,536 MB, two-thread policy; the daemon used the production
512 MB, four-thread policy.

Exact harness output:

```json
{"status":"pass","messages":2500000,"participants":75000,"participant_edges":6000000,"build_seconds":15.14,"build_peak_rss_bytes":2227453952,"relationships_ms":63.839,"relationships_rss_kb":132864,"people_ms":38.206,"people_rss_kb":169104,"domains_ms":2.952,"domains_rss_kb":169168,"source_only_people_ms":212.120,"source_only_people_rss_kb":248976,"date_window_relationships_ms":56.389,"date_window_relationships_rss_kb":250528,"filtered_people_ms":101.698,"filtered_people_rss_kb":277648,"filtered_relationships_ms":94.062,"filtered_relationships_rss_kb":278352,"baseline_rss_kb":71840,"peak_rss_kb":278688,"peak_rss_delta_kb":206848,"settled_rss_kb":278736,"settled_rss_delta_kb":206896,"peak_spill_bytes":0,"query_profile":{"version":1,"operations":[{"name":"relationships","rows_scanned":11166155,"spill_bytes":0,"rows_returned":100},{"name":"people-search","rows_scanned":299850,"spill_bytes":0,"rows_returned":100},{"name":"domain-search","rows_scanned":100,"spill_bytes":0,"rows_returned":100},{"name":"source-only-people","rows_scanned":299850,"spill_bytes":0,"rows_returned":100},{"name":"date-window-relationships","rows_scanned":11016309,"spill_bytes":0,"rows_returned":100},{"name":"filtered-people","rows_scanned":17912705,"spill_bytes":0,"rows_returned":15},{"name":"filtered-relationships","rows_scanned":17762705,"spill_bytes":0,"rows_returned":15}]},"duckdb_memory":{"columns":["tag","memory_usage_bytes","temporary_storage_bytes"],"rows":[["ALLOCATOR",0,0],["ART_INDEX",0,0],["BASE_TABLE",0,0],["COLUMN_DATA",0,0],["CSV_READER",0,0],["EXTENSION",0,0],["EXTERNAL_FILE_CACHE",3973120,0],["HASH_TABLE",0,0],["IN_MEMORY_TABLE",0,0],["METADATA",0,0],["OBJECT_CACHE",14710,0],["ORDER_BY",0,0],["OVERFLOW_STRINGS",0,0],["PARQUET_READER",0,0],["TRANSACTION",0,0],["WINDOW",0,0]],"row_count":16}}
```

The HTTP measurements come from the first requests issued to a newly started
daemon. The opt-in Go benchmark separately creates a fresh DuckDB engine for
each measured operation and records DuckDB profiling metrics for scanned rows
and spill bytes. The daemon also reports settled `duckdb_memory()` accounting
so retained process RSS can be distinguished from live DuckDB allocations.
