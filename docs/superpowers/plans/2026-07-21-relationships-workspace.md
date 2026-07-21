# Relationships Workspace Implementation Plan (Plan 3 of 3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the three-pane Relationships workspace as the web UI's landing view (per `docs/superpowers/specs/2026-07-20-web-ui-relationships-design.md`), merge People/Domains into it as facets, add the identity-link UI, decompose AppShell, fix the accumulated carry-over bugs, and land the kit-ui/theme visual pass.

**Architecture:** A new `RelationshipsWorkspace` hub (left ranked list / center person header + unified timeline / right reading pane) driven by a dedicated state controller over the Plan 2 endpoints (`listRelationships`, `getRelationshipTimeline`, `linkIdentityParticipants`, `unlinkIdentityParticipants`) plus the existing people/domains search + detail endpoints. URL state gains `workspace: 'relationships'` with a facet field; old `people`/`domains` states normalize to it. AppShell sheds the Everything data-loading into an extracted controller module.

**Tech Stack:** Svelte 5 runes, @kenn-io/kit-ui, openapi-fetch (generated `schema.d.ts` already has all four new operations — no codegen needed), vitest + @testing-library/svelte (colocated), Playwright.

## Global Constraints

- All web work under `web/`; run with `bun`. Quality gates per task: `bun run check` (svelte-check, 0 errors), `bun run test` (vitest), `bun run lint` if present.
- Component tests use the established pattern: real `createAPIClient(fetchFn)` with a `vi.fn` fetch routed on `pathname` returning `Response.json(...)` bodies (see `PeopleWorkspace.test.ts:1-30`). No MSW, no client mocking.
- Svelte 5 runes only (`$state`/`$derived`/`$effect`/`untrack`); no stores for new code. Prop-drilling is the established pattern (no context) — follow it.
- Spec bindings: three-pane layout regions exactly as the spec diagram; chat bursts render as single "N messages in <conversation>" rows; **nothing in the workspace is modal; no scrim, ever**; `j`/`k` act on the focused pane; Esc walks back one layer; below the three-pane breakpoint the reading pane stacks over the timeline (Esc returns); below the two-pane breakpoint the list becomes a slide-in drawer; virtualization via kit-ui `virtualSlice`; cursor pagination, **no unbounded walk-to-end**; old `workspace=people|domains` URLs normalize to Relationships facets; Relationships is the default landing, falling back to Everything when the analytical engine is unavailable; degraded states are named, never silent.
- Identity-link contract: `200 {identity_revision, cache_state: "ready"|"stale"}`; `stale` → named `identity_cache_stale` UI state with rebuild guidance; `409 already_linked` message surfaced; retry safe.
- Never name private downstream projects; no real PII in fixtures (synthetic names only).
- Commit after every task; hooks must pass; never `--no-verify`; imperative subject ≤72 chars. Any Go change: `go fmt ./...`, `go vet ./...`, tests with `-tags "fts5 sqlite_vec"`.

---

### Task 1: Carry-over bug fixes (paging wedge, sortNotice, FileViewer fallback, dead saved-view field)

**Files:**
- Modify: `web/src/lib/components/people/PeopleWorkspace.svelte:228-234`
- Modify: `web/src/lib/components/people/DomainWorkspace.svelte:217-223`
- Modify: `web/src/lib/components/shell/AppShell.svelte` (sortNotice lifecycle, ~909/1504 region and the End-paging notice)
- Modify: `web/src/lib/components/files/FileViewer.svelte:150,260,265,280`
- Modify: `web/src/lib/components/saved-views/SavedViewsWorkspace.svelte:68,176`
- Tests: colocated `.test.ts` files for each

**Interfaces:** no new interfaces — behavior fixes only. These land first so the bugs don't survive into code Task 4 copies from.

- [ ] **Step 1: Fix the loadMore wedge (TDD)**

Failing test first (PeopleWorkspace.test.ts): render with a fetch mock whose detail/timeline responses are delayed so `identityAuthority` is still `undefined`; click load-more; then let detail resolve; click load-more again and assert the second click issues a timeline request (today it never does — the flag wedges). Then fix both copies by resetting the flag on the early return:

```ts
timelineLoadingMore = true;
if (!identityAuthority) {
  timelineLoadingMore = false;
  return;
}
```

(Identical two-line fix in `PeopleWorkspace.svelte` `loadMoreTimeline` and `DomainWorkspace.svelte` `loadMore`.)

