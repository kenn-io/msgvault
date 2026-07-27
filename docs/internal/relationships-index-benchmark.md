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
- Commit: `74ab0e6dc5712a4da096daf04ca813222e1aceb0`
- Machine: Apple M5 Max, 18 cores, 128 GiB RAM
- OS: macOS 26.5.2 (25F84), Darwin arm64
- Go: 1.26.5
- DuckDB: 1.5.4 through `duckdb-go/v2` 2.10504.0
- Archive: 2,500,000 messages, 75,000 participants, and exactly
  6,000,000 sender-plus-recipient edges

## Results

| Measurement | Gate | Measured | Result |
| --- | ---: | ---: | --- |
| Full cache build | 25 s | 14.18 s | Pass |
| Builder peak RSS | 3 GiB | 2,139,684,864 bytes | Pass |
| Cold relationships | 250 ms | 57.096 ms | Pass |
| Cold people search | 250 ms | 39.426 ms | Pass |
| Cold domain search | 250 ms | 3.393 ms | Pass |
| Filtered people, provisional | 1 s | 100.901 ms | Pass |
| Filtered relationships, provisional | 1 s | 88.788 ms | Pass |
| Interactive peak RSS growth | 1.5 GiB | 240,608 KiB | Pass |
| Settled RSS growth after 5 s | 256 MiB | 240,608 KiB | Pass |
| Peak DuckDB spill | Reported | 0 bytes | — |

The gate thresholds are unchanged from the approved design. The builder used
the production 1,536 MB, two-thread policy; the daemon used the production
512 MB, four-thread policy.

Exact harness output:

```json
{"status":"pass","messages":2500000,"participants":75000,"participant_edges":6000000,"build_seconds":14.18,"build_peak_rss_bytes":2139684864,"relationships_ms":57.096,"relationships_rss_kb":128032,"people_ms":39.426,"people_rss_kb":165952,"domains_ms":3.393,"domains_rss_kb":166112,"filtered_people_ms":100.901,"filtered_people_rss_kb":298672,"filtered_relationships_ms":88.788,"filtered_relationships_rss_kb":313216,"baseline_rss_kb":72608,"peak_rss_kb":313216,"peak_rss_delta_kb":240608,"settled_rss_kb":313216,"settled_rss_delta_kb":240608,"peak_spill_bytes":0}
```

The HTTP measurements come from the first requests issued to a newly started
daemon. The opt-in Go benchmark separately creates a fresh DuckDB engine for
each measured operation and records DuckDB profiling metrics for scanned rows
and spill bytes.
