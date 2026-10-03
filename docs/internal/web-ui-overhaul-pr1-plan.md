---
title: "Web UI overhaul PR 1: foundation and shell — implementation plan"
description: "Implementation plan for the Web UI foundation and application shell."
last_edited: "2026-09-29"
---

# Web UI overhaul PR 1: foundation and shell — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task by task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the ten-tab top bar with a grouped, collapsible sidebar and a
slim top bar that holds global search, and give every workspace the same page
header, palette, and type rules, without removing any capability.

**Architecture:** New focused shell components (`AppSidebar`,
`NavigationDrawer`, `DisplayMenu`, `PageHeader`, and the reworked `SearchBar`)
live in `web/src/lib/components/shell/` and `web/src/lib/components/search/`.
`AppShell.svelte` composes them and keeps ownership of navigation state,
shortcuts, and focus. A palette file outside `src/` retunes kit-ui tokens.

**Tech stack:** Svelte 5 (runes), `@kenn-io/kit-ui` at pin `e43c820f`,
`@lucide/svelte` 1.26.0, Vitest with Testing Library (jsdom), Playwright.

**Spec:** [Web UI overhaul](web-ui-overhaul-design.md). Read its "Visual
language", "Information architecture", and "Control relocation" sections before
starting. This plan covers delivery item 1 only.

## Global constraints

- Keep every capability. A control that moves keeps its accessible name unless
  the spec names the change.
- Keep URL state keys, the `explore` JSON format, and API calls unchanged.
- No new npm dependencies. Icons come from `@lucide/svelte`
  (`@lucide/svelte/icons/<name>`).
- One solid primary button per screen: `tone="info" surface="solid"`.
- Sentence case for every visible label, heading, table header, and chip.
- Monospace only for identifiers, hashes, code, keys, and cron text.
- Raw colors appear only in `web/palette.css`. Components use tokens;
  `bun run check:kit-ui` enforces this for `src/`.
- Tests use Vitest `expect` and Testing Library for unit tests, and Playwright
  for browser tests. Prefer role and accessible-name queries.
- Run commands from `web/` unless a step says otherwise.
- Commit after each task with the `kenn-io-commit` skill. The repository is
  public: run the private-data scan before each commit.

## File structure

| File | Responsibility |
|---|---|
| `web/palette.css` (new) | kit-ui token values: neutral surfaces, one blue accent, status colors, system fonts, type scale, light and dark |
| `web/src/main.ts` | Imports the palette after kit-ui's theme and before `app.css` |
| `web/src/styles/tokens.css` | Semantic aliases; `[data-mono]` and `[data-section-label]` rules |
| `web/src/app.css` | Base element rules and the few kit-class overrides kit has no token for |
| `web/src/lib/commands/registry.ts` | Adds the Shift+Space command so the dialog documents it |
| `web/src/lib/explore/state.svelte.ts` | Adds `commitSearchIn(workspace, query, mode)` |
| `web/src/lib/components/search/SearchBar.svelte` | Global search form: live or draft mode, narrow layout |
| `web/src/lib/components/shell/navigation.ts` (new) | Sidebar groups, labels, icons, and workspace ids |
| `web/src/lib/components/shell/AppSidebar.svelte` (new) | Grouped nav, icon rail, footer (status and shortcuts), collapse toggle |
| `web/src/lib/components/shell/NavigationDrawer.svelte` (new) | Narrow-screen modal: scrim, Escape, focus trap, focus restoration |
| `web/src/lib/components/shell/DisplayMenu.svelte` (new) | Per-tab density override and "Use daemon theme" |
| `web/src/lib/components/shell/PageHeader.svelte` (new) | Title, description, actions, and optional view row |
| `web/src/lib/components/shell/AppShell.svelte` | Composes the shell; wires search, sidebar, drawer, title |
| `web/src/lib/components/shell/EverythingWorkspace.svelte` | Loses its search form and keyboard footer; uses `PageHeader` |
| `web/src/lib/components/explore/SelectionBar.svelte` | Loses its key badges |
| Each workspace component | Uses `PageHeader`; loses eyebrows; sentence case |
| `web/src/App.svelte`, `web/src/lib/components/auth/Login.svelte` | Boot and sign-in screens use the palette and lose eyebrows |
| `web/tests/kit-ui.ts` | Navigation and display-menu helpers for Playwright |

---

### Task 1: Palette and type rules

**Files:**
- Create: `web/palette.css`
- Modify: `web/src/main.ts`, `web/src/styles/tokens.css`, `web/src/app.css`
- Modify (uppercase removal): `DeletionsWorkspace.svelte:462`,
  `OperationRunDetail.svelte:152`, `OperationsWorkspace.svelte:377`,
  `OperationRelatedStatus.svelte:167`, `CardDAVConflicts.svelte:308`,
  `SourcesWorkspace.svelte:473`, `FilesPresentation.svelte:357`,
  `GroupTable.svelte:437`, `IdentityCandidateCard.svelte:89`,
  `EverythingTable.svelte:693`, `KeyboardHelp.svelte:78`,
  `RelationshipReviewCard.svelte:61`, `FilesWorkspace.svelte:1107`,
  `PersonTimeline.svelte:54`, `SavedViewsWorkspace.svelte:361`

**Interfaces:**
- Produces: kit tokens with new values; `--nav-width: 232px`,
  `--nav-rail-width: 56px`, `--nav-active-bg`, `--header-height: 48px`.
  Later tasks read these tokens.

CSS has no meaningful unit test. This task is verified by `check:kit-ui`,
the build, and screenshot review in Task 13.

- [ ] **Step 1: Capture before screenshots**

The fixture daemon from the design review runs at `http://127.0.0.1:18733`. If
it is not running, start it as described in
`docs/screenshots/generate-web-fixture-screenshots.sh`, with an isolated
`--home`. Save captures of all ten workspaces at 1440×900 light, 1440×900 dark,
and 420×860 light into the scratchpad `before/` directory, using the script
from the design review (`shoot.mjs`).

- [ ] **Step 2: Create `web/palette.css`**

```css
/*
 * msgvault retunes kit-ui's tokens for a quiet archive workspace: platform
 * system fonts, neutral surfaces, one blue accent, and green, amber, and red
 * for status only. kit-ui components read only these tokens, so declaring
 * them after kit-ui/theme.css restyles every component consistently.
 *
 * This is the palette definition file, so raw colors belong here. It lives
 * outside src/ because kit-ui-check guards component styles, not palettes.
 */
:root {
  --bg-primary: #f7f8fa;
  --bg-surface: #ffffff;
  --bg-surface-hover: #f2f4f7;
  --bg-inset: #f4f5f7;
  --border-default: #dde1e6;
  --border-muted: #eceef1;

  --text-primary: #1b1f24;
  --text-secondary: #464e58;
  --text-muted: #666e79;

  --accent-blue: #0061d5;
  --accent-amber: #b45309;
  --accent-purple: #6d4ad6;
  --accent-green: #047857;
  --accent-red: #c62828;
  --accent-teal: #0e7490;

  --nav-active-bg: #e6eefb;

  --shadow-sm: 0 1px 2px rgba(16, 24, 40, 0.05);
  --shadow-md: 0 4px 12px rgba(16, 24, 40, 0.08);
  --shadow-lg: 0 16px 40px rgba(16, 24, 40, 0.16);
  --overlay-bg: rgba(16, 24, 40, 0.32);

  --radius-sm: 4px;
  --radius-md: 6px;
  --radius-lg: 8px;

  --font-sans: -apple-system, BlinkMacSystemFont, "Segoe UI", system-ui, Roboto,
    "Helvetica Neue", Arial, sans-serif;
  --font-mono: ui-monospace, "SF Mono", SFMono-Regular, Menlo, Consolas,
    "Liberation Mono", monospace;
  --letter-spacing-label: 0;

  --font-size-2xs: 0.6875rem;
  --font-size-xs: 0.75rem;
  --font-size-sm: 0.8125rem;
  --font-size-md: 0.875rem;
  --font-size-lg: 0.9375rem;
  --font-size-xl: 1.125rem;
  --font-size-2xl: 1.5rem;

  --header-height: 48px;
  --nav-width: 232px;
  --nav-rail-width: 56px;
}

:root.dark {
  --bg-primary: #151618;
  --bg-surface: #1c1d20;
  --bg-surface-hover: #26282b;
  --bg-inset: #222427;
  --border-default: #34373c;
  --border-muted: #2a2c30;

  --text-primary: #ececee;
  --text-secondary: #b8bcc3;
  --text-muted: #8f949c;

  --accent-blue: #4d9cff;
  --accent-amber: #f0a64a;
  --accent-purple: #a995f5;
  --accent-green: #4cc38a;
  --accent-red: #f27474;
  --accent-teal: #3fc1d6;

  --nav-active-bg: #243650;

  --shadow-sm: 0 1px 2px rgba(0, 0, 0, 0.3);
  --shadow-md: 0 4px 12px rgba(0, 0, 0, 0.35);
  --shadow-lg: 0 16px 40px rgba(0, 0, 0, 0.5);
  --overlay-bg: rgba(0, 0, 0, 0.55);
}

/* Repeats kit-ui's handheld type scale, which the base block above would
 * otherwise override by source order. */
@media (hover: none) and (pointer: coarse) {
  :root {
    --font-size-2xs: 0.75rem;
    --font-size-xs: 0.875rem;
    --font-size-sm: 0.9375rem;
    --font-size-md: 1rem;
    --font-size-lg: 1.0625rem;
    --font-size-xl: 1.25rem;
    --font-size-2xl: 1.75rem;
  }
}
```