- [ ] **Step 2: sortNotice lifecycle fixes (TDD)**

Two behaviors, tests in `AppShell.test.ts`:
1. The Files workspace must not display a stale Everything sortNotice: clear `sortNotice` (set to its neutral/default value) whenever `exploreState.current.workspace` changes — add it to the existing workspace-change handling (find where workspace commits happen in AppShell; the `commitWorkspace` wrapper at ~AppShell.svelte:108-146 is the chokepoint).
2. The "End paused loading…" pause message must reset to neutral when a later End press exhausts the cursor (walk reaches the true end): in `loadThroughEnd`, when the walk completes because `nextCursor` is gone (not because the page cap hit), reset the notice.

- [ ] **Step 3: FileViewer filename fallback (TDD)**

`FileViewer.svelte` applies a filename fallback only to the modal heading; the download action and labels render blank when the authoritative filename is missing. Extract one `displayFilename` derived (fallback chain: authoritative filename → `file.filename` → `attachment-<id>`) and use it in all four sites (~150, 260, 265, 280). Test: file fact with empty filename renders the fallback in heading, download label, and download attribute.

- [ ] **Step 4: Remove the dead saved-view inspector_pinned round-trip**

`SavedViewsWorkspace.svelte:68` stops writing `inspector_pinned` into the create payload; `:176` stops reading it back (drop the field from the patch — `normalize()` force-overrides it anyway). Keep the server-side OpenAPI field untouched (compat: old saved views still deserialize; the client just ignores the field). Update any fixtures in `SavedViewsWorkspace`/`AppShell` tests that assert on it.

- [ ] **Step 5: Verify and commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "fix(web): paging wedge, sortNotice lifecycle, file viewer fallback"
```

---

### Task 2: URL state — `relationships` workspace, facet, normalization, landing default

**Files:**
- Modify: `web/src/lib/explore/models.ts` (ExploreURLState fields)
- Modify: `web/src/lib/explore/state.svelte.ts` (defaults, `normalize()`, workspace whitelist ~212-217)
- Test: `web/src/lib/explore/state.svelte.test.ts` (or the file's existing test sibling — check `ls web/src/lib/explore/*.test.ts`)

**Interfaces (produces):**
```ts
// ExploreURLState gains:
relationshipFacet: 'people' | 'domains';        // default 'people'
relationshipTarget: string | null;              // 'cluster:<canonicalID>' | 'domain:<domain>' | null
relationshipShowAll: boolean;                   // default false — lifts the reciprocity gate
relationshipFiles: boolean;                     // default false — center-pane Files N toggle
// workspace union gains 'relationships'; DEFAULT workspace becomes 'relationships'
```
Consumed by Tasks 3-6. `analysisTarget`/`selectedIdentifier` stay (Everything's person-timeline path still uses them) — the hub uses the new fields only.

- [ ] **Step 1: Failing tests**

In the explore state test file:
```ts
it('normalizes legacy people/domains workspaces to relationships facets', () => {
  // workspace 'people' + analysisTarget 'person:12' → workspace 'relationships',
  //   relationshipFacet 'people', relationshipTarget null (person IDs are participant
  //   IDs, which map 1:1 onto cluster member IDs — carry it: relationshipTarget 'cluster:12')
  // workspace 'domains' + analysisTarget 'domain:example.com' → facet 'domains',
  //   relationshipTarget 'domain:example.com'
});
it('defaults workspace to relationships', () => { /* parse '{}' → workspace 'relationships' */ });
it('round-trips relationship fields through serialize/parse', () => { ... });
it('rejects invalid facet/target shapes', () => { /* facet 'x' → 'people'; target 'garbage' → null */ });
```

- [ ] **Step 2: Implement**

In `normalize()` (the single validation chokepoint):
- Add the three field validations (facet whitelist, target regex `^cluster:\d+$` or `^domain:[^\s]+$`, booleans).
- Legacy mapping BEFORE workspace whitelisting: `people` → `{workspace:'relationships', relationshipFacet:'people', relationshipTarget: analysisTarget?.startsWith('person:') ? 'cluster:'+id : null}`; `domains` analogous with `domain:` targets.
- Change the fallback/default workspace from `'everything'` to `'relationships'` (both `defaultExploreURLState` and the whitelist's else-branch).
- `RESTORATION_INVALIDATING_FIELDS`: add `relationshipFacet`, `relationshipTarget` (target changes invalidate row restoration), NOT the toggles.

- [ ] **Step 3: Fix the fallout**

`bun run test` will surface every test that assumed `'everything'` is the default workspace (AppShell.test.ts fixtures etc.). Update fixtures to request `workspace: 'everything'` explicitly where the test is about Everything. Do not weaken assertions.

- [ ] **Step 4: Verify and commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "feat(web): relationships URL state with legacy people/domains normalization"
```

