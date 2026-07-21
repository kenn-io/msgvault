# Shell Stabilization Implementation Plan (Plan 1 of 3)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the user-visible quality and performance defects in the existing web UI — dark-theme washout, raw `###` markup in excerpts, per-keystroke DuckDB queries, unbounded walk-to-end, fixed-interval polling, and reactive-dependency hacks — per the approved spec `docs/superpowers/specs/2026-07-20-web-ui-relationships-design.md`.

**Architecture:** Backend excerpt flattening in the explore row path (Go, `internal/query`), then frontend changes in the Svelte 5 SPA under `web/src`: the inspector becomes pinned-only (removing the modal `DetailDrawer` scrim that collapses dark-mode contrast), search typing debounces before hitting state, walk-to-end is capped, coverage polling backs off, and `JSON.stringify` dependency hacks become `$derived` fingerprints.

**Tech Stack:** Go + testify + DuckDB test fixtures (`internal/query`); Svelte 5 runes + vitest + @testing-library/svelte (`web/`); bun as the package runner.

**Scope note:** This is Plan 1 of 3 for the spec. Plan 2 (relationships backend) and Plan 3 (three-pane Relationships workspace) follow. Full AppShell component decomposition is deliberately deferred to Plan 3, where the new workspace forces the restructuring; this plan only removes the reactive-dependency hacks that make AppShell's effects fire redundantly.

## Global Constraints

- Go tests use testify only: `require.X` halts, `assert.X` continues; argument order is `(want, got)`. Never introduce `t.Errorf`/`t.Fatalf`.
- After Go changes: `go fmt ./...` and `go vet ./...`; stage all resulting changes.
- Web commands run from `web/`: `bun run test` (vitest), `bun run check` (svelte-check). Both must pass before each commit.
- Never use real PII in fixtures — synthetic names/addresses only.
- Commit after every task; pre-commit hooks must pass without `--no-verify`.
- Run `make lint-ci` before finishing (CI has checks not in the pre-commit hook).

---

### Task 1: Flatten snippet markup in explore excerpts (Go)

Meeting importers (Granola/Circleback) persist raw body prefixes as snippets — `internal/granola/format.go:132`, `internal/circleback/format.go:194` — so `### Heading` markdown surfaces verbatim in Everything rows. Flatten at query time so existing stored snippets are covered without migration.

**Files:**
- Create: `internal/query/snippet.go`
- Create: `internal/query/snippet_test.go`
- Modify: `internal/query/explore.go` (scan loop, ~line 81-90)
- Modify: `internal/api/explore.go` (semantic excerpt fill, ~line 1091-1100)

**Interfaces:**
- Produces: `query.FlattenSnippet(snippet string) string` — pure function, exported so `internal/api` can reuse it for `match.strongest_excerpt`.

- [ ] **Step 1: Write the failing test**

Create `internal/query/snippet_test.go`:

```go
package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFlattenSnippet(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain text unchanged", "Quarterly planning notes", "Quarterly planning notes"},
		{"leading heading", "### Architecture\nMonorepo vs. two repos", "Architecture Monorepo vs. two repos"},
		{"mid-string heading after join", "sync When: 09:00 ### Architecture: Monorepo", "sync When: 09:00 Architecture: Monorepo"},
		{"issue reference preserved", "raulcd opened a new pull request, #50362", "raulcd opened a new pull request, #50362"},
		{"hashtag preserved", "tagged #launch in the notes", "tagged #launch in the notes"},
		{"bullet list", "- keep Core separate\n- Chinese wall", "keep Core separate Chinese wall"},
		{"numbered list", "1. kickoff talk\n2) judging", "kickoff talk judging"},
		{"bold and code stripped", "**Consensus:** keep `Kenn Core` separate", "Consensus: keep Kenn Core separate"},
		{"whitespace collapsed", "line one\n\n\tline two", "line one line two"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, FlattenSnippet(tt.in))
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" ./internal/query/ -run TestFlattenSnippet -v`
Expected: FAIL — `undefined: FlattenSnippet`

- [ ] **Step 3: Write the implementation**

Create `internal/query/snippet.go`:

```go
package query

import (
	"regexp"
	"strings"
)

var (
	// Heading markers require trailing whitespace so issue refs (#50362)
	// and hashtags (#launch) survive.
	snippetHeading    = regexp.MustCompile(`(^|\s)#{1,6}\s+`)
	snippetListMarker = regexp.MustCompile(`(?m)^\s{0,3}(?:[-*+]|\d{1,3}[.)])\s+`)
	snippetEmphasis   = regexp.MustCompile("(?:\\*\\*|__|`)")
	snippetWhitespace = regexp.MustCompile(`\s+`)
)

// FlattenSnippet renders stored snippet markup (markdown headings, list
// markers, bold/code emphasis) as plain single-line text. Meeting importers
// persist raw body prefixes as snippets, so structural markdown otherwise
// surfaces verbatim in explore excerpts.
func FlattenSnippet(snippet string) string {
	if snippet == "" {
		return snippet
	}
	flattened := snippetHeading.ReplaceAllString(snippet, "$1")
	flattened = snippetListMarker.ReplaceAllString(flattened, "")
	flattened = snippetEmphasis.ReplaceAllString(flattened, "")
	flattened = snippetWhitespace.ReplaceAllString(flattened, " ")
	return strings.TrimSpace(flattened)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags "fts5 sqlite_vec" ./internal/query/ -run TestFlattenSnippet -v`
Expected: PASS (all subtests)

- [ ] **Step 5: Apply at the explore scan site**

In `internal/query/explore.go`, the row scan loop ends around line 88 (`rows.Scan(... &row.Title, &row.Preview, ...)`). Immediately after the successful `Scan` (before the row is appended), add:

```go
row.Title = FlattenSnippet(row.Title)
row.Preview = FlattenSnippet(row.Preview)
```

- [ ] **Step 6: Apply at the semantic-excerpt site**

In `internal/api/explore.go` (~line 1095), the semantic excerpt map is built from message summaries:

```go
byID[summary.ID] = summary.Snippet
```

Change to:

```go
byID[summary.ID] = query.FlattenSnippet(summary.Snippet)
```

(`internal/api` already imports `go.kenn.io/msgvault/internal/query`.)

- [ ] **Step 7: Run the affected packages' tests**

Run: `go test -tags "fts5 sqlite_vec" ./internal/query/ ./internal/api/`
Expected: PASS. If any existing test asserts a raw markup snippet in a preview, update its expectation to the flattened form — the flattening is the new intended behavior.

- [ ] **Step 8: Format, vet, commit**

```bash
go fmt ./... && go vet ./...
git add -A
git commit -m "fix(query): flatten snippet markup in explore excerpts"
```

---

### Task 2: Make the inspector pinned-only

The unpinned inspector renders as kit-ui `DetailDrawer` with a full-viewport 62% black scrim (`--overlay-bg`, `theme-dark.css:39`) — this is the dark-theme washout. The pinned side panel becomes the only mode; the dead `relationships`/`summary` extension-slot scaffolding goes too.

**Files:**
- Modify: `web/src/lib/explore/state.svelte.ts` (default line 88, normalize line 255)
- Modify: `web/src/lib/components/inspector/Inspector.svelte` (props ~39-77, extension slots 300-303, render branch 307-325, CSS 441-443)
- Modify: `web/src/lib/components/shell/AppShell.svelte` (lines 1544, 1651-1696)
- Modify: `web/src/lib/explore/state.test.ts`, `web/src/lib/components/inspector/Inspector.test.ts`, `web/src/lib/components/shell/AppShell.test.ts` (whatever references pinning/drawer)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces: `Inspector` no longer accepts `pinned`/`onPinnedChange` props; `ExploreURLState.inspectorPinned` remains in the URL envelope but always normalizes to `true`.

- [ ] **Step 1: Write the failing normalization test**

In `web/src/lib/explore/state.test.ts`, add alongside the existing round-trip tests:

```ts
it('normalizes legacy unpinned inspector states to pinned', () => {
  const parsed = parseExploreURLState(serializeExploreURLState({
    ...defaultExploreURLState,
    inspectorPinned: false
  }));
  expect(parsed.inspectorPinned).toBe(true);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && bun run test -- src/lib/explore/state.test.ts`
Expected: FAIL — `inspectorPinned` parses back as `false`.

- [ ] **Step 3: Force pinned in state**

In `web/src/lib/explore/state.svelte.ts`:

- Line 88 (`defaultExploreURLState`): `inspectorPinned: false,` → `inspectorPinned: true,`
- Line 255 (`normalize`): `inspectorPinned: value.inspectorPinned === true,` → `inspectorPinned: true,`

Keep the field in the envelope so old URLs and saved views (which persist inspector preference) parse cleanly; they simply normalize to pinned.

If the existing "round-trips every durable field" test asserts `inspectorPinned: false` round-trips, update it to expect `true` — the normalization is now intentional.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && bun run test -- src/lib/explore/state.test.ts`
Expected: PASS.

- [ ] **Step 5: Remove the DetailDrawer branch from Inspector.svelte**

- Import (line 26): drop `DetailDrawer` → `import { Button, SplitResizeHandle, type SplitResizeEvent } from '@kenn-io/kit-ui';`
- Props: remove `pinned` and `onPinnedChange` from the destructuring (lines 47, 50) and the type (lines 66, 69).
- Locate any header pin/unpin control with `rg -n "onPinnedChange" web/src/lib/components/inspector/Inspector.svelte` and remove it.
- Replace the render branch (lines 307-325) with the unconditional pinned markup:

```svelte
<div class="pinned-inspector">
  <SplitResizeHandle ariaLabel="Resize inspector" onResizeStart={beginResize} onResize={resize} />
  <aside aria-label={`Inspect ${title}`} style:width={`${width}px`}>
    {@render header()}
    <div class="pinned-body">{@render contents()}</div>
  </aside>
</div>
```

- Delete the dead extension-slot scaffolding: the `<div class="extension-slots" …>` block (lines 300-303) and its `.extension-slots { display: none; }` rule (lines 441-443).

- [ ] **Step 6: Remove the unpinned render path from AppShell.svelte**

- Delete the whole unpinned block, lines 1675-1696 (`{#if inspectorTargetKey && !exploreState.current.inspectorPinned}` … `{/if}`).
- Line 1651: `{#if inspectorTargetKey && exploreState.current.inspectorPinned}` → `{#if inspectorTargetKey}`.
- Line 1544: `class:results-and-inspector--pinned={Boolean(inspectorTargetKey && exploreState.current.inspectorPinned)}` → `class:results-and-inspector--pinned={Boolean(inspectorTargetKey)}`.
- In the remaining `<Inspector …>` instance, remove the `pinned` and `onPinnedChange={…}` props.
- Sweep for stragglers: `rg -n "inspectorPinned|onPinnedChange" web/src/lib/components/shell/AppShell.svelte` — any remaining `commitNavigation({ inspectorPinned })` call sites go with the removed prop.

- [ ] **Step 7: Fix affected tests and type-check**

Run: `cd web && bun run check && bun run test`
Expected: svelte-check flags any leftover `pinned` prop usage — fix all. Inspector/AppShell tests that exercised drawer mode or the pin toggle should be updated to assert the pinned panel renders (and that no `.kit-detail-drawer-overlay` exists after selecting a row). Delete tests that only covered the removed drawer behavior.

- [ ] **Step 8: Verify no scrim remains, then commit**

Run: `rg -n "DetailDrawer" web/src` — expected: no matches.

```bash
git add -A
git commit -m "fix(web): make the inspector a pinned side panel only"
```

---

### Task 3: Guard theme-token parity between light and dark

`theme-light.css:1` uses `:root, :root[data-theme='light']` — light is the unconditional base layer, so any token added to light but forgotten in dark silently renders its light value in dark mode. Both files currently define the same 38 tokens; lock that in with a test.

**Files:**
- Create: `web/src/lib/a11y/theme-parity.test.ts`

**Interfaces:**
- Consumes/Produces: nothing — standalone guard test (mirrors the file-reading pattern of `web/src/lib/a11y/contrast.test.ts`).

- [ ] **Step 1: Write the test**

```ts
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const root = process.cwd();

function definedTokenNames(cssPath: string): string[] {
  const css = readFileSync(join(root, cssPath), 'utf8');
  return [...new Set([...css.matchAll(/--[\w-]+(?=\s*:)/g)].map((match) => match[0]))].sort();
}

describe('theme token parity', () => {
  it('defines the identical custom-property set in light and dark themes', () => {
    expect(definedTokenNames('src/styles/theme-dark.css'))
      .toEqual(definedTokenNames('src/styles/theme-light.css'));
  });

  it('overrides color-scheme for dark mode', () => {
    const tokens = readFileSync(join(root, 'src/styles/tokens.css'), 'utf8');
    expect(tokens).toMatch(/:root\[data-theme='dark'\]\s*\{[^}]*color-scheme:\s*dark/);
  });
});
```

- [ ] **Step 2: Run and verify it passes today**

Run: `cd web && bun run test -- src/lib/a11y/theme-parity.test.ts`
Expected: PASS (token sets are currently identical).

- [ ] **Step 3: Verify the test catches a leak**

Temporarily add `--parity-canary: red;` to `theme-light.css`, re-run, confirm FAIL, then revert the canary.

- [ ] **Step 4: Commit**

```bash
git add web/src/lib/a11y/theme-parity.test.ts
git commit -m "test(web): guard light/dark theme token parity"
```

---

### Task 4: Debounce identity and filename search typing

`SearchInput` `oninput` fires per keystroke into `exploreState.replaceCommittedDraft(...)`, which refires the workspace search effects — a `limit: 500` DuckDB query per character (People/Domains identity search, Files filename filter). Debounce the state write 250 ms; Everything's submit-only search is untouched.

**Files:**
- Create: `web/src/lib/util/debounce.ts`
- Create: `web/src/lib/util/debounce.test.ts`
- Modify: `web/src/lib/components/shell/AppShell.svelte` (handler wirings at lines 1360, 1365, 1381, 1385, 1456-1458; `onDestroy`)

**Interfaces:**
- Produces: `debounce<T extends unknown[]>(fn: (...args: T) => void, delayMs: number): Debounced<T>` where `Debounced<T>` is callable and has `.cancel()`. Task 5+ and Plan 3 may reuse it.

- [ ] **Step 1: Write the failing utility test**

Create `web/src/lib/util/debounce.test.ts`:

```ts
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { debounce } from './debounce';

describe('debounce', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('fires once with the latest arguments after the delay', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    debounced('ab');
    debounced('abc');
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(250);
    expect(fn).toHaveBeenCalledExactlyOnceWith('abc');
  });

  it('restarts the delay on each call', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    vi.advanceTimersByTime(200);
    debounced('ab');
    vi.advanceTimersByTime(200);
    expect(fn).not.toHaveBeenCalled();
    vi.advanceTimersByTime(50);
    expect(fn).toHaveBeenCalledExactlyOnceWith('ab');
  });

  it('cancel discards the pending call', () => {
    const fn = vi.fn();
    const debounced = debounce(fn, 250);
    debounced('a');
    debounced.cancel();
    vi.advanceTimersByTime(1000);
    expect(fn).not.toHaveBeenCalled();
  });
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd web && bun run test -- src/lib/util/debounce.test.ts`
Expected: FAIL — module not found.

- [ ] **Step 3: Implement the utility**

Create `web/src/lib/util/debounce.ts`:

```ts
export interface Debounced<T extends unknown[]> {
  (...args: T): void;
  cancel(): void;
}

export function debounce<T extends unknown[]>(
  fn: (...args: T) => void,
  delayMs: number
): Debounced<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  const debounced = (...args: T): void => {
    if (timer !== undefined) clearTimeout(timer);
    timer = setTimeout(() => {
      timer = undefined;
      fn(...args);
    }, delayMs);
  };
  debounced.cancel = (): void => {
    if (timer !== undefined) clearTimeout(timer);
    timer = undefined;
  };
  return debounced;
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd web && bun run test -- src/lib/util/debounce.test.ts`
Expected: PASS.

- [ ] **Step 5: Wire it into AppShell**

Add near the other module-scope constants in `AppShell.svelte` (after line ~95):

```svelte
import { debounce } from '../../util/debounce';

const SEARCH_TYPING_DEBOUNCE_MS = 250;
const debouncedSearchPatch = debounce(
  (patch: Partial<ExploreURLState>) => exploreState.replaceCommittedDraft(patch),
  SEARCH_TYPING_DEBOUNCE_MS
);
```

Replace the five per-keystroke handlers:

```svelte
<!-- line 1360, PeopleWorkspace -->
onIdentityQueryChange={(identityQuery) => debouncedSearchPatch({ identityQuery, analysisTarget: null, selectedIdentifier: null })}
<!-- line 1365, PeopleWorkspace -->
onFileFilenameQueryChange={(fileFilenameQuery) => debouncedSearchPatch({ fileFilenameQuery })}
<!-- line 1381, DomainWorkspace -->
onIdentityQueryChange={(identityQuery) => debouncedSearchPatch({ identityQuery, analysisTarget: null })}
<!-- line 1385, DomainWorkspace -->
onFileFilenameQueryChange={(fileFilenameQuery) => debouncedSearchPatch({ fileFilenameQuery })}
<!-- lines 1456-1458, FilesWorkspace -->
onFilenameQueryChange={(fileFilenameQuery) => debouncedSearchPatch({
  fileFilenameQuery, activeRow: null, selectedRow: null, scrollAnchor: null
})}
```

In the existing `onDestroy` callback (locate with `rg -n "onDestroy" web/src/lib/components/shell/AppShell.svelte`), add `debouncedSearchPatch.cancel();`.

Note the accepted tradeoff: the `SearchInput` `value` prop lags typed text by ≤250 ms. The input's DOM value already holds the typed text, so no visible clobbering occurs unless an unrelated re-render lands mid-typing — acceptable, and this whole surface is rebuilt in Plan 3.

- [ ] **Step 6: Write the shell-level debounce test**

In `web/src/lib/components/shell/AppShell.test.ts` (follow the file's existing render/client-stub helpers and the fake-timer precedent of the "debounces rapid visible conversation churn" test at ~line 516):

```ts
it('debounces identity search typing into one committed state write', async () => {
  vi.useFakeTimers();
  try {
    const state = new ExploreState();
    state.commitWorkspace('people');
    // render AppShell with the file's standard client stub and `state`
    const input = screen.getByLabelText('Search people');
    await fireEvent.input(input, { target: { value: 'al' } });
    await fireEvent.input(input, { target: { value: 'alice' } });
    expect(state.current.identityQuery).toBe('');
    vi.advanceTimersByTime(250);
    expect(state.current.identityQuery).toBe('alice');
  } finally {
    vi.useRealTimers();
  }
});
```

- [ ] **Step 7: Run the full web suite and type-check**

Run: `cd web && bun run check && bun run test`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "perf(web): debounce identity and filename search typing"
```

---

### Task 5: Cap walk-to-end pagination

`End` triggers `loadThroughEnd`, which loops 500-row pages until the cursor is exhausted — on an unfiltered 2.5M-row archive that is thousands of sequential round-trips. Cap each invocation at 20 pages; the cursor is preserved, so pressing `End` again continues from where it paused.

**Files:**
- Create: `web/src/lib/explore/paging.ts`
- Modify: `web/src/lib/components/shell/AppShell.svelte:834-839` (`loadThroughEnd`)
- Modify: `web/src/lib/components/people/PeopleWorkspace.svelte:226-233` (`loadTimelineThroughEnd`)
- Modify: `web/src/lib/components/people/DomainWorkspace.svelte:215-222` (`loadThroughEnd`)

**Interfaces:**
- Produces: `LOAD_THROUGH_END_MAX_PAGES = 20` exported from `web/src/lib/explore/paging.ts`.

- [ ] **Step 1: Create the shared constant**

Create `web/src/lib/explore/paging.ts`:

```ts
// Each walk-to-end invocation loads at most this many pages so `End` on a
// multi-million-row view cannot spiral into thousands of sequential
// round-trips. The cursor is preserved: pressing End again continues.
export const LOAD_THROUGH_END_MAX_PAGES = 20;
```

- [ ] **Step 2: Cap AppShell's loadThroughEnd**

Replace lines 834-839 of `AppShell.svelte`:

```svelte
async function loadThroughEnd(): Promise<void> {
  let pages = 0;
  while (nextCursor && !requestController?.signal.aborted) {
    if (pages >= LOAD_THROUGH_END_MAX_PAGES) {
      sortNotice = 'End paused loading to keep the table responsive; press End again to continue or refine the filters.';
      return;
    }
    const outcome = await loadMore();
    if (outcome.status !== 'advanced') break;
    pages += 1;
  }
}
```

Add `import { LOAD_THROUGH_END_MAX_PAGES } from '../../explore/paging';` to the imports block. (`sortNotice` feeds the `role="status"` live region at line 1540, so the pause is announced to screen readers.)

- [ ] **Step 3: Cap the People timeline walk**

Replace `loadTimelineThroughEnd` in `PeopleWorkspace.svelte` (lines 226-233):

```svelte
async function loadTimelineThroughEnd(): Promise<void> {
  let pages = 0;
  while (timelineNextCursor && !detailController?.signal.aborted) {
    if (pages >= LOAD_THROUGH_END_MAX_PAGES) return;
    const before = timelineNextCursor;
    await loadMoreTimeline();
    if (timelineNextCursor === before) break;
    pages += 1;
  }
}
```

Add the same import. Apply the identical transformation to `DomainWorkspace.svelte`'s `loadThroughEnd` (line 215; same shape, `timelineNextCursor`/`loadMoreTimeline` names may differ slightly — mirror whatever that function's loop uses).

- [ ] **Step 4: Write the cap test**

In `web/src/lib/components/shell/AppShell.test.ts`, using the file's existing client stub: make the explore endpoint always return a fresh, never-repeating `nextCursor` and one new row per page, render, trigger `End` (the table's `onLoadThroughEnd`), and assert the explore endpoint was called exactly `1 + LOAD_THROUGH_END_MAX_PAGES` times (initial load + 20 pages) rather than unbounded:

```ts
it('caps End at LOAD_THROUGH_END_MAX_PAGES pages per press', async () => {
  // client stub: POST /api/v1/explore returns { rows: [row(n)], nextCursor: `cursor-${n}` } with n incrementing
  // render AppShell, wait for initial load
  // fire keydown End on the grid
  // await settle
  expect(explorePostCount).toBe(1 + LOAD_THROUGH_END_MAX_PAGES);
});
```

- [ ] **Step 5: Run the web suite and type-check**

Run: `cd web && bun run check && bun run test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "perf(web): cap walk-to-end pagination at 20 pages per press"
```

---

### Task 6: Back off the coverage poll

The semantic-coverage effect re-polls every fixed 1 s while coverage is `initializing` (`AppShell.svelte:441-483`). Add exponential backoff: 1 s, 2 s, 4 s, 8 s, capped at 10 s; reset when the coverage context (workspace/mode/filters) changes or coverage leaves `initializing`.

**Files:**
- Modify: `web/src/lib/components/shell/AppShell.svelte:441-483`

**Interfaces:**
- Consumes: nothing new (Task 7 later converts this effect's `JSON.stringify(filters)` dependency; keep that line intact here to avoid conflicts).

- [ ] **Step 1: Add attempt tracking and backoff**

Near the other coverage state (line ~143), add plain (non-`$state`) variables:

```svelte
let coveragePollAttempts = 0;
let coveragePollKey = '';
```

Inside the effect, after the dependency reads (lines 442-446), add:

```svelte
const pollKey = `${workspace}|${mode}|${JSON.stringify(filters)}`;
if (pollKey !== coveragePollKey) {
  coveragePollKey = pollKey;
  coveragePollAttempts = 0;
}
```

Replace the fixed re-poll (lines 463-468):

```svelte
if (loaded.status === 'initializing') {
  const delay = Math.min(10_000, 1_000 * 2 ** coveragePollAttempts);
  coveragePollAttempts += 1;
  coveragePollTimer = setTimeout(() => {
    coveragePollTimer = undefined;
    if (generation === coverageRequestGeneration) coverageRetryRevision += 1;
  }, delay);
} else {
  coveragePollAttempts = 0;
}
```

- [ ] **Step 2: Write the backoff test**

In `AppShell.test.ts` with fake timers and a client stub whose coverage endpoint always returns `status: 'initializing'`: assert the second poll fires only after 1 s, the third after a further 2 s, the fourth after a further 4 s (count coverage POSTs at each `vi.advanceTimersByTime` checkpoint). Follow the existing coverage-test setup in that file.

- [ ] **Step 3: Run the web suite**

Run: `cd web && bun run check && bun run test`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "perf(web): exponential backoff for semantic coverage polling"
```

---

### Task 7: Replace JSON.stringify dependency hacks with $derived fingerprints

Ten effects use a bare `JSON.stringify(x);` statement to force deep reactive tracking. Each re-serializes on every run and refires the effect even when the serialized value is unchanged. Hoisting to a `$derived` string means dependents only re-run when the fingerprint value actually changes.

**Files:**
- Modify: `web/src/lib/components/shell/AppShell.svelte` (~lines 221, 376, 446, 603)
- Modify: `web/src/lib/components/people/PeopleWorkspace.svelte` (~lines 80, 111)
- Modify: `web/src/lib/components/people/DomainWorkspace.svelte` (~lines 73, 104)
- Any further sites found by the sweep in Step 1.

**Interfaces:**
- Consumes: Task 6's `pollKey` (coordinate: the coverage effect keeps computing `pollKey` from the untracked value; only the *dependency* line changes).

- [ ] **Step 1: Enumerate the hack sites**

Run: `rg -n "JSON.stringify" web/src/lib/components web/src/lib/explore`
A "hack site" is a bare `JSON.stringify(x);` expression statement (value discarded) inside an `$effect`. Legitimate value uses (serialization for URLs, fingerprint keys that are then *used*) stay.

- [ ] **Step 2: Convert each site — the pattern**

For the coverage effect in `AppShell.svelte` (lines 444-446), hoist a derived fingerprint above the effect:

```svelte
const coverageFiltersFingerprint = $derived(JSON.stringify(exploreState.current.filters));
```

and inside the effect replace:

```svelte
const filters = exploreState.current.filters;
void coverageRetryRevision;
JSON.stringify(filters);
```

with:

```svelte
void coverageRetryRevision;
void coverageFiltersFingerprint;
const filters = untrack(() => exploreState.current.filters);
```

(`untrack` is already imported at line 12.) The effect now depends on the *string* — identical filter content no longer refires it.

For `PeopleWorkspace.svelte`'s search effect (line 80 `JSON.stringify(context);`):

```svelte
const searchContextFingerprint = $derived(JSON.stringify(contextPredicate(predicate)));
```

and in the effect:

```svelte
void searchContextFingerprint;
const context = untrack(() => contextPredicate(predicate));
```

(import `untrack` from `'svelte'` if the file doesn't already). Apply the same mechanical transformation to every site from Step 1: one `$derived` fingerprint per distinct tracked object, `void <fingerprint>;` as the dependency, `untrack(() => …)` for the value read.

- [ ] **Step 3: Verify no bare-stringify statements remain**

Run: `rg -n "^\s*JSON.stringify\(.*\);\s*$" web/src/lib`
Expected: no matches.

- [ ] **Step 4: Run the full web suite**

Run: `cd web && bun run check && bun run test`
Expected: PASS — behavior is unchanged; only refire granularity improves. Pay attention to any test that asserted effect refire counts.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "perf(web): replace stringify dependency hacks with derived fingerprints"
```

---

### Task 8: Final verification pass

**Files:** none new.

- [ ] **Step 1: Full test suites**

```bash
make test
cd web && bun run check && bun run test
```

Expected: all pass.

- [ ] **Step 2: Lint (CI parity)**

```bash
make lint-ci
```

Expected: clean (CI's testify-helper-check is not in the pre-commit hook).

- [ ] **Step 3: Visual smoke test**

Build and serve (`make build && ./msgvault serve`), open the printed URL in dark mode, and confirm: selecting an Everything row opens a side panel with no full-screen scrim and no contrast collapse; meeting rows no longer show `###` in excerpts; typing in People search feels smooth. Capture a screenshot for comparison against `~/Desktop/Screenshot 2026-07-20 at 11.43.01.png`.

- [ ] **Step 4: Commit any stragglers**

```bash
git status
git add -A && git commit -m "chore: shell stabilization follow-ups"
```

(Skip if clean.)