- [ ] **Step 3: Import the palette**

In `web/src/main.ts`, change the import block to:

```ts
import '@kenn-io/kit-ui/theme.css';
import '../palette.css';
import './app.css';
```

In `web/src/app.css`, delete the line `@import '@kenn-io/kit-ui/theme.css';`,
because `main.ts` now imports it first.

- [ ] **Step 4: Update the type rules in `web/src/styles/tokens.css`**

1. Delete the `--text-muted: color-mix(...)` override and its comment. The
   palette's `--text-muted` meets WCAG AA on both surfaces.
2. Replace the `[data-mono]` block and its comment with:

```css
/* Data values (timestamps, counts, sizes) keep the UI typeface with tabular
 * figures so columns align. Identifiers use <code> or [data-metadata]. */
:where([data-mono]) {
  font-variant-numeric: tabular-nums;
}
```

3. Replace the `[data-section-label]` block and its comment with:

```css
/* Pane headers, month markers, and group headings: small sentence-case
 * labels, never uppercase. */
:where([data-section-label]) {
  color: var(--text-muted);
  font-family: var(--font-sans);
  font-size: var(--font-size-xs);
  font-weight: 600;
}
```

- [ ] **Step 5: Move identifiers off `[data-mono]`**

Run `rg -n 'data-mono' src/lib --glob '*.svelte'`. For each hit, decide:
- The value is an ID, hash, raw code, email address in a code-like position,
  or cron text: replace `data-mono` with `data-metadata`.
- The value is a timestamp, count, duration, or size: keep `data-mono`.

List the files you changed in the commit body.

- [ ] **Step 6: Remove uppercase styling**

In each file listed under **Files**, delete `text-transform: uppercase;` and
any `letter-spacing` on the same rule. Where the rule also sets
`font-weight: 800`, change it to `600`. In `OperationsWorkspace.svelte:377` and
`OperationRelatedStatus.svelte:167` also change
`color: var(--status-warning-ink)` to `color: var(--text-muted)`, because
eyebrows are not warnings. Task 10 removes those eyebrows entirely.

- [ ] **Step 7: Add kit overrides for treatments kit has no token for**

Append to `web/src/app.css`:

```css
/* kit-ui has no tokens for these treatments. If kit renames the classes,
 * these overrides stop applying; check them when bumping the kit pin. */
.kit-chip {
  text-transform: none;
  letter-spacing: 0;
}

.kit-table th,
.kit-table-header-cell {
  text-transform: none;
  letter-spacing: 0;
  color: var(--text-muted);
  font-weight: 600;
}
```

Before committing, confirm each selector exists in kit with
`rg -n 'kit-chip\b|kit-table th|kit-table-header-cell' node_modules/@kenn-io/kit-ui/src/lib/components`.
Delete any rule whose selector kit does not use.

- [ ] **Step 8: Verify**

Run: `bun run check && bun run check:kit-ui && bun run build`
Expected: all pass with no warnings.

Capture the ten workspaces again into `after-task1/` and compare with
`before/`. Nothing should be uppercase, and no text should fall below AA
contrast.

- [ ] **Step 9: Commit**

Subject: `feat(web): adopt the quiet kenn palette and sentence-case type`.

---

### Task 2: Document Shift+Space in the shortcut registry

The Everything footer is the only place that documents Shift+Space
(extending a selection). Task 9 removes the footer, so the registry must
document it first.

**Files:**
- Modify: `web/src/lib/commands/registry.ts`
- Modify: `web/src/lib/components/shell/AppShell.svelte` (`relay`,
  `relayGridKey`, `commandHandlers`)
- Test: `web/src/lib/components/shell/KeyboardHelp.test.ts`,
  `web/src/lib/components/shell/AppShell.test.ts`

**Interfaces:**
- Produces: command id `extend-selection`, label
  `Extend selection to focused row`, keys `['Shift', 'Space']`, combo
  `shift+space`, section `Selection`.

- [ ] **Step 1: Write the failing tests**

Add to `KeyboardHelp.test.ts`:

```ts
it('documents extending a selection with Shift+Space', () => {
  const handlers = Object.fromEntries(
    COMMAND_DEFINITIONS.map(({ id }) => [id, () => undefined])
  ) as CommandHandlers;
  render(KeyboardHelp, { commands: createCommandRegistry(handlers), onclose: () => undefined });

  expect(screen.getByText('Extend selection to focused row')).toBeTruthy();
});
```

Import `COMMAND_DEFINITIONS`, `createCommandRegistry`, and `CommandHandlers`
from `../../commands/registry` if the file does not already.

Add to `AppShell.test.ts`, next to the existing selection-shortcut tests:

```ts
it('extends a selection with Shift+Space pressed outside the grid', async () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const state = new ExploreState(window);
  const rows = [0, 1, 2].map((index) => entry(index));
  render(AppShell, {
    client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse({ rows, total_count: 3 })))),
    state
  });
  const grid = await screen.findByRole('grid', { name: 'Everything results' });
  grid.focus();
  await fireEvent.keyDown(grid, { key: ' ' });
  await fireEvent.keyDown(grid, { key: 'j' });
  await fireEvent.keyDown(grid, { key: 'j' });
  document.body.focus();

  await fireEvent.keyDown(window, { key: ' ', shiftKey: true });

  await waitFor(() => expect(screen.getByText('3 selected')).toBeTruthy());
  state.destroy();
});
```

`entry(index)` is the existing row fixture in `AppShell.test.ts`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/shell/KeyboardHelp.test.ts src/lib/components/shell/AppShell.test.ts -t "Shift"`
Expected: FAIL. The dialog lacks the label, and the selection stays at 1.

- [ ] **Step 3: Add the command**

In `registry.ts`, after the `toggle-selection` entry, add:

```ts
  command('extend-selection', 'Extend selection to focused row', ['Shift', 'Space'], ['shift+space'], 'Selection'),
```

In `AppShell.svelte`, let `relay` and `relayGridKey` carry modifier keys:

```ts
function relayGridKey(event: KeyboardEvent, key: string, init: KeyboardEventInit = {}): void {
  if (event.target instanceof Element && event.target.closest('button, a, summary, [role="button"]')) return;
  const grid = currentGrid();
  if (!grid || event.target === grid) return;
  grid.focus();
  grid.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: false, cancelable: true, ...init }));
}
```

```ts
function relay(event: KeyboardEvent | undefined, key: string | undefined = undefined, init: KeyboardEventInit = {}): void {
  const resolvedKey = key ?? event?.key;
  if (!resolvedKey) return;
  if (event) {
    relayGridKey(event, resolvedKey, init);
    return;
  }
  queueMicrotask(() => {
    const grid = currentGrid();
    if (!grid) return;
    grid.focus();
    grid.dispatchEvent(new KeyboardEvent('keydown', { key: resolvedKey, bubbles: false, cancelable: true, ...init }));
  });
}
```

Add the handler to `commandHandlers`:

```ts
'extend-selection': (event) => relay(event, ' ', { shiftKey: true }),
```

- [ ] **Step 4: Run the tests to verify they pass**

Run the command from Step 2.
Expected: PASS. Then run `bunx vitest run src/lib/commands src/lib/components/shell`
and confirm nothing else regressed.

- [ ] **Step 5: Commit**

Subject: `feat(web): list Shift+Space in the keyboard shortcuts dialog`.

---

### Task 3: One history entry for a search that changes workspace

**Files:**
- Modify: `web/src/lib/explore/state.svelte.ts`
- Test: `web/src/lib/explore/state.test.ts`

**Interfaces:**
- Produces:
  `ExploreState.commitSearchIn(workspace: ExploreWorkspace, query: string, searchMode: ExploreSearchMode): void`.
  It pushes one history entry that sets the workspace, query, and mode. It
  clears the same transient fields as `commitWorkspace`, and it remembers the
  mode like `commitSearch`.

- [ ] **Step 1: Write the failing test**

```ts
it('commits a search into another workspace as one history entry', () => {
  window.history.replaceState(null, '', '/?workspace=sources&mode=full_text');
  const state = new ExploreState(window);
  const before = window.history.length;

  state.commitSearchIn('everything', 'quarterly report', 'hybrid');

  expect(window.history.length).toBe(before + 1);
  expect(state.current.workspace).toBe('everything');
  expect(state.current.query).toBe('quarterly report');
  expect(state.current.searchMode).toBe('hybrid');
  expect(state.current.selectedRow).toBeNull();
  state.destroy();
});
```

The existing state tests drive the jsdom `window` directly, as this test does.

- [ ] **Step 2: Run the test to verify it fails**

Run: `bunx vitest run src/lib/explore -t "one history entry"`
Expected: FAIL with `state.commitSearchIn is not a function`.

- [ ] **Step 3: Implement**

Add after `commitWorkspace` in `state.svelte.ts`:

```ts
  commitSearchIn(workspace: ExploreWorkspace, query: string, searchMode: ExploreSearchMode): void {
    rememberSearchMode(searchMode, this.preferenceStorage);
    this.navigate({
      workspace,
      query,
      searchMode,
      analysisTarget: null,
      selectedIdentifier: null,
      activeRow: null,
      selectedRow: null,
      conversationAnchor: null,
      scrollAnchor: null,
      operationStatus: '',
      settingsAuthority: ''
    }, 'push');
  }