---

### Task 3: Relationships data controller

**Files:**
- Create: `web/src/lib/relationships/controller.svelte.ts`
- Create: `web/src/lib/relationships/controller.svelte.test.ts`
- Create: `web/src/lib/relationships/models.ts`

**Interfaces (produces):**
```ts
// models.ts — re-export/narrow the generated types:
import type { components } from '../api/generated/schema';
export type RelationshipRow = components['schemas']['RelationshipRow'];
export type RelationshipSignals = components['schemas']['RelationshipSignals'];
export type RelationshipTimelineRow = components['schemas']['TimelineRow'];
export type RelationshipFacet = 'people' | 'domains';

// controller.svelte.ts
export class RelationshipsController {
  constructor(client: APIClient, timezone: () => string) {} // timezone: () => Intl.DateTimeFormat().resolvedOptions().timeZone
  // Left list state
  facet: RelationshipFacet; query: string; showAll: boolean;
  listRows: RelationshipRow[] | PersonSummary[] | DomainSummary[];  // ranked | searched
  listLoading: boolean; listError: string | null;
  degraded: 'cache_unavailable' | null;      // 503 from listRelationships → named degraded state
  // Center state
  target: string | null;                      // 'cluster:<id>' | 'domain:<domain>'
  detail: PersonSummary | DomainSummary | null;
  timelineRows: RelationshipTimelineRow[]; timelineCursor: string | null;
  timelineLoading: boolean; timelineLoadingMore: boolean; timelineError: string | null;
  canonicalID: number | null; identityRevision: number | null;
  // Actions
  loadList(predicate: ExplorePredicate): Promise<void>;
  openTarget(target: string, predicate: ExplorePredicate): Promise<void>;  // detail + first timeline page
  loadMoreTimeline(): Promise<void>;          // cursor pagination; 409 cursor_invalidated → restart from page 1
  linkParticipants(a: number, b: number): Promise<LinkOutcome>;
  unlinkParticipants(a: number, b: number): Promise<LinkOutcome>;
}
export type LinkOutcome =
  | { ok: true; identityRevision: number; cacheState: 'ready' | 'stale' }
  | { ok: false; code: 'already_linked' | 'invalid' | 'error'; message: string };
```

Behavior contracts (each is a test):
- Ranked mode: empty `query` on the people facet → `POST /api/v1/relationships {show_all, limit: 200, filters}`; rows are `RelationshipRow`s ordered as returned (server sorts).
- Search mode: non-empty `query` → the EXISTING `POST /api/v1/people/search` (or `/domains/search`) with `identity_query` — "search finds any person, ranked or not".
- Domains facet always uses `POST /api/v1/domains/search` (activity-ranked, no gate — spec).
- 503 with the cache-unavailable body → `degraded = 'cache_unavailable'`, no throw.
- `openTarget('cluster:12', …)` → `GET /api/v1/people/12` for the header detail AND `POST /api/v1/relationships/12/timeline {timezone, filters, limit}`; stores `canonical_id` + `identity_revision` from the response. `openTarget('domain:x.com', …)` → existing domain detail + domain timeline endpoints (domains have no cluster timeline).
- `loadMoreTimeline`: passes `cursor`; on 409 (`cursor_invalidated`) clears rows + cursor and reloads page 1 (assert exactly one restart request); the wedge-bug shape from Task 1 must not be reproducible (flag reset on EVERY early return — write the test).
- `linkParticipants`: maps 200 → ok with cacheState; 409 body `already_linked` → `{ok:false, code:'already_linked'}`; 400 → `'invalid'`; else `'error'`. After `ok` with a changed `identityRevision`, the controller re-runs `openTarget` (cluster membership changed).
- Generation guard: a stale `openTarget` response arriving after a newer call is discarded (follow the `detailGeneration` counter pattern from `PeopleWorkspace.svelte`).
- AbortController per load; `destroy()` aborts all.

