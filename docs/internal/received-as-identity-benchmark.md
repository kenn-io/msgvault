---
last_edited: "2026-10-02"
---

# Received-account benchmark

The 150k-message benchmark passed on 2026-10-02. Exact-account pages use the
scalar address index; masked groups remain one selector and one catalog row.
The received projection revisited one message when a new mask was confirmed.
The surrounding identity confirmation still spends most of its time in main's
existing ownership refresh. The related masked-inventory branch owns that
optimization; combine the hooks described in Kata before claiming fast
end-to-end identity confirmation.

## Method

The isolated Linux amd64 runner had two CPUs and 8 GB of RAM. Go reported an
AMD EPYC 9554P. Commands used `GOFLAGS=-p=2`, `GOMAXPROCS=2`,
`-tags "fts5 sqlite_vec"`, `-p 2`, and `-parallel 2`.

The deterministic fixture has one Gmail source, 1001 confirmed masks, the
source address and one work address. It precomputes scalar facts and compact
header evidence for 150,000 messages. Source-only counts match all rows; exact
account, received-address and masked-group counts each match 15,000 rows.
Messages share a timestamp, subject and snippet. Lists return 50 rows sorted
by date descending. Keyword counts use the production metadata-only fast search.
Parquet measurements use a real cache publication and a standalone DuckDB
engine. The catalog verifies four buckets and an additive total of 150,000.

Query, catalog and repair cases use five timed iterations after Go's initial
benchmark calibration. Confirmation uses one timed iteration because its
existing whole-source ownership refresh is expensive. These are warm query
measurements, not cold disk-cache measurements. Go allocation counters exclude
DuckDB's native allocations. There is no runner-speed correctness assertion.
Source-only and address predicates have different selectivity; this is not an
equal-selectivity comparison with separately synced accounts. Cold-cache runs
and that comparison remain unmeasured.

## Measured results

| Engine | Operation | Matches / rows | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|---:|
| SQLite | `source` | 150000 | 81,151,236 | 1,534 | 34 |
| SQLite | `page: source` | 50 | 640,152,379 | 155,592 | 2,090 |
| SQLite | `account:work@example.org` | 15000 | 44,179,744 | 1,942 | 40 |
| SQLite | `page: account:work@example.org` | 50 | 1,791,160 | 156,822 | 2,098 |
| SQLite | `received:work@example.org` | 15000 | 37,587,706 | 1,865 | 40 |
| SQLite | `page: received:work@example.org` | 50 | 1,684,069 | 156,214 | 2,096 |
| SQLite | `account:fastmail-masked:inbox@example.net` | 15000 | 640,193,652 | 2,635 | 41 |
| SQLite | `page: account:fastmail-masked:inbox@example.net` | 50 | 380,554,165 | 156,472 | 2,097 |
| SQLite | `report` | 150000 | 480,117,461 | 14,421,038 | 750,135 |
| SQLite | `report_account:work@example.org` | 15000 | 82,354,159 | 1,457,686 | 75,133 |
| SQLite | `virtual_account_catalog` | 4 buckets | 479,011,519 | 4,008 | 67 |
| SQLite | `confirm new mask (whole operation)` | 1 revisited | 76,721,403,870 | 54,776 | 515 |
| Parquet | `source` | 150000 | 17,121,980 | 63,059 | 492 |
| Parquet | `page: source` | 50 | 38,686,705 | 196,916 | 1,824 |
| Parquet | `account:work@example.org` | 15000 | 16,983,156 | 68,712 | 504 |
| Parquet | `page: account:work@example.org` | 50 | 41,328,030 | 202,612 | 1,833 |
| Parquet | `received:work@example.org` | 15000 | 5,807,876 | 63,481 | 503 |
| Parquet | `page: received:work@example.org` | 50 | 39,944,714 | 194,052 | 1,831 |
| Parquet | `account:fastmail-masked:inbox@example.net` | 15000 | 39,620,084 | 63,942 | 503 |
| Parquet | `page: account:fastmail-masked:inbox@example.net` | 50 | 74,271,773 | 197,625 | 1,832 |

The 1000-row compact-evidence backfill took **493,527,496 ns/op** (about
2026 messages/second), **10,250,393 B/op**, and **271,243 allocs/op**. This
measures the real 100-row repair pages, cursor commits and projection updates.
It excludes initial MIME decompression. The resumability/replay tests separately
verify cancellation and unchanged derived revisions on a completed replay.

SQLite query-plan tests verify the source/address message index, the indexed
mention lookup and the partial source/address identity index used by group
membership. The catalog first aggregates messages by source/address, then
resolves group membership for each distinct address.

## Reproduce

```bash
export GOFLAGS=-p=2 GOMAXPROCS=2
go test -tags "fts5 sqlite_vec" -p 2 -parallel 2 ./internal/store -run '^$' \
  -bench 'BenchmarkAccountScopes150k/(source|list|account:.*|received:.*|report.*|virtual_account_catalog)$' \
  -benchmem -benchtime=5x -count=1
go test -tags "fts5 sqlite_vec" -p 2 -parallel 2 ./internal/store -run '^$' \
  -bench 'BenchmarkAccountScopes150k/confirm_new_mask$' -benchmem -benchtime=1x -count=1
go test -tags "fts5 sqlite_vec" -p 2 -parallel 2 ./internal/store -run '^$' \
  -bench 'BenchmarkAccountBackfill1000$' -benchmem -benchtime=5x -count=1
go test -tags "fts5 sqlite_vec" -p 2 -parallel 2 ./cmd/msgvault/cmd -run '^$' \
  -bench 'BenchmarkAccountParquet150k' -benchmem -benchtime=5x -count=1
```

See the [design record](received-as-identity-design.md) for the attribution
contract and the remaining calendar mapping transaction limitation. Kata
`msgvault#xt30` and `msgvault#8zrn` record the provider/ownership integration
hooks. No provider inventories or personal archive data were used.