```

- [ ] **Step 4: Run the test to verify it passes**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

Subject: `feat(web): commit a search into another workspace in one step`.

---

### Task 4: Global search form

`search/SearchBar.svelte` is imported only by its own test. It becomes the
top-bar search. It keeps today's Everything names: form "Search Everything",
searchbox "Search everything", radiogroup "Search mode", button "Search".

**Files:**
- Modify: `web/src/lib/components/search/SearchBar.svelte`
- Test: `web/src/lib/components/search/SearchBar.test.ts` (replace its
  contents)

**Interfaces:**
- Consumes: `SearchModeControl` (unchanged), `ExploreSearchMode`.
- Produces: `SearchBar` props:
  - `query: string` — committed query from explore state
  - `mode: ExploreSearchMode`
  - `live: boolean` — true on Everything and Files
  - `compact: boolean` — true below 900px
  - `onDraft: (query: string, mode: ExploreSearchMode) => void` — called while
    typing or changing mode, only when `live`
  - `onSubmit: (query: string, mode: ExploreSearchMode) => void`
  - `inputEl?: HTMLInputElement` — `$bindable`, for `/` and "Refine search"

- [ ] **Step 1: Write the failing tests**

Replace `SearchBar.test.ts` with:

```ts
import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import SearchBar from './SearchBar.svelte';

function setup(live: boolean) {
  const onDraft = vi.fn();
  const onSubmit = vi.fn();
  render(SearchBar, { query: 'budget', mode: 'full_text', live, compact: false, onDraft, onSubmit });
  return { onDraft, onSubmit, input: screen.getByRole('searchbox', { name: 'Search everything' }) };
}