- [ ] **Step 1: Write the failing controller tests** (fetch-mock pattern, one `it` per contract above)
- [ ] **Step 2: Implement the controller** (port the proven mechanics — generation counters, seenCursors cycle guard, `analyticalAuthority` — from `PeopleWorkspace.svelte`, as functions of this class rather than component-locals)
- [ ] **Step 3: Verify and commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "feat(web): relationships data controller"
```

---

### Task 4: RelationshipsWorkspace — three-pane hub

**Files:**
- Create: `web/src/lib/components/relationships/RelationshipsWorkspace.svelte`
- Create: `web/src/lib/components/relationships/RelationshipList.svelte` (left pane)
- Create: `web/src/lib/components/relationships/RelationshipTimeline.svelte` (center timeline)
- Create: `web/src/lib/components/relationships/RelationshipHeader.svelte` (center header)
- Create: colocated `.test.ts` for each
- Reuse (do not fork): `Inspector.svelte` (right pane, `InspectorSelection {kind:'entry'}`), `FilesWorkspace.svelte` (Files N toggle, via its existing `identityScope`/`expectedAuthority` props), `ContentFrame` (via Inspector), kit-ui `virtualSlice`, `SearchInput`, `Button`, `SegmentedControl`.

**Interfaces:**
- Consumes: `RelationshipsController` (Task 3), URL fields (Task 2), `APIClient`.
- Produces (props contract AppShell wires in Task 6):
```ts
interface Props {
  client: APIClient;
  controller: RelationshipsController;
  facet: RelationshipFacet; target: string | null; showAll: boolean; filesOpen: boolean;
  predicate: ExplorePredicate;
  onFacetChange(f: RelationshipFacet): void;   // → commitNavigation patch
  onTargetChange(t: string | null): void;
  onShowAllChange(v: boolean): void;
  onFilesToggle(v: boolean): void;
}
```

Layout (binding, from the spec diagram): left `aside` (relationship search, facet toggle People|Domains, ranked list, "show all senders" control) / center `section` (header: display name, identity chips, counts, `Link identity` and `Files N` actions; below: unified timeline with month headers OR the files table when `filesOpen`) / right reading pane rendering the selected timeline row via `Inspector` with `{kind:'entry'}`. **No modal, no scrim.** Row kinds: `email`/`event`/`meeting` rows render one item; `chat_burst` rows render "N messages in <conversation title>" and on selection open the conversation window bounded to the burst's local day (the conversation endpoint's `start`/`end` params — compute the UTC bounds of the burst's local day from `first_at`/`occurred_at`).

Keyboard (binding): `j`/`k` move within the focused pane (list or timeline — two `role="grid"`/listbox regions with their own focus); Enter opens (list → target; timeline → reading pane); Esc walks back one layer (reading pane → timeline → list → nothing), consistent with `handleEscape` in AppShell. Reuse the app's shortcut registration only through props/events — the hub handles its own keys locally via `onkeydown` on its panes (matching how `EverythingTable` does it — read it first).

Responsive (binding): CSS container/media queries — below ~1100px the reading pane stacks over the timeline (Esc returns to timeline); below ~720px the left list becomes a slide-in drawer (still not modal — a `position: absolute` panel with focus trap via existing patterns, no kit-ui Modal/DetailDrawer, no scrim; a translucent-free backdrop is NOT added).

Degraded states (binding): `controller.degraded === 'cache_unavailable'` → named full-pane state: "Relationship ranking needs the analytical cache/engine" with the standard rebuild guidance and an "Open Everything" button (calls the workspace-change callback). Timeline 409-restart shows a one-line notice ("Timeline restarted: the archive changed"). `identity_cache_stale` handled in Task 5.

Month headers: group timeline rows by `occurred_at` month (user's local timezone — same `timezone()` the controller sends); virtualized list must account for header rows (follow `PersonTimeline.svelte`'s existing virtualization with `virtualSlice` — read it and reuse its row-height approach).

- [ ] **Step 1: RelationshipList TDD** — render ranked rows (label, modality badges from `signals.modalities`, last-interaction date, score-proportional activity indicator), facet toggle fires `onFacetChange`, show-all checkbox fires `onShowAllChange`, typing in search switches to search results, `j`/`k`/Enter keyboard, degraded state renders the named message.
- [ ] **Step 2: RelationshipHeader TDD** — display name, identity chips (from the person detail's identifier evidence — same data `PeopleWorkspace` renders as identifier chips today; read its markup), item counts, `Files N` toggle button, `Link identity` button (opens Task 5's dialog; in this task it renders disabled with a title).
- [ ] **Step 3: RelationshipTimeline TDD** — month headers, one row per item, `chat_burst` renders "N messages in <title>", selection ring, `j`/`k`, Enter fires row-open, load-more via cursor with the capped walk (no unbounded End), 409 restart notice.
- [ ] **Step 4: RelationshipsWorkspace composition TDD** — three panes render; selecting a list row calls `onTargetChange` + controller.openTarget; selecting a timeline row shows the Inspector reading pane; `chat_burst` selection requests the conversation window with the day's UTC `start`/`end`; `filesOpen` swaps center to `FilesWorkspace`; Esc layering; responsive classes applied at the breakpoints (assert class switching with a resized container, not real media queries, if jsdom can't do them — use the container-query class hook pattern and test the class logic).
- [ ] **Step 5: Verify and commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "feat(web): three-pane relationships workspace"
```

