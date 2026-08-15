# Web UI Reliability and Operational Clarity

Date: 2026-08-15
Status: Approved for implementation planning

## Problem

Large archives can make the initial Everything and Files listings exceed the
interactive DuckDB memory or request-time budgets. The existing listing fast
paths intend to page narrow metadata before enriching rows, but they still
carry or rescan the `analytical_entries` view in ways that can assemble
archive-wide participant lists. The performance fixture contains roughly
100,000 messages, so it does not exercise multi-million-message archives or
the high-cardinality participant data that dominates the failed queries.

Cache initialization is already observable through authenticated health, but
the web UI does not present it or retry analytical views when initialization
finishes. Users instead see a generic loading failure while an otherwise
healthy daemon prepares the cache.

The Sources workspace exposes useful operational facts too literally: raw UTC
timestamps, cron expressions, internal reason codes, and complete transport
errors compete with the source's current status and next useful action.

These are independent reliability and presentation problems. They should ship
as three reviewable changes rather than one broad web UI rewrite.

## Goals

- Make default Everything and Files listings complete within the existing
  interactive memory and request-time budgets on multi-million-message
  archives.
- Preserve exact listing, ordering, pagination, total-count, and participant
  enrichment behavior.
- Show cache preparation as normal background work, including elapsed time,
  and retry affected analytical views once the cache becomes ready.
- Present source status in human language while retaining exact diagnostic
  details behind progressive disclosure.
- Keep every change product-local and provider-neutral.

## Non-goals

- Raising the 512 MB interactive memory limit or extending ordinary request
  deadlines.
- Changing the persisted analytics-cache schema or rebuilding existing caches
  solely for these fixes.
- Optimizing cache construction before phase-level evidence identifies a
  specific bottleneck.
- Changing scheduler semantics, provider integrations, or sync error storage.
- Redesigning unrelated analytical workspaces.

## Change 1: bounded analytical listings

### Query shape

The default ungrouped listing paths will use an explicitly narrow row source
for their page and count phases. The source will project only scalar columns
needed for filtering, classification, grouping, sorting, and the returned page.
It will not use `SELECT *` across `analytical_entries`, nor project participant
list columns before the page is bounded.

Everything will:

1. Filter and classify narrow message metadata.
2. Form logical entries and select the requested page.
3. Compute the total from a separate narrow scan.
4. Resolve participant facts and owner/counterpart information only for the
   bounded page.

Files will:

1. Filter narrow message metadata and attachment metadata.
2. Sort and page without participant-list columns.
3. Compute the total from a separate narrow attachment scan.
4. Resolve participant facts only for message IDs present on the bounded page.

Participant- and domain-filtered paths retain their current semantics. Shared
helpers may move their predicates to edge-table existence checks when required
to keep those paths bounded, but the first acceptance boundary is the default
Everything and Files listings that currently fail on large archives.

### Resource policy

The existing interactive policy remains authoritative: 512 MB memory, bounded
threads and spill, and the ordinary request deadline. A query that only works
after raising those values is not considered fixed.

The implementation will avoid persisted cache changes. Narrow SQL is built
over the existing Parquet relations and in-process convenience views, so old
and newly built caches behave identically.

### Verification model

Existing fast-path-versus-legacy equivalence coverage remains the semantic
oracle. New behavior tests will generate an adversarial archive with enough
message/attachment and participant fan-out to make the old query exceed a
tight test memory limit, then assert that the real listing operations return
correct bounded pages and totals. This proves the behavior under pressure
without making CI construct a private-archive-sized fixture.

The manual large-archive benchmark will gain a Files first-page case and a
clearly documented cardinality. Benchmark timing remains diagnostic rather
than a flaky unit-test assertion.

### Alternatives rejected

- **Raise memory or timeouts.** This masks an unnecessarily large working set,
  increases contention with other daemon work, and merely moves the failure to
  a larger archive.
- **Remove exact totals.** Totals are part of the current browser and API
  contract. A slim count is cheaper than changing pagination semantics.
- **Materialize another persisted dataset.** That may eventually benefit more
  analytical operations, but it creates cache-schema and migration work that
  is unnecessary for the listing defect.
- **Use the transactional database as fallback.** The analytical cache is the
  selected authority for these workspaces; silently changing engines would
  make behavior and performance mode-dependent.

## Change 2: cache readiness in the web UI

An application-scoped readiness controller will poll authenticated health only
while cache preparation is active. It will use the existing analytics-engine
mode and operation holder (`label` and `started_at`) rather than introduce a
second source of lifecycle truth.

While preparation is active, the shell will show a calm, non-blocking status
such as “Preparing archive views — 48s.” Other workspaces remain usable.
Everything and Files will retain their current query, filters, and selection,
and register one generation-fenced retry. When health reports DuckDB ready,
each still-current affected view reloads once. Navigation, a changed query, or
component destruction cancels the pending retry.

Polling starts promptly, backs off to a bounded interval, pauses while the
document is hidden, and stops on readiness, terminal failure, or unmount. Older
servers without detailed operation health degrade to the existing explicit
unavailable state.

Cache construction duration will not be optimized speculatively. Existing
stage logs will first be reviewed and, if insufficient, receive phase timing in
this change so later performance work targets the measured bottleneck.

### Alternatives rejected

- **Block the whole shell until cache readiness.** Settings, Sources, and
  transactional detail surfaces remain useful during preparation.
- **Blindly retry failed requests on a timer.** Health provides lifecycle truth;
  repeated heavy queries during construction waste resources and obscure real
  failures.
- **Report a fabricated percentage.** The current builder has no trustworthy
  denominator across validation, export, and open phases. Elapsed time and the
  named operation are honest.

## Change 3: Sources presentation

Each source card will prioritize:

1. Source identity and current state.
2. Last successful sync and next scheduled action using relative time.
3. The available action, such as Sync now.

Exact timestamps remain available through accessible labels or detail text.
Raw cron expressions move beneath schedule details rather than serving as the
primary schedule description.

Known machine reason codes receive stable human labels, including stale
results, unscheduled sources, another active sync, and sources that cannot be
scheduled. Unknown codes display a generic explanation and retain the literal
code in diagnostic details.

Run and scheduler failures show a concise summary by default. The exact stored
error and bounded item-error list remain available in a closed `details`
section. Raw errors are not rewritten or discarded, and security-relevant
denials are not softened into success.

Status-load and sync-start failures keep their current retry and polling
semantics. This change is presentation-only and does not infer state the API did
not report.

### Alternatives rejected

- **Discard technical errors.** Exact details are necessary for diagnosis; they
  should be subordinate, not removed.
- **Teach the UI provider-specific error parsing.** Provider transport strings
  are unstable. The run status supplies the durable summary, while raw details
  remain available generically.
- **Hide stale successful runs.** Staleness is useful operational information,
  but it is a warning about age rather than a failed sync.

## Delivery sequence

1. Bounded analytical listings.
2. Cache readiness and automatic recovery.
3. Sources operational presentation.

Each change receives its own branch and pull request. Public artifacts describe
only msgvault behavior and use synthetic fixture data.