describe('SearchBar', () => {
  it('reports each keystroke when live', async () => {
    const { onDraft, input } = setup(true);
    await fireEvent.input(input, { target: { value: 'budget q3' } });
    expect(onDraft).toHaveBeenLastCalledWith('budget q3', 'full_text');
  });

  it('keeps typing local when not live and submits the draft', async () => {
    const { onDraft, onSubmit, input } = setup(false);
    await fireEvent.input(input, { target: { value: '  merger  ' } });
    await fireEvent.click(screen.getByRole('radio', { name: 'Hybrid' }));
    await fireEvent.submit(screen.getByRole('search', { name: 'Search Everything' }));
    expect(onDraft).not.toHaveBeenCalled();
    expect(onSubmit).toHaveBeenCalledWith('merger', 'hybrid');
  });

  it('shows the committed query again when it changes', async () => {
    const { input } = setup(false);
    expect((input as HTMLInputElement).value).toBe('budget');
  });

  it('offers the search mode as a select when compact', () => {
    render(SearchBar, { query: '', mode: 'semantic', live: true, compact: true, onDraft: vi.fn(), onSubmit: vi.fn() });
    expect(screen.getByRole('combobox', { name: /^Search mode:/ })).toBeTruthy();
    expect(screen.queryByRole('radiogroup', { name: 'Search mode' })).toBeNull();
  });
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/search/SearchBar.test.ts`
Expected: FAIL, because the current component uses different names and props.

- [ ] **Step 3: Implement**

Replace `SearchBar.svelte` with:

```svelte
<script lang="ts">
  import { Button, SearchInput, SelectDropdown } from '@kenn-io/kit-ui';

  import type { ExploreSearchMode } from '../../explore/models';
  import SearchModeControl from './SearchModeControl.svelte';

  interface Props {
    query: string;
    mode: ExploreSearchMode;
    live: boolean;
    compact: boolean;
    onDraft: (query: string, mode: ExploreSearchMode) => void;
    onSubmit: (query: string, mode: ExploreSearchMode) => void;
    inputEl?: HTMLInputElement;
  }

  let { query, mode, live, compact, onDraft, onSubmit, inputEl = $bindable() }: Props = $props();

  let draft = $state('');
  let draftMode = $state<ExploreSearchMode>('full_text');

  $effect(() => {
    draft = query;
    draftMode = mode;
  });

  const modeOptions = [
    { value: 'full_text', label: 'Full text' },
    { value: 'semantic', label: 'Semantic' },
    { value: 'hybrid', label: 'Hybrid' }
  ];

  function changeQuery(value: string): void {
    draft = value;
    if (live) onDraft(value, draftMode);
  }

  function changeMode(value: ExploreSearchMode): void {
    draftMode = value;
    if (live) onDraft(draft, value);
  }

  function submit(event: SubmitEvent): void {
    event.preventDefault();
    onSubmit(draft.trim(), draftMode);
  }
</script>

<form class="global-search" class:global-search--compact={compact} role="search"
  aria-label="Search Everything" onsubmit={submit}>
  <div class="global-search__query">
    <SearchInput
      id="everything-search"
      bind:inputEl
      value={draft}
      ariaLabel="Search everything"
      placeholder="Search people, conversations, events, and files…"
      block
      oninput={changeQuery}
    />
  </div>
  {#if compact}
    <SelectDropdown
      title="Search mode"
      value={draftMode}
      options={modeOptions}
      align="end"
      onchange={(value) => changeMode(value as ExploreSearchMode)}
    />
  {:else}
    <SearchModeControl requestedMode={draftMode} onchange={changeMode} />
    <Button type="submit" label="Search" tone="info" surface="solid" />
  {/if}
</form>

<style>
  .global-search {
    display: flex;
    min-width: 0;
    flex: 1;
    align-items: center;
    gap: var(--space-3);
  }

  .global-search__query {
    min-width: 0;
    flex: 1;
    max-width: 640px;
  }

  .global-search--compact {
    gap: var(--space-2);
  }
</style>
```

If a kit component's prop name differs from the one used here (`bind:inputEl`,
`oninput`), match the usage in the current `EverythingWorkspace.svelte`
search form. That usage is known to work.

- [ ] **Step 4: Run the tests to verify they pass**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

Subject: `feat(web): turn SearchBar into the global search form`.

---

### Task 5: Sidebar navigation

**Files:**
- Create: `web/src/lib/components/shell/navigation.ts`
- Create: `web/src/lib/components/shell/AppSidebar.svelte`
- Test: `web/src/lib/components/shell/AppSidebar.test.ts`

**Interfaces:**
- Produces from `navigation.ts`:

```ts
export interface NavigationItem { id: ExploreWorkspace; label: string; icon: Component }
export interface NavigationGroup { label: string; items: NavigationItem[] }
export const NAVIGATION_GROUPS: NavigationGroup[];
export function workspaceLabel(id: ExploreWorkspace): string;
export const SIDEBAR_COLLAPSED_KEY = 'msgvault.sidebar.collapsed';
```

- Produces the `AppSidebar` props:
  - `active: ExploreWorkspace`
  - `collapsed: boolean` — rail mode; the drawer always passes `false`
  - `showCollapseToggle: boolean` — false inside the drawer
  - `status: { tone: 'working' | 'idle' | 'unclean'; label: string; text: string }`
  - `onNavigate: (id: ExploreWorkspace) => void`
  - `onToggleCollapsed: () => void`
  - `onOpenShortcuts: () => void`

- [ ] **Step 1: Write the failing tests**

```ts
import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import AppSidebar from './AppSidebar.svelte';

const status = { tone: 'idle' as const, label: 'Local archive ready', text: 'Local archive' };

function renderSidebar(overrides: Record<string, unknown> = {}) {
  const props = {
    active: 'everything' as const, collapsed: false, showCollapseToggle: true, status,
    onNavigate: vi.fn(), onToggleCollapsed: vi.fn(), onOpenShortcuts: vi.fn(), ...overrides
  };
  render(AppSidebar, props);
  return props;
}

describe('AppSidebar', () => {
  it('lists workspaces in People, Archive, Manage order with the active one current', () => {
    renderSidebar();
    const nav = screen.getByRole('navigation', { name: 'Primary' });
    expect(within(nav).getAllByRole('button').map((b) => b.getAttribute('aria-label') ?? b.textContent?.trim()))
      .toEqual(['Relationships', 'Directory', 'Reviews', 'Everything', 'Files', 'Saved views',
        'Sources', 'Operations', 'Deletions', 'Settings']);
    expect(within(nav).getByRole('button', { name: 'Everything' }).getAttribute('aria-current')).toBe('page');
    expect(within(nav).getByRole('button', { name: 'Files' }).hasAttribute('aria-current')).toBe(false);
  });

  it('navigates when an item is chosen', async () => {
    const props = renderSidebar();
    await fireEvent.click(screen.getByRole('button', { name: 'Deletions' }));
    expect(props.onNavigate).toHaveBeenCalledWith('deletions');
  });

  it('keeps full accessible names in the icon rail and hides group headings', () => {
    renderSidebar({ collapsed: true });
    expect(screen.getByRole('button', { name: 'Saved views' })).toBeTruthy();
    expect(screen.queryByText('People')).toBeNull();
  });

  it('toggles the rail and opens shortcuts from the footer', async () => {
    const props = renderSidebar();
    await fireEvent.click(screen.getByRole('button', { name: 'Collapse sidebar' }));
    await fireEvent.click(screen.getByRole('button', { name: /Keyboard shortcuts/ }));
    expect(props.onToggleCollapsed).toHaveBeenCalled();
    expect(props.onOpenShortcuts).toHaveBeenCalled();
    expect(screen.getByText('Local archive')).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/shell/AppSidebar.test.ts`
Expected: FAIL, because the module does not exist.

- [ ] **Step 3: Create `navigation.ts`**

```ts
import type { Component } from 'svelte';
import Activity from '@lucide/svelte/icons/activity';
import Bookmark from '@lucide/svelte/icons/bookmark';
import CheckCheck from '@lucide/svelte/icons/check-check';
import Contact from '@lucide/svelte/icons/contact';
import Inbox from '@lucide/svelte/icons/inbox';
import Paperclip from '@lucide/svelte/icons/paperclip';
import Plug from '@lucide/svelte/icons/plug';
import Settings from '@lucide/svelte/icons/settings';
import Trash2 from '@lucide/svelte/icons/trash-2';
import Users from '@lucide/svelte/icons/users';

import type { ExploreWorkspace } from '../../explore/models';

export interface NavigationItem {
  id: ExploreWorkspace;
  label: string;
  icon: Component;
}

export interface NavigationGroup {
  label: string;
  items: NavigationItem[];
}

export const SIDEBAR_COLLAPSED_KEY = 'msgvault.sidebar.collapsed';

export const NAVIGATION_GROUPS: NavigationGroup[] = [
  {
    label: 'People',
    items: [
      { id: 'relationships', label: 'Relationships', icon: Users },
      { id: 'directory', label: 'Directory', icon: Contact },
      { id: 'directory_review', label: 'Reviews', icon: CheckCheck }
    ]
  },
  {
    label: 'Archive',
    items: [
      { id: 'everything', label: 'Everything', icon: Inbox },
      { id: 'files', label: 'Files', icon: Paperclip },
      { id: 'saved_views', label: 'Saved views', icon: Bookmark }
    ]
  },
  {
    label: 'Manage',
    items: [
      { id: 'sources', label: 'Sources', icon: Plug },
      { id: 'operations', label: 'Operations', icon: Activity },
      { id: 'deletions', label: 'Deletions', icon: Trash2 },
      { id: 'settings', label: 'Settings', icon: Settings }
    ]
  }
];

export function workspaceLabel(id: ExploreWorkspace): string {
  for (const group of NAVIGATION_GROUPS) {
    const item = group.items.find((candidate) => candidate.id === id);
    if (item) return item.label;
  }
  return 'msgvault';
}
```

- [ ] **Step 4: Create `AppSidebar.svelte`**

```svelte
<script lang="ts">
  import { KbdBadge, StatusDot, Tooltip } from '@kenn-io/kit-ui';
  import Keyboard from '@lucide/svelte/icons/keyboard';
  import PanelLeftClose from '@lucide/svelte/icons/panel-left-close';
  import PanelLeftOpen from '@lucide/svelte/icons/panel-left-open';

  import type { ExploreWorkspace } from '../../explore/models';
  import { NAVIGATION_GROUPS } from './navigation';

  interface Props {
    active: ExploreWorkspace;
    collapsed: boolean;
    showCollapseToggle: boolean;
    status: { tone: 'working' | 'idle' | 'unclean'; label: string; text: string };
    onNavigate: (id: ExploreWorkspace) => void;
    onToggleCollapsed: () => void;
    onOpenShortcuts: () => void;
  }

  let { active, collapsed, showCollapseToggle, status, onNavigate, onToggleCollapsed, onOpenShortcuts }: Props = $props();
</script>

<div class="sidebar" class:sidebar--rail={collapsed}>
  <div class="sidebar__brand">msgvault</div>
  <nav aria-label="Primary">
    {#each NAVIGATION_GROUPS as group, index (group.label)}
      {#if collapsed}
        {#if index > 0}<hr class="sidebar__divider" />{/if}
      {:else}
        <h2 class="sidebar__group">{group.label}</h2>
      {/if}
      <ul>
        {#each group.items as item (item.id)}
          {@const Icon = item.icon}
          <li>
            {#if collapsed}
              <Tooltip text={item.label}>
                <button type="button" class="sidebar__item" aria-label={item.label}
                  aria-current={item.id === active ? 'page' : undefined} onclick={() => onNavigate(item.id)}>
                  <Icon size={18} aria-hidden="true" />
                </button>
              </Tooltip>
            {:else}
              <button type="button" class="sidebar__item"
                aria-current={item.id === active ? 'page' : undefined} onclick={() => onNavigate(item.id)}>
                <Icon size={18} aria-hidden="true" /><span>{item.label}</span>
              </button>
            {/if}
          </li>
        {/each}
      </ul>
    {/each}
  </nav>
  <div class="sidebar__footer">
    <span class="sidebar__status" title={status.label}>
      <StatusDot status={status.tone} label={status.label} />
      {#if !collapsed}<span>{status.text}</span>{/if}
    </span>
    <button type="button" class="sidebar__item" aria-label="Keyboard shortcuts" onclick={onOpenShortcuts}>
      <Keyboard size={18} aria-hidden="true" />
      {#if !collapsed}<span>Keyboard shortcuts</span><KbdBadge keys={['?']} />{/if}
    </button>
    {#if showCollapseToggle}
      <button type="button" class="sidebar__item" aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
        onclick={onToggleCollapsed}>
        {#if collapsed}<PanelLeftOpen size={18} aria-hidden="true" />{:else}<PanelLeftClose size={18} aria-hidden="true" /><span>Collapse</span>{/if}
      </button>
    {/if}
  </div>
</div>

<style>
  .sidebar {
    display: flex;
    width: var(--nav-width);
    height: 100%;
    flex-direction: column;
    gap: var(--space-3);
    padding: var(--space-3) var(--space-2);
    overflow-y: auto;
    background: var(--bg-surface);
    border-right: 1px solid var(--border-default);
  }

  .sidebar--rail {
    width: var(--nav-rail-width);
    align-items: center;
  }

  .sidebar__brand {
    padding: var(--space-1) var(--space-3);
    color: var(--text-primary);
    font-size: var(--font-size-md);
    font-weight: 650;
  }

  .sidebar--rail .sidebar__brand {
    visibility: hidden;
  }

  nav {
    display: flex;
    flex: 1;
    flex-direction: column;
    gap: var(--space-1);
  }

  ul {
    display: flex;
    flex-direction: column;
    gap: 2px;
    margin: 0;
    padding: 0;
    list-style: none;
  }

  .sidebar__group {
    margin: var(--space-3) 0 var(--space-1);
    padding: 0 var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
    font-weight: 600;
  }

  .sidebar__divider {
    width: 24px;
    margin: var(--space-2) auto;
    border: 0;
    border-top: 1px solid var(--border-default);
  }

  .sidebar__item {
    display: flex;
    width: 100%;
    min-height: 32px;
    align-items: center;
    gap: var(--space-3);
    padding: 0 var(--space-3);
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--text-secondary);
    font-size: var(--font-size-md);
    text-align: left;
    cursor: pointer;
  }

  .sidebar--rail .sidebar__item {
    width: 36px;
    justify-content: center;
    padding: 0;
  }

  .sidebar__item:hover {
    background: var(--bg-surface-hover);
    color: var(--text-primary);
  }

  .sidebar__item[aria-current='page'] {
    background: var(--nav-active-bg);
    color: var(--text-primary);
    font-weight: 600;
  }

  .sidebar__item :global(.kit-kbd) {
    margin-left: auto;
  }

  .sidebar__footer {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding-top: var(--space-2);
    border-top: 1px solid var(--border-muted);
  }

  .sidebar__status {
    display: flex;
    min-height: 28px;
    align-items: center;
    gap: var(--space-2);
    padding: 0 var(--space-3);
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }
</style>
```

Check the `KbdBadge` class name with
`rg -n 'class="kit-kbd' node_modules/@kenn-io/kit-ui/src/lib/components/KbdBadge.svelte`
and adjust the `:global()` selector to match.

- [ ] **Step 5: Run the tests to verify they pass**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 6: Commit**

Subject: `feat(web): add a grouped sidebar with an icon rail`.

---

### Task 6: Narrow-screen navigation drawer

**Files:**
- Create: `web/src/lib/components/shell/NavigationDrawer.svelte`
- Test: `web/src/lib/components/shell/NavigationDrawer.test.ts`

**Interfaces:**
- Produces the `NavigationDrawer` props: `onclose: () => void` and
  `children: Snippet`. The parent mounts the drawer only while it is open.
- Behavior contract:
  - Mounting pushes the shortcut scope `navigation-drawer`. That suspends all
    root shortcuts, including `close-layer`, so Escape cannot also close the
    reading pane.
  - Escape, registered in that scope, calls `onclose`.
  - Selecting the scrim calls `onclose`.
  - On mount, focus moves to the drawer's `[aria-current="page"]` button. kit
    `trapFocus` keeps Tab inside and, on unmount, restores focus to the element
    that had it before, which is the "Open navigation" button.
  - Choosing an item also restores focus to "Open navigation". The spec's
    "workspace-change focus rules" leave focus on the navigation control that
    was used; in the drawer that control is the menu button.

- [ ] **Step 1: Write the failing tests**

```ts
import { fireEvent, render, screen } from '@testing-library/svelte';
import { appShortcuts } from '@kenn-io/kit-ui';
import { createRawSnippet } from 'svelte';
import { describe, expect, it, vi } from 'vitest';

import NavigationDrawer from './NavigationDrawer.svelte';

const items = createRawSnippet(() => ({
  render: () => '<nav aria-label="Primary"><button>Files</button><button aria-current="page">Everything</button></nav>'
}));

describe('NavigationDrawer', () => {
  it('focuses the current item and closes on Escape without reaching root shortcuts', async () => {
    const root = vi.fn();
    const unregister = appShortcuts.register('escape', root);
    const onclose = vi.fn();
    render(NavigationDrawer, { onclose, children: items });

    expect(document.activeElement).toBe(screen.getByRole('button', { name: 'Everything' }));
    await fireEvent.keyDown(document.activeElement!, { key: 'Escape' });

    expect(onclose).toHaveBeenCalledOnce();
    expect(root).not.toHaveBeenCalled();
    unregister();
  });

  it('closes when the scrim is selected', async () => {
    const onclose = vi.fn();
    render(NavigationDrawer, { onclose, children: items });
    await fireEvent.click(screen.getByRole('button', { name: 'Close navigation' }));
    expect(onclose).toHaveBeenCalledOnce();
  });

  it('returns focus to the opener when unmounted', () => {
    const opener = document.createElement('button');
    document.body.append(opener);
    opener.focus();
    const rendered = render(NavigationDrawer, { onclose: vi.fn(), children: items });
    rendered.unmount();
    expect(document.activeElement).toBe(opener);
    opener.remove();
  });
});
```

The first test relies on `initShortcuts` having attached kit's keydown
listener. If it is not attached in unit tests, call `initShortcuts()` in a
`beforeAll` and its returned cleanup in `afterAll`, as `AppShell.svelte` does.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/shell/NavigationDrawer.test.ts`
Expected: FAIL, because the module does not exist.

- [ ] **Step 3: Implement**

```svelte
<script lang="ts">
  import { appShortcuts, trapFocus } from '@kenn-io/kit-ui';
  import { onMount, tick, type Snippet } from 'svelte';

  let { onclose, children }: { onclose: () => void; children: Snippet } = $props();
  let panel = $state<HTMLElement>();

  onMount(() => {
    const popScope = appShortcuts.pushScope('navigation-drawer');
    const unregister = appShortcuts.register('escape', () => onclose(), { scope: 'navigation-drawer' });
    void tick().then(() => panel?.querySelector<HTMLElement>('[aria-current="page"]')?.focus());
    return () => {
      unregister();
      popScope();
    };
  });
</script>

<div class="drawer">
  <button type="button" class="drawer__scrim" aria-label="Close navigation" onclick={onclose}></button>
  <div class="drawer__panel" role="dialog" aria-modal="true" aria-label="Navigation" tabindex="-1"
    bind:this={panel} {@attach trapFocus}>
    {@render children()}
  </div>
</div>

<style>
  .drawer {
    position: fixed;
    inset: 0;
    z-index: 50;
  }

  .drawer__scrim {
    position: absolute;
    inset: 0;
    border: 0;
    background: var(--overlay-bg);
  }

  .drawer__panel {
    position: absolute;
    inset: 0 auto 0 0;
    height: 100%;
    box-shadow: var(--shadow-lg);
  }
</style>
```

`trapFocus` runs its initial focus before `tick()` resolves. The `tick()`
callback then moves focus to the current item, which is inside the trap.

- [ ] **Step 4: Run the tests to verify they pass**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

Subject: `feat(web): add the narrow-screen navigation drawer`.

---

### Task 7: Display menu

**Files:**
- Create: `web/src/lib/components/shell/DisplayMenu.svelte`
- Test: `web/src/lib/components/shell/DisplayMenu.test.ts`

**Interfaces:**
- Produces the `DisplayMenu` props:
  - `density: 'daemon' | 'compact' | 'comfortable'` — `'daemon'` means no
    override
  - `themeOverridden: boolean`
  - `onDensityChange: (value: 'daemon' | 'compact' | 'comfortable') => void`
  - `onUseDaemonTheme: () => void`
- Accessible names: trigger button "Display"; radio group "Temporary
  density" with radios "Auto", "Compact", and "Comfortable"; menu item "Use
  daemon theme", shown only when `themeOverridden`.

- [ ] **Step 1: Write the failing tests**

```ts
import { fireEvent, render, screen } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';

import DisplayMenu from './DisplayMenu.svelte';

describe('DisplayMenu', () => {
  it('sets and clears the per-tab density override', async () => {
    const onDensityChange = vi.fn();
    render(DisplayMenu, { density: 'daemon', themeOverridden: false, onDensityChange, onUseDaemonTheme: vi.fn() });
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));

    const group = screen.getByRole('group', { name: 'Temporary density' });
    expect(group).toBeTruthy();
    await fireEvent.click(screen.getByRole('menuitemradio', { name: 'Comfortable' }));
    expect(onDensityChange).toHaveBeenCalledWith('comfortable');
    expect(screen.queryByRole('menuitem', { name: 'Use daemon theme' })).toBeNull();
  });

  it('offers the daemon theme only while a theme override is active', async () => {
    const onUseDaemonTheme = vi.fn();
    render(DisplayMenu, { density: 'compact', themeOverridden: true, onDensityChange: vi.fn(), onUseDaemonTheme });
    await fireEvent.click(screen.getByRole('button', { name: 'Display' }));
    await fireEvent.click(screen.getByRole('menuitem', { name: 'Use daemon theme' }));
    expect(onUseDaemonTheme).toHaveBeenCalledOnce();
  });
});
```

Check the roles kit renders with
`rg -n 'role=' node_modules/@kenn-io/kit-ui/src/lib/components/MenuRadio*.svelte node_modules/@kenn-io/kit-ui/src/lib/components/MenuItem.svelte`.
If the group role is `radiogroup`, use that in the test.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/shell/DisplayMenu.test.ts`
Expected: FAIL, because the module does not exist.

- [ ] **Step 3: Implement**

```svelte
<script lang="ts">
  import { Menu, MenuContent, MenuItem, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuTrigger } from '@kenn-io/kit-ui';
  import SlidersHorizontal from '@lucide/svelte/icons/sliders-horizontal';

  type Density = 'daemon' | 'compact' | 'comfortable';

  interface Props {
    density: Density;
    themeOverridden: boolean;
    onDensityChange: (value: Density) => void;
    onUseDaemonTheme: () => void;
  }

  let { density, themeOverridden, onDensityChange, onUseDaemonTheme }: Props = $props();
</script>

<Menu align="end">
  <MenuTrigger ariaLabel="Display" title="Display">
    <SlidersHorizontal size={18} aria-hidden="true" />
  </MenuTrigger>
  <MenuContent ariaLabel="Display">
    <MenuRadioGroup ariaLabel="Temporary density" value={density} onchange={(value) => onDensityChange(value as Density)}>
      <MenuRadioItem value="daemon">Auto</MenuRadioItem>
      <MenuRadioItem value="compact">Compact</MenuRadioItem>
      <MenuRadioItem value="comfortable">Comfortable</MenuRadioItem>
    </MenuRadioGroup>
    {#if themeOverridden}
      <MenuSeparator />
      <MenuItem onselect={onUseDaemonTheme}>Use daemon theme</MenuItem>
    {/if}
  </MenuContent>
</Menu>
```

- [ ] **Step 4: Run the tests to verify they pass**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

Subject: `feat(web): move per-tab display overrides into a menu`.

---

### Task 8: Page header

**Files:**
- Create: `web/src/lib/components/shell/PageHeader.svelte`
- Test: `web/src/lib/components/shell/PageHeader.test.ts`

**Interfaces:**
- Produces the `PageHeader` props:
  - `title: string`
  - `description?: string`
  - `actions?: Snippet`
  - `view?: Snippet` — a row below the title for view switches
  - `visuallyHiddenTitle?: boolean` — defaults to false. It exists only for
    the narrow Operations detail view, which renders its own heading.

- [ ] **Step 1: Write the failing test**

```ts
import { render, screen } from '@testing-library/svelte';
import { createRawSnippet } from 'svelte';
import { describe, expect, it } from 'vitest';

import PageHeader from './PageHeader.svelte';

describe('PageHeader', () => {
  it('renders one level-one heading, the description, and actions', () => {
    const actions = createRawSnippet(() => ({ render: () => '<button>Refresh operations</button>' }));
    render(PageHeader, { title: 'Operations', description: 'Background work and its history.', actions });

    expect(screen.getByRole('heading', { level: 1, name: 'Operations' })).toBeTruthy();
    expect(screen.getByText('Background work and its history.')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Refresh operations' })).toBeTruthy();
  });
});
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `bunx vitest run src/lib/components/shell/PageHeader.test.ts`
Expected: FAIL, because the module does not exist.

- [ ] **Step 3: Implement**

```svelte
<script lang="ts">
  import type { Snippet } from 'svelte';

  interface Props {
    title: string;
    description?: string;
    actions?: Snippet;
    view?: Snippet;
    visuallyHiddenTitle?: boolean;
  }

  let { title, description, actions, view, visuallyHiddenTitle = false }: Props = $props();
</script>

<header class="page-header">
  <div class="page-header__row">
    <div class="page-header__text" class:kit-sr-only={visuallyHiddenTitle}>
      <h1>{title}</h1>
      {#if description}<p>{description}</p>{/if}
    </div>
    {#if actions}<div class="page-header__actions">{@render actions()}</div>{/if}
  </div>
  {#if view}<div class="page-header__view">{@render view()}</div>{/if}
</header>

<style>
  .page-header {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
  }

  .page-header__row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3) var(--space-6);
  }

  .page-header__text {
    min-width: 0;
  }

  h1 {
    margin: 0;
    color: var(--text-primary);
    font-size: var(--font-size-xl);
    font-weight: 650;
    line-height: 1.25;
  }

  p {
    margin: var(--space-1) 0 0;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
  }

  .page-header__actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
  }
</style>
```

- [ ] **Step 4: Run the test to verify it passes**

Run the command from Step 2. Expected: PASS.

- [ ] **Step 5: Commit**

Subject: `feat(web): add a shared page header`.

---

### Task 9: Compose the shell

This task replaces kit `TopBar` in `AppShell.svelte` with the new layout. It
moves the search form out of Everything and removes the keyboard footer and
the selection-bar key badges.

**Files:**
- Modify: `web/src/lib/components/shell/AppShell.svelte`
- Modify: `web/src/lib/components/shell/EverythingWorkspace.svelte`
- Modify: `web/src/lib/components/explore/SelectionBar.svelte`
- Modify: `web/src/App.svelte` (remove the static `<title>`)
- Test: `web/src/lib/components/shell/AppShell.test.ts`,
  `web/src/lib/components/shell/EverythingWorkspace.test.ts`

**Interfaces:**
- Consumes: `SearchBar` (Task 4), `AppSidebar`, `NAVIGATION_GROUPS`,
  `workspaceLabel`, `SIDEBAR_COLLAPSED_KEY` (Task 5), `NavigationDrawer`
  (Task 6), `DisplayMenu` (Task 7), `ExploreState.commitSearchIn` (Task 3).
- `EverythingWorkspace` changes from `bind:searchInput` to a plain prop
  `searchInput: HTMLInputElement | undefined`. "Refine search" still calls
  `searchInput?.focus()`. The `commitSearch` and `SearchCoverage` props are
  unchanged.

- [ ] **Step 1: Write the failing tests**

Add to `AppShell.test.ts`. Replace the existing test "presents the primary
navigation tabs…" with the first test below; it checks the new order and
labels.

```ts
it('groups workspaces in the sidebar with Relationships first', () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const state = new ExploreState(window);
  const rendered = render(AppShell, {
    client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))), state, enabled: false
  });
  const nav = screen.getByRole('navigation', { name: 'Primary' });
  expect(within(nav).getAllByRole('button').map((button) => button.textContent?.trim())).toEqual([
    'Relationships', 'Directory', 'Reviews', 'Everything', 'Files', 'Saved views', 'Sources', 'Operations', 'Deletions', 'Settings'
  ]);
  expect(within(nav).getByRole('button', { name: 'Everything' }).getAttribute('aria-current')).toBe('page');
  rendered.unmount();
  state.destroy();
});

it('names the browser tab after the workspace', async () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'sources' }))}`);
  const state = new ExploreState(window);
  render(AppShell, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ sources: [] }))), state, enabled: false });
  await waitFor(() => expect(document.title).toBe('Sources · msgvault'));
  await fireEvent.click(screen.getByRole('button', { name: 'Directory' }));
  await waitFor(() => expect(document.title).toBe('Directory · msgvault'));
  state.destroy();
});

it('opens Everything with the query when searching from another workspace', async () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'sources' }))}`);
  const state = new ExploreState(window);
  const length = window.history.length;
  render(AppShell, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))), state, enabled: false });

  const search = screen.getByRole('searchbox', { name: 'Search everything' });
  await fireEvent.input(search, { target: { value: 'pipeline' } });
  expect(state.current.workspace).toBe('sources');
  await fireEvent.submit(screen.getByRole('search', { name: 'Search Everything' }));

  expect(state.current.workspace).toBe('everything');
  expect(state.current.query).toBe('pipeline');
  expect(window.history.length).toBe(length + 1);
  state.destroy();
});

it('updates Everything results as the global search is typed', async () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const state = new ExploreState(window);
  render(AppShell, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))), state, enabled: false });
  await fireEvent.input(screen.getByRole('searchbox', { name: 'Search everything' }), { target: { value: 'gas' } });
  expect(state.current.query).toBe('gas');
  state.destroy();
});

it('remembers the collapsed sidebar across reloads', async () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const state = new ExploreState(window);
  const first = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
  await fireEvent.click(screen.getByRole('button', { name: 'Collapse sidebar' }));
  first.unmount();
  render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });
  expect(screen.getByRole('button', { name: 'Expand sidebar' })).toBeTruthy();
  state.destroy();
});

it('opens a modal navigation menu on narrow screens and closes it on Escape', async () => {
  window.innerWidth = 480;
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const state = new ExploreState(window);
  render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false });

  expect(screen.queryByRole('navigation', { name: 'Primary' })).toBeNull();
  const opener = screen.getByRole('button', { name: 'Open navigation' });
  await fireEvent.click(opener);
  const current = screen.getByRole('button', { name: 'Everything' });
  expect(document.activeElement).toBe(current);

  await fireEvent.keyDown(current, { key: 'Escape' });
  await waitFor(() => expect(screen.queryByRole('navigation', { name: 'Primary' })).toBeNull());
  expect(document.activeElement).toBe(opener);
  window.innerWidth = 1024;
  state.destroy();
});
```

Also update the test "focuses search with slash…". It must still pass with
the searchbox in the top bar and without the Everything search form.

In `EverythingWorkspace.test.ts`, delete the assertions that query the
Everything search form or the keyboard footer. Add:

```ts
it('leaves searching to the global search box', () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace: 'everything' }))}`);
  const state = new ExploreState(window);
  render(AppShell, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))), state, enabled: false });
  const main = screen.getByRole('main', { name: 'Everything' });
  expect(within(main).queryByRole('search')).toBeNull();
  expect(screen.getAllByRole('search')).toHaveLength(1);
  expect(screen.queryByRole('contentinfo', { name: 'Keyboard shortcuts' })).toBeNull();
  state.destroy();
});
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/shell`
Expected: the new tests FAIL.

- [ ] **Step 3: Restructure the AppShell markup**

Replace the `<TopBar …>…</TopBar>` block and the `app-shell` wrapper with this
structure. Keep every `{#if exploreState.current.workspace === …}` branch
inside `<div class="app-main">` unchanged, and keep the elements that follow
the wrapper (palette, keyboard help, viewers) as they are.

```svelte
<svelte:window bind:innerWidth={viewportWidth} />

<div class="app-shell" class:app-shell--narrow={narrow}>
  <span class="kit-sr-only" role="status" aria-label="Operation status" aria-live="polite">
    {#key operationAnnouncement.key}<span>{operationAnnouncement.message}</span>{/key}
  </span>
  {#if !narrow}
    <AppSidebar active={exploreState.current.workspace} collapsed={sidebarCollapsed} showCollapseToggle
      status={archiveStatus} onNavigate={openWorkspaceTab} onToggleCollapsed={toggleSidebar}
      onOpenShortcuts={() => { keyboardHelpOpen = true; }} />
  {/if}
  <div class="app-column" inert={drawerOpen}>
    <header class="app-top-bar">
      {#if narrow}
        <IconButton label="Open navigation" onclick={() => { drawerOpen = true; }} bind:element={drawerOpener}>
          <Menu size={18} aria-hidden="true" />
        </IconButton>
      {/if}
      <SearchBar query={exploreState.current.query} mode={exploreState.current.searchMode}
        live={exploreState.current.workspace === 'everything' || exploreState.current.workspace === 'files'}
        compact={narrow} bind:inputEl={searchInput} onDraft={(query, mode) => exploreState.replaceSearchDraft(query, mode)}
        onSubmit={submitGlobalSearch} />
      <div class="app-top-bar__end" aria-label="Appearance controls">
        <ThemeToggle />
        <DisplayMenu density={appearance.temporary.density ?? 'daemon'}
          themeOverridden={appearance.temporary.theme !== undefined}
          onDensityChange={applyTemporaryDensity} onUseDaemonTheme={() => appearance.clearTemporary('theme')} />
      </div>
    </header>
    <div class="app-main">
      <!-- existing workspace branches, unchanged -->
    </div>
  </div>
  {#if narrow && drawerOpen}
    <NavigationDrawer onclose={() => { drawerOpen = false; }}>
      <AppSidebar active={exploreState.current.workspace} collapsed={false} showCollapseToggle={false}
        status={archiveStatus} onNavigate={(id) => { drawerOpen = false; openWorkspaceTab(id); }}
        onToggleCollapsed={() => undefined} onOpenShortcuts={() => { drawerOpen = false; keyboardHelpOpen = true; }} />
    </NavigationDrawer>
  {/if}
</div>
```

Check the `IconButton` prop names with
`sed -n '/interface Props/,/}: Props/p' node_modules/@kenn-io/kit-ui/src/lib/components/IconButton.svelte`.
If it has no element binding, wrap it in a `<span bind:this>` and focus the
`button` inside. `trapFocus` restores focus to the opener either way, so the
reference is needed only if restoration fails in the browser test.

- [ ] **Step 4: Add the script state and functions**

In `AppShell.svelte`'s script:

```ts
import AppSidebar from './AppSidebar.svelte';
import DisplayMenu from './DisplayMenu.svelte';
import NavigationDrawer from './NavigationDrawer.svelte';
import SearchBar from '../search/SearchBar.svelte';
import { SIDEBAR_COLLAPSED_KEY, workspaceLabel } from './navigation';
import Menu from '@lucide/svelte/icons/menu';
```

Add `IconButton` to the kit-ui import. Remove `TopBar`, `SelectDropdown`,
`StatusDot`, `Button` (if no longer used), and the `tabs` and
`densityOptions` constants.

```ts
const NARROW_WIDTH = 900;
let viewportWidth = $state(typeof window === 'undefined' ? 1280 : window.innerWidth);
const narrow = $derived(viewportWidth < NARROW_WIDTH);
let drawerOpen = $state(false);
let drawerOpener = $state<HTMLElement>();
let sidebarCollapsed = $state(readSidebarCollapsed());

function readSidebarCollapsed(): boolean {
  try {
    return localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === 'true';
  } catch {
    return false;
  }
}

function toggleSidebar(): void {
  sidebarCollapsed = !sidebarCollapsed;
  try {
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, String(sidebarCollapsed));
  } catch {
    // Storage may be disabled; the rail still toggles for this page view.
  }
}

$effect(() => {
  if (!narrow) drawerOpen = false;
});

$effect(() => {
  document.title = `${workspaceLabel(exploreState.current.workspace)} · msgvault`;
});

const archiveStatus = $derived(
  loader.loading
    ? { tone: 'working' as const, label: 'Searching', text: 'Searching' }
    : loader.error || loader.unavailable
      ? { tone: 'unclean' as const, label: 'Archive needs attention', text: 'Attention' }
      : { tone: 'idle' as const, label: 'Local archive ready', text: 'Local archive' }
);

function submitGlobalSearch(query: string, mode: ExploreSearchMode): void {
  const workspace = exploreState.current.workspace;
  if (workspace === 'everything' || workspace === 'files') {
    commitSearch(query, mode);
    focusGrid();
    return;
  }
  beforeCommit();
  exploreState.commitSearchIn('everything', query, mode);
}
```

`applyTemporaryDensity` keeps its body. It now receives
`'daemon' | 'compact' | 'comfortable'` from `DisplayMenu`.

- [ ] **Step 5: Update EverythingWorkspace**

In `EverythingWorkspace.svelte`:
1. Delete the `<form class="search-bar" …>…</form>` block, the
   `submitSearch` function, and the `.search-bar` and `.query-control` styles
   and their media-query rules.
2. Change the `searchInput` prop from `$bindable()` to a plain prop, and
   update the `Props` interface.
3. Delete the `<footer class="keyboard-help" …>` block, its styles, and the
   `KbdBadge` import if nothing else uses it.
4. Keep "Refine search", `SearchCoverage`, and every other block.

In `AppShell.svelte`, change `bind:searchInput` on `<EverythingWorkspace>` to
`{searchInput}`.

In `SelectionBar.svelte`, delete the "Space toggle" and "A visible" key-badge
markup at lines 65–66 and any styles only they use.

In `App.svelte`, delete `<svelte:head><title>Everything · msgvault</title></svelte:head>`.
AppShell now owns the title. The boot and login screens set theirs in Task 11.

- [ ] **Step 6: Add the layout styles**

Replace the `.app-shell`, `.brand`, `.app-shell :global(.kit-top-bar…)`,
`.archive-state…`, and `.appearance-controls` rules with:

```css
.app-shell {
  display: flex;
  height: 100vh;
  min-height: 100vh;
  overflow: hidden;
  background: var(--bg-primary);
  color: var(--text-primary);
}

.app-column {
  display: flex;
  min-width: 0;
  flex: 1;
  flex-direction: column;
}

.app-top-bar {
  display: flex;
  min-height: var(--header-height);
  align-items: center;
  gap: var(--space-3);
  padding: 0 var(--space-5);
  background: var(--bg-surface);
  border-bottom: 1px solid var(--border-default);
}

.app-shell--narrow .app-top-bar {
  padding: 0 var(--space-3);
}

.app-top-bar__end {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  margin-left: auto;
}

.app-main {
  display: flex;
  min-height: 0;
  flex: 1;
  flex-direction: column;
  overflow: hidden;
}
```

Keep `.files-shell` and `.archive-navigation-status`.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `bunx vitest run src/lib/components/shell src/lib/components/search src/lib/components/explore`
Expected: PASS. Fix any older test that looked for the Everything search form
inside `main[aria-label="Everything"]`; the searchbox is now in the top bar.

- [ ] **Step 8: Check types and build**

Run: `bun run check && bun run check:kit-ui && bun run build`
Expected: no errors or warnings.

- [ ] **Step 9: Commit**

Subject: `feat(web): move navigation to a sidebar and search to the top bar`.

---

### Task 10: Page headers and sentence case in every workspace

**Files:**
- Modify: `EverythingWorkspace.svelte`, `FilesWorkspace.svelte`,
  `SavedViewsWorkspace.svelte`, `SourcesWorkspace.svelte`,
  `OperationsWorkspace.svelte`, `OperationRelatedStatus.svelte`,
  `DeletionsWorkspace.svelte`, `DirectoryWorkspace.svelte`,
  `DirectoryReviewCentre.svelte`, `RelationshipsWorkspace.svelte`,
  `SettingsWorkspace.svelte`, `PersonTimeline.svelte` (eyebrow only),
  and `AppShell.svelte` (`.files-shell`)
- Test: `web/src/lib/components/shell/AppShell.test.ts`

**Interfaces:**
- Consumes: `PageHeader` (Task 8).

Use these titles and descriptions. Existing descriptions are kept where they
already say what the page is for.

| Workspace | Title | Description | Actions and view row |
|---|---|---|---|
| Relationships | Relationships | People and domains you've exchanged messages with. | none |
| Directory | Directory | People you've saved, with profiles and contact details. | Promote to person (when present) |
| Reviews | Reviews | Decide which identities and facts belong together. | view row: the existing "Review type" control |
| Everything | Everything | none | `actions`: the existing Preview position control and result count |
| Files | Files (Attachments when person-scoped) | none | file count in `actions` |
| Saved views | Saved views | Searches and layouts you've saved to reuse. | none |
| Sources | Sources | Accounts and imports in your archive, and when they last synced. | View source operations |
| Operations | Operations | Background work and its history. | Refresh operations |
| Deletions | Deletions | Deletions you've staged. Nothing is deleted until you run `msgvault delete-staged`. | none |
| Settings | Settings | How this daemon and the web app behave. | none |

The "Live authority" related-status view in `OperationRelatedStatus.svelte`
keeps its own `h1`, because it replaces the Operations page body. Remove only
its eyebrow paragraph.

- [ ] **Step 1: Write the failing test**

```ts
it.each([
  ['relationships', 'Relationships'], ['directory', 'Directory'], ['directory_review', 'Reviews'],
  ['everything', 'Everything'], ['files', 'Files'], ['saved_views', 'Saved views'],
  ['sources', 'Sources'], ['operations', 'Operations'], ['deletions', 'Deletions']
])('shows one visible page title in %s', async (workspace, title) => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({ workspace }))}`);
  const state = new ExploreState(window);
  render(AppShell, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json(exploreResponse()))), state, enabled: false });
  const headings = await screen.findAllByRole('heading', { level: 1 });
  expect(headings.map((heading) => heading.textContent?.trim())).toEqual([title]);
  expect(headings[0]!.closest('.kit-sr-only')).toBeNull();
  expect(screen.queryByText(/archive workspace|archive operations/i)).toBeNull();
  state.destroy();
});
```

Settings is covered separately, because it renders through the `settings`
snippet. Add a test in `SettingsWorkspace.test.ts` that finds the visible
level-one heading "Settings".

- [ ] **Step 2: Run the tests to verify they fail**

Run: `bunx vitest run src/lib/components/shell/AppShell.test.ts -t "page title"`
Expected: FAIL for Relationships (screen-reader-only title), Saved views
(title case), and the workspaces with eyebrows.

- [ ] **Step 3: Apply `PageHeader`**

For each workspace, replace its header markup with `<PageHeader …>` using the
table above. Move existing action buttons into the `actions` snippet without
changing their labels or handlers. Delete eyebrow paragraphs and their CSS.
Delete each workspace's local `h1` styles.

Use the same outer padding everywhere:
`padding: var(--space-5) var(--space-6) var(--space-4);`, with
`padding-inline: var(--space-4)` below 760px. Data workspaces fill the width:
remove `max-width` and `margin-inline: auto` from `.everything-workspace`,
`.files-shell`, and `.saved-views`. Saved views keeps a readable width on its
form content only: `max-width: 960px` on the form and list, not on the page.

For Relationships, render `PageHeader` above the list and detail split, and
remove the screen-reader-only `h1`. For Settings, replace the
screen-reader-only `h1` with `PageHeader` above `SettingsLayout`.

For Files, move the Everything `ContextBar` in `AppShell.svelte`'s `files-shell`
below the Files `PageHeader`. Do this by moving `FilesWorkspace`'s header into
`AppShell`'s `files-shell` above `ContextBar`. `FilesWorkspace` keeps rendering
its own header when `personScoped`, because Relationships and Directory embed
it without the shell.

- [ ] **Step 4: Apply sentence case**

Change these visible strings, keeping the accessible names the spec names:
- `main aria-label="Saved Views"` → `"Saved views"`; heading and empty state
  "No Saved Views yet" → "No saved views yet"; "Loading Saved Views…" →
  "Loading saved views…"; modal "Delete Saved View?" → "Delete saved view?"
- The command palette entry "Open Reviews" keeps its label; it names a
  workspace.

Then run `rg -n "Saved Views" src --glob '*.svelte'` and change each remaining
visible occurrence the same way. Playwright's default name match is
case-insensitive, so specs that use `'Saved Views'` without `exact: true` keep
working. Task 12 fixes any spec that uses `exact: true`.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `bunx vitest run src/lib/components`
Expected: PASS.

- [ ] **Step 6: Check and commit**

Run: `bun run check && bun run check:kit-ui`
Subject: `feat(web): give every workspace the same page header`.

---

### Task 11: Sign-in and boot screens

**Files:**
- Modify: `web/src/App.svelte` (boot markup and styles)
- Modify: `web/src/lib/components/auth/Login.svelte`
- Test: `web/src/App.test.ts`

- [ ] **Step 1: Write the failing test**

Add to `App.test.ts`, following its existing session fixtures:

```ts
it('titles the connecting screen msgvault', () => {
  const session = createSessionController(() => new Promise<Response>(() => undefined));
  render(App, { session });
  expect(screen.getByRole('main', { name: 'Connecting' })).toBeDefined();
  expect(document.title).toBe('msgvault');
});
```

The eyebrow removal is visual and is checked in the Task 13 screenshots.

- [ ] **Step 2: Run the test to verify it fails**

Run: `bunx vitest run src/App.test.ts -t "connecting screen msgvault"`
Expected: FAIL.

- [ ] **Step 3: Implement**

In `App.svelte`, add
`<svelte:head>{#if !shellMounted || messageID !== undefined}<title>msgvault</title>{/if}</svelte:head>`.
Replace each `<p class="eyebrow">msgvault</p>` with a brand line matching the
sidebar brand: `<p class="boot__brand">msgvault</p>`. Use sentence case and
palette tokens in the `.boot` styles: 14px base, `--text-muted` for secondary
text, one solid "Retry" button.

In `Login.svelte`, make the same eyebrow change. Keep the heading "Log in"
and every field name.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `bunx vitest run src/App.test.ts src/lib/components/auth`
Expected: PASS.

- [ ] **Step 5: Commit**

Subject: `feat(web): match the sign-in and boot screens to the new palette`.

---

### Task 12: Browser tests

**Files:**
- Modify: `web/tests/kit-ui.ts`
- Modify: `web/tests/density-restoration.spec.ts`,
  `web/tests/theme-keyboard.spec.ts`,
  `web/tests/docs-fixture-screenshots.spec.ts`,
  `web/tests/e2e/accessibility.spec.ts`, and any spec found by the searches in
  Step 1
- Create: `web/tests/shell-navigation.spec.ts`

**Interfaces:**
- Produces in `tests/kit-ui.ts`:
  - `selectWorkspace(page, label)` — replaces `selectKitTopBarTab`
  - `setTemporaryDensity(page, 'Auto' | 'Compact' | 'Comfortable')`

- [ ] **Step 1: Find the affected specs**

Run:
```bash
rg -ln "selectKitTopBarTab|Temporary density|Density: |name: 'Primary'|keyboard-help|Preview position" tests
rg -n "exact: true" tests | rg -i "saved views"
```

- [ ] **Step 2: Update the helpers**

Replace `selectKitTopBarTab` in `tests/kit-ui.ts` with:

```ts
export async function selectWorkspace(page: Page, label: string): Promise<void> {
  const opener = page.getByRole('button', { name: 'Open navigation' });
  if (await opener.isVisible()) await opener.click();
  await page.getByRole('navigation', { name: 'Primary' }).getByRole('button', { name: label, exact: true }).click();
}

export async function setTemporaryDensity(
  page: Page,
  density: 'Auto' | 'Compact' | 'Comfortable'
): Promise<void> {
  await page.getByRole('button', { name: 'Display' }).click();
  await page.getByRole('menuitemradio', { name: density }).click();
  await page.keyboard.press('Escape');
}
```

Rename every `selectKitTopBarTab` call to `selectWorkspace`, and use sentence
case labels ("Saved views"). Replace every
`selectKitOption(page, 'Temporary density', 'Density: X')` with
`setTemporaryDensity(page, 'X')`. Replace assertions on the combobox
"Temporary density: …" with assertions on the checked radio:
`page.getByRole('menuitemradio', { name: 'Compact', checked: true })` after
opening the Display menu.

- [ ] **Step 3: Write the new spec**

`tests/shell-navigation.spec.ts` uses the same archive fixture routing as
`session-navigation.spec.ts`; copy its `beforeEach` setup.

```ts
import { expect, test } from '@playwright/test';

import { selectWorkspace } from './kit-ui';

test('sidebar rail keeps names and survives reload', async ({ page }) => {
  await page.goto('/?workspace=everything');
  await page.getByRole('button', { name: 'Collapse sidebar' }).click();
  await expect(page.getByRole('navigation', { name: 'Primary' }).getByRole('button', { name: 'Saved views' })).toBeVisible();
  await page.reload();
  await expect(page.getByRole('button', { name: 'Expand sidebar' })).toBeVisible();
});

test('narrow navigation menu traps focus and closes three ways', async ({ page }) => {
  await page.setViewportSize({ width: 420, height: 860 });
  await page.goto('/?workspace=everything');
  const opener = page.getByRole('button', { name: 'Open navigation' });

  await opener.click();
  await expect(page.getByRole('button', { name: 'Everything' })).toBeFocused();
  for (let index = 0; index < 20; index += 1) await page.keyboard.press('Tab');
  await expect(page.getByRole('dialog', { name: 'Navigation' }).locator(':focus')).toHaveCount(1);
  await page.keyboard.press('Escape');
  await expect(opener).toBeFocused();

  await opener.click();
  await page.getByRole('button', { name: 'Close navigation' }).click({ position: { x: 400, y: 400 } });
  await expect(opener).toBeFocused();

  await selectWorkspace(page, 'Sources');
  await expect(page.getByRole('heading', { level: 1, name: 'Sources' })).toBeVisible();
  await expect(page.getByRole('dialog', { name: 'Navigation' })).toHaveCount(0);
});

test('Escape in the narrow menu leaves an open reading pane open', async ({ page }) => {
  await page.setViewportSize({ width: 420, height: 860 });
  await page.goto('/?workspace=everything');
  await page.getByRole('grid', { name: 'Everything results' }).getByRole('row').nth(1).click();
  await expect(page.getByRole('complementary', { name: /^Reading pane/ })).toBeVisible();
  await page.getByRole('button', { name: 'Open navigation' }).click();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('complementary', { name: /^Reading pane/ })).toBeVisible();
});

test('global search from another workspace opens Everything', async ({ page }) => {
  await page.goto('/?workspace=sources');
  await page.getByRole('searchbox', { name: 'Search everything' }).fill('fixture');
  await page.keyboard.press('Enter');
  await expect(page.getByRole('main', { name: 'Everything' })).toBeVisible();
  await page.goBack();
  await expect(page.getByRole('main', { name: 'Sources' })).toBeVisible();
});
```

- [ ] **Step 4: Run the browser tests**

From the repository root, run: `make web-test-browser`
Expected: all specs pass, including `e2e/accessibility` (axe) in both themes.

- [ ] **Step 5: Commit**

Subject: `test(web): cover the sidebar, navigation menu, and global search`.
In the body, list every existing spec edit and why it was needed, as the spec's
"Tests and accessible names" section requires.

---

### Task 13: Verify and prepare the pull request

- [ ] **Step 1: Run the full web checks**

From the repository root:
`make web-check && make web-test && make web-test-browser && make lint-ci`
Expected: all pass with no warnings.

- [ ] **Step 2: Capture after screenshots**

Rebuild the branch binary with `make build` and restart the isolated fixture
daemon with the new binary. Capture the same set as Task 1 Step 1, plus the
collapsed rail at 1440×900 and the open narrow menu at 420×860, into `after/`.
View every image. Check that:
- no page shows an eyebrow, uppercase label, or duplicate title;
- one blue primary button at most per screen;
- the top bar never wraps at 420px;
- dark mode has no unreadable text.

- [ ] **Step 3: Review the diff**

Run `git diff main...HEAD --stat` and read the full diff. Remove unused
imports, styles, and props. Confirm nothing in `web/src/lib/api/generated`
changed.

- [ ] **Step 4: Open the pull request**

Use the `kenn-io-commit-push-pr` skill, and the `kenn-io-pr-desc` skill for
the body. Run the private-data scrub on the body and on every screenshot. The
Enron fixture captures are the documented exception in `AGENTS.md`.

The body describes the result for people using the Web UI, what reviewers
should check (accessible-name changes, kit overrides in `app.css`, the palette
outside `src/`), and before and after screenshots. It has no validation or
test-plan section.

## Self-review notes

- **Spec coverage for delivery item 1:**
  - Palette, type, and status tones: Task 1.
  - Sidebar with rail, footer, and collapse storage: Task 5 and Task 9.
  - Narrow menu with scrim, Escape, focus trap, focus restoration, and Escape
    precedence: Task 6, Task 9, and Task 12.
  - Top bar, global search, and `/`: Task 3, Task 4, and Task 9.
  - Display menu: Task 7 and Task 9.
  - `PageHeader` on every workspace: Task 8 and Task 10.
  - Keyboard registry and footer removal: Task 2 and Task 9.
  - Tab titles: Task 9 and Task 11.
  - Sign-in and boot screens: Task 11.
- **Deferred to later pull requests, per the spec:** empty-state copy changes
  in Deletions and Saved views, the code-label maps, the selection-bar
  redesign, and everything else in delivery items 2 through 4.
- **Focus after choosing a drawer item** returns to "Open navigation". This is
  the drawer's equivalent of today's rule, where focus stays on the tab you
  clicked.