---

### Task 5: Identity-link UI

**Files:**
- Create: `web/src/lib/components/relationships/LinkIdentityDialog.svelte` (+ test)
- Modify: `web/src/lib/components/relationships/RelationshipHeader.svelte` (chips get unlink affordance; Link identity opens the dialog; `identity_cache_stale` banner)

**Interfaces:**
- Consumes: `controller.linkParticipants`/`unlinkParticipants` (Task 3), `POST /api/v1/people/search` for the participant search.
- Dialog contract: search input → people search (`identity_query`), results list rows show display name + identifiers + participant IDs; confirming calls `linkParticipants(currentClusterMemberID, chosenParticipantID)`. This IS a true modal flow (an action dialog, not an analytical surface) — kit-ui `Modal` is allowed here per the spec's "`DetailDrawer` remains for true modal flows only"; use `Modal` like `SavedViewsWorkspace` does.
- Outcomes (binding): `ok/ready` → toast-free silent success; header refreshes (controller re-opens target — Task 3 behavior). `ok/stale` → persistent named `identity_cache_stale` banner on the header: "Identity saved; the cache refresh failed — groupings may be stale until a rebuild. Retrying is safe." with a Retry button (re-invokes the same link call — idempotent per the API contract). `already_linked` → inline dialog error: "These identities are already linked through another identifier." `invalid`/`error` → inline message.
- Chips: each identity chip annotated by source; chips representing linked cluster members (member_ids beyond the primary) get an unlink `×` affordance with an inline confirm (not a browser confirm) → `unlinkParticipants`; same outcome handling.

- [ ] **Step 1: Dialog TDD** — search issues people-search calls (debounced 250ms via `web/src/lib/util/debounce.ts`), Enter/click selects, confirm fires link with the right IDs, each outcome renders its named state, Esc closes.
- [ ] **Step 2: Header integration TDD** — Link identity opens dialog; stale banner persists across dialog close and clears after a later `ready` outcome; unlink flow (confirm → call → refresh).
- [ ] **Step 3: Verify and commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "feat(web): identity link and unlink UI"
```

---

### Task 6: AppShell integration — tabs, rendering, landing fallback, People/Domains removal

**Files:**
- Modify: `web/src/lib/components/shell/AppShell.svelte` (tabs ~153-162, workspace chain ~1420-1480, PeopleWorkspace/DomainWorkspace wiring removal)
- Delete: `web/src/lib/components/people/PeopleWorkspace.svelte` + test, `web/src/lib/components/people/DomainWorkspace.svelte` + test (keep `PersonTimeline.svelte` if the Everything person-timeline path still renders it — check `rg -n "PersonTimeline" web/src` first; Everything's timeline presentation still uses it, so KEEP it)
- Modify: `web/src/lib/components/shell/AppShell.test.ts` (fixtures/assertions)

**Interfaces:**
- Tabs become (binding order): `relationships, everything, files, saved_views, sources, deletions, settings` — labels "Relationships · Everything · Files · Saved Views · Sources · Deletions · Settings". `people`/`domains` tabs removed.
- Workspace chain renders `<RelationshipsWorkspace>` for `'relationships'`, wiring the Task 4 props to `exploreState` patches: facet/target/showAll/filesOpen ↔ the Task 2 URL fields, via the existing flush-before-commit navigation wrappers.
- The controller is constructed once in AppShell (like `api`), destroyed in `onDestroy`.
- Landing fallback (binding): when the archive-state the shell already loads reports the analytical engine unavailable (read how the archive-state indicator gets its data — find it with `rg -n "archive-state\|archiveState" web/src/lib/components/shell`) AND the current workspace is `'relationships'` without an explicit user navigation (i.e. the state came from defaults, not a URL that named relationships — detect: the parsed URL had no `explore` param), `replaceTransient({workspace:'everything'})`. When the URL explicitly says relationships, keep it and let the hub show its degraded state.
- `analysisTarget`-based person/domain drill-ins from Everything (e.g. "Open person" affordances): retarget them to commit `{workspace:'relationships', relationshipFacet, relationshipTarget}` — find all call sites with `rg -n "analysisTarget" web/src/lib/components` and update each; Everything's own inline person-timeline presentation stays unchanged.
- Inspector gains an "Open relationship" action for entry selections (spec): a button in the Inspector header that commits to the relationships workspace targeting the entry's primary counterpart participant (the Inspector already knows the entry's participants — read `Inspector.svelte`'s entry model first; if the participant ID isn't available on the entry row, wire it through from `EntryRow` — check `rg -n "participant" web/src/lib/explore/models.ts`).

- [ ] **Step 1: Failing AppShell tests** — tab list content/order; `'relationships'` renders the hub; legacy `workspace=people` URL lands in the hub with the facet set; explicit-URL degraded case keeps relationships; default-landing fallback switches to everything when archive-state says unavailable.
- [ ] **Step 2: Implement + delete the two workspaces** (and their prop-wiring blocks in AppShell; run `rg -n "PeopleWorkspace\|DomainWorkspace" web/src` to catch stragglers).
- [ ] **Step 3: Full web suite; fix fallout; commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "feat(web): relationships hub as landing workspace, retire people/domains"
```

---

### Task 7: AppShell decomposition

**Files:**
- Create: `web/src/lib/components/shell/EverythingWorkspace.svelte` (the current inline Everything `<main>` block: search bar, coverage, context bar, results+inspector split, footer)
- Create: `web/src/lib/explore/loader.svelte.ts` (the extracted Everything data controller)
- Modify: `web/src/lib/components/shell/AppShell.svelte` (shrinks to: shell chrome, tabs, workspace switch, palette/help/viewer, shortcut registration)
- Tests: move/extend colocated tests accordingly (`EverythingWorkspace.test.ts`; loader tests in `web/src/lib/explore/loader.svelte.test.ts`)

**Interfaces (produces):**
```ts
// loader.svelte.ts — owns what AppShell.svelte:670-981 does today:
export class ExploreLoader {
  constructor(client: APIClient, state: ExploreState) {}
  rows: EntryRow[]; groupRows: GroupRow[]; fileFacts: FileFact[];
  result: ExploreResult | null; loading: boolean; error: string | null;
  nextCursor: string | null; pagingNotice: string | null;
  loadMore(): Promise<void>; loadThroughEnd(): Promise<void>;   // capped, Task 1 notice semantics
  destroy(): void;
}
```

Binding refactor rules (from the spec):
- Replace every `JSON.stringify(...)`-as-reactive-dependency in the moved code with explicit `$derived` fingerprint values read via `untrack` in effects (the pattern already used for `coverageFiltersFingerprint`) — after the move, `rg -n "JSON.stringify" web/src/lib/components/shell/AppShell.svelte web/src/lib/components/shell/EverythingWorkspace.svelte web/src/lib/explore/loader.svelte.ts` must show ZERO occurrences used as effect dependencies (serialization for request bodies is fine).
- Collapse the overlapping per-predicate effects: the main load effect, the inspector group-detail load, and the lexical count loader must be driven off ONE fingerprint change each — one state change triggers one coordinated load (no double-fetch; write a test that counts fetches for a single filter commit).
- This is a MOVE, not a rewrite: behavior-preserving. The existing `AppShell.test.ts` assertions keep passing (split the test file along the component split; do not delete assertions — relocate them).
- AppShell.svelte target: under 700 lines after the split. If a piece doesn't fit cleanly (e.g. the Esc-layering spans workspaces), leave it in AppShell and note it in the report rather than forcing an awkward seam.

- [ ] **Step 1: Extract `ExploreLoader`** (move the load/paging/restoration blocks; keep function bodies byte-identical where possible), colocated tests for: one commit → one fetch; paging cap notice; restoration walk.
- [ ] **Step 2: Extract `EverythingWorkspace.svelte`** (markup + its handlers), splitting `AppShell.test.ts`.
- [ ] **Step 3: Fingerprint sweep** (the `rg` above → zero), double-fetch regression test.
- [ ] **Step 4: Full suite; commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "refactor(web): extract everything workspace and explore loader from AppShell"
```

---

### Task 8: Visual pass — theme base layer, type scale, table chrome

**Files:**
- Modify: `web/src/styles/theme-light.css`, `web/src/styles/theme-dark.css`
- Modify: `web/src/lib/a11y/theme-parity.test.ts`
- Modify: table/grid styles in `EverythingTable.svelte`, `GroupTable.svelte`, `RelationshipTimeline.svelte`, `RelationshipList.svelte`, `FilesPresentation.svelte` (chrome only)
- Possibly modify: `web/src/styles/*.css` global type scale

**Binding requirements (spec):**
1. Fix the fragile base-layer pattern: `theme-light.css` currently declares tokens on `:root, :root[data-theme='light']`, so any token missing from `theme-dark.css` silently leaks a light value into dark mode. Restructure: a `:root` block holds ONLY theme-neutral tokens (spacing/type/radii if any); ALL colour tokens live in `:root[data-theme='light']` and `:root[data-theme='dark']` blocks, with `:root:not([data-theme])` defaulting to the light block via a shared selector (`:root:not([data-theme='dark'])`, keeping no-attribute = light). Strengthen `theme-parity.test.ts`: besides name-set equality, assert NO colour token is declared in an unscoped `:root` block.
2. One type scale: audit `font-size` declarations across `web/src/lib/components` (`rg -n "font-size" web/src/lib/components | sort`), collapse to a token scale (`--text-xs/-sm/-base/-lg/-xl`) defined in the neutral block; components reference tokens. Keep visual sizes ~as-is (this is consolidation, not redesign).
3. Quieter table chrome: row borders → `--border-muted`; header backgrounds → `--bg-surface` (not raised); hover → `--bg-surface-hover`; selection keeps `--selected-bg`/`--selected-border`. Density comes from the existing density preference — don't touch it.
4. No modal scrims over analytical surfaces: `rg -n "overlay-bg\|scrim" web/src` — every remaining consumer must be a true modal flow (CommandPalette, KeyboardHelp, dialogs, FileViewer); list them in the commit message body.

- [ ] **Step 1: Theme restructure + parity test (TDD: extend the test first, watch it fail on the current files)**
- [ ] **Step 2: Type-scale tokens + component sweep**
- [ ] **Step 3: Table chrome + scrim audit**
- [ ] **Step 4: Visual smoke** — build + serve a fake-vault daemon, screenshot light/dark of Relationships + Everything + open reading pane (Playwright script per the webapp-testing pattern), verify no full-viewport translucent overlay exists with the reading pane open (reuse the scrim-detection check from the earlier smoke script). Keep the screenshots in the scratchpad for the final report.
- [ ] **Step 5: Full suite; commit**

```bash
cd web && bun run check && bun run test
git add -A && git commit -m "style(web): theme base-layer fix, type scale, quieter table chrome"
```

---

### Task 9: Playwright, full verification, final review

- [ ] **Step 1: Playwright updates**

- Rewrite `web/tests/people-domains.spec.ts` → `web/tests/relationships.spec.ts`: legacy `workspace=people` URL lands on the relationships hub; ranked list renders; opening a person shows the timeline; a chat-burst row opens the reading pane; facet toggle to Domains; browser back returns through the states.
- Extend `web/tests/e2e/accessibility.spec.ts` + `keyboard.spec.ts` to cover the hub (axe pass on the three-pane layout; `j`/`k`/Enter/Esc walk).
- Run whatever Playwright projects the repo's config wires to a fixture server (check `web/playwright.config.ts` for how existing specs get data — follow it).

- [ ] **Step 2: Full verification**

```bash
cd web && bun run check && bun run test && bun run build
cd .. && make test && make lint-ci
```

Plus the fake-vault UI smoke: build, `msgvault serve` on the synthetic vault, headless Playwright — hub renders ranked list, link identity flow (link two participants → combined cluster in the list), timeline bursts, reading pane, no scrim, dark theme clean. Stop the daemon after.

- [ ] **Step 3: Final whole-branch review** (superpowers final review flow), fix wave if needed, ledger, commit.

- [ ] **Step 4: roborev-fix** for the reviews this plan's commits generate.
