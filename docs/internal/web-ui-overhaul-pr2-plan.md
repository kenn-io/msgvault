---
title: "Web UI overhaul PR 2: Everything, Files, and Saved views — implementation plan"
description: "Implementation plan for Everything, Files, and Saved views in the Web UI."
last_edited: "2026-09-30"
---

# Web UI overhaul PR 2: Everything, Files, and Saved views — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task by task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Everything and Files one toolbar row, readable context chips, a
selection bar that appears only when needed, a single Files view, and a Save
view dialog next to the view it saves, without removing any capability.

**Architecture:** `ContextBar` becomes the one toolbar for Everything and
Files. It takes a sort configuration, a count label, and optional slots instead
of fixed controls. Label maps live in one new module. Saved-view conversion
moves out of `SavedViewsWorkspace` into a module shared by a new
`SaveViewDialog`. A rule in `normalize()` in `explore/state.svelte.ts` sends
every Everything-as-Files state to the Files workspace.

**Tech stack:** Svelte 5 (runes), `@kenn-io/kit-ui` (`FilterDropdown`,
`SelectDropdown`, `Menu`, `Modal`, `IconButton`), `@lucide/svelte`, Vitest
with Testing Library, Playwright.

**Spec:** [Web UI overhaul](web-ui-overhaul-design.md), sections
"Everything", "Files", "Saved views", and "Control relocation". This plan
covers delivery item 2. PR 1 is merged (#995); its shell, `PageHeader`,
palette, and focus rules are the baseline.

## Global constraints

- Keep every capability. A control that moves keeps its accessible name unless
  this plan or the spec names the change.
- Keep URL state keys, the `explore` JSON format, and API calls unchanged. The
  only state rule added is the Files normalization in Task 2.
- No new npm dependencies. Icons come from `@lucide/svelte`.
- One solid primary button per screen: `tone="info" surface="solid"`. Purple
  (`workflow`) and green (`success`) are not button colors.
- Sentence case for every visible label. Monospace only for identifiers,
  hashes, code, keys, and cron text. Raw colors only in `web/palette.css`.
- Raw API codes are never the only text a person sees. Each displayed code gets
  a readable label; the raw value may stay in a `title` tooltip.
- Tests use Vitest and Testing Library or Playwright, query by role and
  accessible name, and use failure-safe cleanup (`afterEach` or
  `try`/`finally`).
- Run commands from `web/` unless a step says otherwise. After each task run
  `bun run check`, `bun run check:kit-ui`, and the focused tests.
- Commit after each task with the `kenn-io-commit` rules: new commits only,
  conventional imperative subject, a short why body, the attribution trailer
  the controller gives you, and a private-data check (public repository).

## File structure

| File | Responsibility |
|---|---|
| `web/src/styles/tokens.css` | New `--page-gutter` token |
| `web/src/lib/explore/labels.ts` (new) | Readable labels for search modes, filter dimensions, presentations, file types, and preflight reasons |
| `web/src/lib/explore/state.svelte.ts` | Files normalization in `normalize()` |
| `web/src/lib/components/explore/ContextBar.svelte` | The one toolbar: Filters, Show as, Group by, Sort, extra controls slot, count; removable chips; filter panel |
| `web/src/lib/components/explore/EverythingTable.svelte` | Columns become fully controlled; the `<details>` picker leaves |
| `web/src/lib/components/explore/ColumnsMenu.svelte` (new) | Columns picker as a kit `FilterDropdown` |
| `web/src/lib/components/explore/SelectionBar.svelte` | Compact strip below results, only while something is selected; Review for deletion; overflow menu |
| `web/src/lib/components/shell/EverythingWorkspace.svelte` | Toolbar composition, notices below the toolbar, selection bar placement, Save view action |
| `web/src/lib/components/reader/ReadingPane.svelte` | Tasks button, icon Close, readable meta strip |
| `web/src/lib/components/tasks/TaskLinks.svelte` | Reports its linked-task count |
| `web/src/lib/components/files/FilesWorkspace.svelte` | Type menu, readable Type column, containing-item row action, Visual search label, no duplicate region |
| `web/src/lib/components/explore/FilesPresentation.svelte` (+ test) | Deleted |
| `web/src/lib/components/shell/AppShell.svelte` | Files toolbar wiring, Show as routing, Save view action, deletion review from the bar, Escape containment, stale count reset |
| `web/src/lib/components/directory/PersonDetail.svelte` | Media & files filters and sort work |
| `web/src/lib/saved-views/canonical.ts` (new) | Explore state ⇄ saved-view canonical state, readable summary |
| `web/src/lib/components/saved-views/SaveViewDialog.svelte` (new) | Name, Description, Save; Files note |
| `web/src/lib/components/saved-views/SavedViewsWorkspace.svelte` | Library only |

---

### Task 1: One page gutter

The page padding is repeated in about ten workspaces, and the top bar uses a
different inset (`--space-5`) from the pages (`--space-6`), so their left
edges do not line up.

**Files:**
- Modify: `web/src/styles/tokens.css`
- Modify: every workspace root that sets
  `padding: var(--space-5) var(--space-6) var(--space-4)` or
  `padding-inline: var(--space-4)` at 760px (find them with
  `rg -n "space-6\) var\(--space-4\)|padding-inline: var\(--space-4\)" src --glob '*.svelte'`),
  and `.app-top-bar` in `AppShell.svelte`

- [ ] **Step 1: Add the token**

Append to the `:root` block in `tokens.css`:

```css
  --page-gutter: var(--space-6);
```

and after that block:

```css
@media (max-width: 760px) {
  :root {
    --page-gutter: var(--space-4);
  }
}
```

- [ ] **Step 2: Use it**

Replace each workspace root's horizontal padding with
`padding: var(--space-5) var(--page-gutter) var(--space-4);` and delete the
now-redundant 760px `padding-inline` overrides. Set `.app-top-bar` horizontal
padding to `var(--page-gutter)` and delete its narrow override.

- [ ] **Step 3: Verify**

Run `bun run check && bun run check:kit-ui && bun run test`. Capture
`everything`, `sources`, and `settings` at 1440 and 420 with the controller's
screenshot helper, and confirm the top-bar search box and page titles share a
left edge.

- [ ] **Step 4: Commit**

Subject: `refactor(web): share one page gutter across the shell`.

---

### Task 2: Everything-as-Files links open the Files workspace

**Files:**
- Modify: `web/src/lib/explore/state.svelte.ts` (`normalize()`, around the
  `presentation` and `workspace` derivations)
- Test: `web/src/lib/explore/state.test.ts`, `web/src/lib/components/shell/AppShell.test.ts`

**Interfaces:**
- Produces: after `normalize()`, `workspace === 'files'` implies
  `presentation === 'files'`, and `presentation === 'files'` implies
  `workspace === 'files'`. Everything never holds `presentation: 'files'`.

- [ ] **Step 1: Write the failing tests**

In `state.test.ts`:

```ts
it('sends an Everything-as-Files link to the Files workspace with its context', () => {
  const restored = parseExploreURLState(
    `?workspace=everything&mode=hybrid&explore=${encodeURIComponent(JSON.stringify({
      presentation: 'files', query: 'invoice', filters: [{ dimension: 'source', values: ['7'] }],
      groupingChain: ['year'], columns: ['kind', 'title']
    }))}`
  );
  expect(restored).toMatchObject({
    workspace: 'files', presentation: 'files', searchMode: 'hybrid', query: 'invoice',
    filters: [{ dimension: 'source', values: ['7'] }], groupingChain: ['year'], columns: ['kind', 'title']
  });
});

it('keeps Files presentation in the Files workspace', () => {
  const restored = parseExploreURLState(
    `?workspace=files&explore=${encodeURIComponent(JSON.stringify({ presentation: 'table' }))}`
  );
  expect(restored.presentation).toBe('files');
});

it('restores Files for an old Everything-as-Files history entry', async () => {
  window.history.replaceState(null, '', '/?workspace=sources');
  const state = new ExploreState(window);
  try {
    window.history.pushState(
      {
        exploreSearch: '?workspace=everything',
        exploreState: { workspace: 'everything', presentation: 'files', query: 'budget' }
      },
      '',
      '/?workspace=everything'
    );
    window.history.pushState(null, '', '/?workspace=sources');
    const restored = new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    window.history.back();
    await restored;
    expect(state.current).toMatchObject({ workspace: 'files', presentation: 'files', query: 'budget' });
  } finally {
    state.destroy();
  }
});
```

`readURLState()` uses `history.state.exploreState` only when
`history.state.exploreSearch` equals `location.search`, which the pushed entry
above satisfies, so the test exercises the real popstate path.

In `AppShell.test.ts`, add a test that loads
`?workspace=everything&explore={"presentation":"files","selectedRow":"attachment:5"}`
with a fetch mock that answers the attachment metadata endpoint the
contextual viewer uses (see `openContextualFile` and the effect that reopens
the viewer from `parseAttachmentSelection`), and asserts the page shows
`main` "Files" and the dialog "View <filename>".

- [ ] **Step 2: Run to verify they fail**

Run: `bunx vitest run src/lib/explore/state.test.ts src/lib/components/shell/AppShell.test.ts -t "Files"`
Expected: FAIL; the workspace stays `everything`.

- [ ] **Step 3: Implement**

In `normalize()`, after `workspace` and `presentation` are derived:

```ts
  const filesView = workspace === 'files' || (workspace === 'everything' && presentation === 'files');
  const normalizedWorkspace = filesView ? 'files' : workspace;
  const normalizedPresentation = filesView
    ? 'files'
    : presentation === 'files' ? defaultExploreURLState.presentation : presentation;
```

Use `normalizedWorkspace` and `normalizedPresentation` in the returned object.
When the address bar holds a link that normalizes differently, the existing
constructor path already rewrites it; confirm the URL after load reads
`workspace=files` and add that assertion to the AppShell test.

If the contextual viewer effect only runs for `workspace === 'everything'`,
extend it to `files` so `attachment:<id>` reopens the viewer there.

- [ ] **Step 4: Run to verify they pass, then run `bunx vitest run src/lib/explore src/lib/components/shell`**

- [ ] **Step 5: Commit**

Subject: `feat(web): open Everything-as-Files links in the Files workspace`.

---

### Task 3: Readable labels

**Files:**
- Create: `web/src/lib/explore/labels.ts`
- Test: `web/src/lib/explore/labels.test.ts`

**Interfaces:**
- Produces:

```ts
export function searchModeLabel(mode: ExploreSearchMode): string;
export function filterDimensionLabel(dimension: ExploreFilterDimension): string;
export function presentationLabel(presentation: ExploreURLState['presentation']): string;
export function fileTypeLabel(mimeType: string | undefined, family: FileMIMEFamily | undefined): string;
export const FILE_FAMILY_LABELS: Record<FileMIMEFamily, string>;
export function preflightReasonLabel(action: string, reason: string): string;
```

- [ ] **Step 1: Write the failing tests**

```ts
import { describe, expect, it } from 'vitest';

import {
  fileTypeLabel, filterDimensionLabel, preflightReasonLabel, presentationLabel, searchModeLabel
} from './labels';

describe('explore labels', () => {
  it('names search modes and presentations', () => {
    expect(searchModeLabel('full_text')).toBe('Full text');
    expect(searchModeLabel('semantic')).toBe('Semantic');
    expect(searchModeLabel('hybrid')).toBe('Hybrid');
    expect(presentationLabel('timeline')).toBe('Timeline');
  });

  it('names filter dimensions and falls back to sentence case', () => {
    expect(filterDimensionLabel('source')).toBe('Source');
    expect(filterDimensionLabel('participant')).toBe('Person');
    expect(filterDimensionLabel('message_type' as never)).toBe('Message type');
  });

  it('names file types from the MIME type, then the family', () => {
    expect(fileTypeLabel('application/pdf', 'pdf')).toBe('PDF');
    expect(fileTypeLabel('image/png', 'image')).toBe('PNG image');
    expect(fileTypeLabel('image/webp', 'image')).toBe('WEBP image');
    expect(fileTypeLabel('application/vnd.openxmlformats-officedocument.wordprocessingml.document', 'document'))
      .toBe('Word document');
    expect(fileTypeLabel('', 'archive')).toBe('Archive');
    expect(fileTypeLabel(undefined, undefined)).toBe('Unknown type');
  });

  it('turns preflight reasons into sentences', () => {
    expect(preflightReasonLabel('open_in_source', 'trusted_source_link_unavailable'))
      .toBe('Your sources don’t provide links to open these items.');
    expect(preflightReasonLabel('export', 'browser_export_requires_single_message'))
      .toBe('Export works for one message at a time.');
    expect(preflightReasonLabel('export', 'some_new_reason')).toBe('Some new reason.');
  });
});
```

Use the real `ExploreFilterDimension` union from `explore/models.ts`: add a
label for every member and keep the fallback for unknown values. Use
`groupingDimensionLabel` from `grouping/catalog.ts` where a dimension is also a
grouping, so the two never disagree.

- [ ] **Step 2: Run to verify it fails** (module missing).

- [ ] **Step 3: Implement**

```ts
import { groupingDimensionLabel, isGroupingDimension } from '../grouping/catalog';
import type { ExploreFilterDimension, ExploreSearchMode, ExploreURLState, FileMIMEFamily } from './models';

const SEARCH_MODES: Record<ExploreSearchMode, string> = {
  full_text: 'Full text',
  semantic: 'Semantic',
  hybrid: 'Hybrid'
};

const PRESENTATIONS: Record<ExploreURLState['presentation'], string> = {
  table: 'Table',
  timeline: 'Timeline',
  files: 'Files'
};

const FILTER_DIMENSIONS: Partial<Record<string, string>> = {
  participant: 'Person',
  identity: 'Identity'
};

export const FILE_FAMILY_LABELS: Record<FileMIMEFamily, string> = {
  image: 'Images',
  pdf: 'PDFs',
  audio: 'Audio',
  video: 'Video',
  text: 'Text',
  document: 'Documents',
  archive: 'Archives',
  other: 'Other'
};

const FAMILY_SINGULAR: Record<FileMIMEFamily, string> = {
  image: 'Image',
  pdf: 'PDF',
  audio: 'Audio',
  video: 'Video',
  text: 'Text',
  document: 'Document',
  archive: 'Archive',
  other: 'File'
};

const KNOWN_MIME: Record<string, string> = {
  'application/pdf': 'PDF',
  'application/zip': 'ZIP archive',
  'application/msword': 'Word document',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document': 'Word document',
  'application/vnd.ms-excel': 'Excel spreadsheet',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet': 'Excel spreadsheet',
  'application/vnd.ms-powerpoint': 'PowerPoint presentation',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation': 'PowerPoint presentation',
  'text/plain': 'Text',
  'text/html': 'HTML',
  'text/csv': 'CSV',
  'text/calendar': 'Calendar invite',
  'message/rfc822': 'Email message'
};

const REASONS: Record<string, string> = {
  'open_in_source:trusted_source_link_unavailable': 'Your sources don’t provide links to open these items.',
  'export:browser_export_requires_single_message': 'Export works for one message at a time.',
  'export:selection_has_no_exportable_raw_message': 'The selection has no original message to export.',
  'export:raw_message_unavailable': 'The original message isn’t available.',
  'export_files:selection_contains_no_files': 'The selection has no files.',
  'stage_deletion:selection_contains_items_that_cannot_be_deleted_from_source':
    'Some selected items can’t be deleted from their source.'
};

function sentenceCase(code: string): string {
  const words = code.replace(/[_-]+/g, ' ').trim();
  return words ? words[0]!.toUpperCase() + words.slice(1) : '';
}

export function searchModeLabel(mode: ExploreSearchMode): string {
  return SEARCH_MODES[mode];
}

export function presentationLabel(presentation: ExploreURLState['presentation']): string {
  return PRESENTATIONS[presentation];
}

export function filterDimensionLabel(dimension: ExploreFilterDimension): string {
  const explicit = FILTER_DIMENSIONS[dimension];
  if (explicit) return explicit;
  if (isGroupingDimension(dimension)) return groupingDimensionLabel(dimension);
  return sentenceCase(dimension);
}

export function fileTypeLabel(mimeType: string | undefined, family: FileMIMEFamily | undefined): string {
  const mime = mimeType?.toLowerCase().split(';')[0]?.trim() ?? '';
  const known = KNOWN_MIME[mime];
  if (known) return known;
  const [kind, subtype] = mime.split('/');
  if ((kind === 'image' || kind === 'audio' || kind === 'video') && subtype) {
    return `${subtype.replace(/^x-/, '').toUpperCase()} ${kind}`;
  }
  return family ? FAMILY_SINGULAR[family] : 'Unknown type';
}

export function preflightReasonLabel(action: string, reason: string): string {
  return REASONS[`${action}:${reason}`] ?? `${sentenceCase(reason)}.`;
}
```

If `groupingDimensionLabel` for a dimension returns something that reads
wrongly as a filter chip, add an explicit `FILTER_DIMENSIONS` entry instead of
changing the grouping catalog.

- [ ] **Step 4: Run to verify it passes.**

- [ ] **Step 5: Commit**

Subject: `feat(web): add readable labels for explore codes`.

---

### Task 4: One toolbar row with removable chips

**Files:**
- Modify: `web/src/lib/components/explore/ContextBar.svelte`
- Create: `web/src/lib/components/explore/ColumnsMenu.svelte`
- Modify: `web/src/lib/components/explore/EverythingTable.svelte`
  (delete the `<details>` Columns picker at ~lines 426-437; keep
  `columns` controlled by the prop)
- Modify: `web/src/lib/components/shell/EverythingWorkspace.svelte`
- Modify: `web/src/lib/components/shell/AppShell.svelte`
  (`openContextControl`, `fixedSortNotice`)
- Test: `ContextBar.test.ts`, `ColumnsMenu.test.ts` (new),
  `EverythingTable.test.ts`, `EverythingWorkspace.test.ts`, `AppShell.test.ts`

**Interfaces:**
- Consumes: `labels.ts` (Task 3).
- Produces the new `ContextBar` props (existing ones unchanged unless listed):
  - `sort: { options: { value: string; label: string }[]; value: string; note?: string; onchange?: (value: string) => void }`
    rendered as a kit `SelectDropdown` titled "Sort" inside
    `<div data-sort-menu>`. Accessible name: "Sort: <selected label>".
  - `countLabel: string` — the one visible count, at the right edge.
  - `extra?: Snippet` — controls placed before the count (Columns, Preview
    position).
  - `onRemoveQuery?: () => void`, `onRemoveFilter?: (index: number) => void`.
  - `totalCount` is removed; callers pass `countLabel`.
- `ColumnsMenu` props: `columns: ExploreColumn[]`,
  `onchange: (columns: ExploreColumn[]) => void`. Kit `FilterDropdown` with
  `label="Columns"`, one section of seven items (Kind, People / source,
  Subject / title, Excerpt, Time, Attachments, Size). Toggling the last visible
  column is ignored so at least one stays, matching the table's current rule.

- [ ] **Step 1: Write the failing tests**

`ContextBar.test.ts` (replace assertions that depended on the old strings):

```ts
it('shows readable, removable chips for the query, filters, and groupings', async () => {
  const onRemoveQuery = vi.fn();
  const onRemoveFilter = vi.fn();
  const onRemoveGroup = vi.fn();
  render(ContextBar, {
    client: createAPIClient(vi.fn()),
    query: 'network', searchMode: 'full_text',
    filters: [{ dimension: 'source', values: ['7'] }],
    groupingChain: ['year'],
    countLabel: '20 items',
    sort: { options: [{ value: 'newest', label: 'Newest first' }], value: 'newest' },
    onAddGroup: vi.fn(), onRemoveGroup, onClearFilters: vi.fn(), onFiltersChange: vi.fn(),
    onRemoveQuery, onRemoveFilter
  });
  expect(screen.getByText('Full text: “network”')).toBeTruthy();
  expect(screen.getByText('Source: 7')).toBeTruthy();
  expect(screen.queryByText(/full_text/)).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Remove search' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Source filter' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Year grouping' }));
  expect(onRemoveQuery).toHaveBeenCalledOnce();
  expect(onRemoveFilter).toHaveBeenCalledWith(0);
  expect(onRemoveGroup).toHaveBeenCalledWith(0);
  expect(screen.getAllByText('20 items')).toHaveLength(1);
});

it('hides the chip line when nothing is active', () => {
  render(ContextBar, {
    client: createAPIClient(vi.fn()),
    query: '', searchMode: 'full_text', filters: [], groupingChain: [],
    countLabel: '20 items',
    sort: { options: [{ value: 'newest', label: 'Newest first' }], value: 'newest' },
    onAddGroup: vi.fn(), onRemoveGroup: vi.fn(), onClearFilters: vi.fn(), onFiltersChange: vi.fn()
  });
  expect(screen.queryByText('All archive entries')).toBeNull();
  expect(screen.queryByRole('button', { name: /^Remove / })).toBeNull();
});
```

`ColumnsMenu.test.ts`: open "Columns", toggle "Excerpt" off, expect
`onchange` without `excerpt`; with only `['title']`, toggling "Subject / title"
does not call `onchange`.

`AppShell.test.ts`: on Everything, press `s` and assert the combobox named
`/^Sort: Newest first/` has focus and its listbox shows the note "Other
orders aren't available yet"; press `r` and assert the "Sort status" live
region announces the fixed order. Assert the result count text appears once
in `main` "Everything".

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**
  1. `ContextBar`:
     - One `.context-controls` row: Filters button, "Show as"
       `SelectDropdown`, "Group by" picker (unchanged), the Sort
       `SelectDropdown` inside `data-sort-menu`, `{@render extra?.()}`, then
       `<span class="context-count" aria-live="polite">{countLabel}</span>`
       pushed right. The count uses tabular sans (`data-mono`), not a box.
     - A `.context-chips` line rendered only when a query, filter, or grouping
       is active. Chips:
       - Query: `${searchModeLabel(searchMode)}: “${query}”`, with an
         `IconButton` named "Remove search" calling `onRemoveQuery`.
       - Filters: `${filterDimensionLabel(dimension)}: ${values.join(', ')}`,
         with an `IconButton` named `Remove ${label} filter`.
       - Groupings: `Grouped by ${label}`, keeping the name
         `Remove ${label} grouping`.
     - Show the sort `note` as a disabled `SelectDropdown` option whose label
       is the note text, if the kit renders disabled options; otherwise render
       it as `title` on the trigger and mention the fallback in your report.
     - Delete the "All archive entries" empty text.
  2. `ColumnsMenu.svelte`: kit `FilterDropdown` as specified; each item's
     `closeOnSelect` is `false`.
  3. `EverythingTable`: delete the `<details>` block and its styles. Keep the
     `columns` prop and derive `visibleColumns` from it (it no longer owns a
     local copy). Leave `onColumnsChange` removal to the caller: delete the
     prop if nothing else uses it.
  4. `EverythingWorkspace`:
     - Remove the count and Preview position from `PageHeader` actions.
     - Pass `countLabel`: the existing text ("N results shown" when the
       candidate pool is saturated, "N items" when counted, "Counting…" while
       loading).
     - Pass `sort={{ options: [{ value: 'newest', label: 'Newest first' }], value: 'newest', note: 'Other orders aren’t available yet', onchange: fixedSortNotice }}`.
     - `extra`: `ColumnsMenu` (table presentation only) wired to
       `exploreState.replaceTransient({ columns })`, and the Preview position
       `SegmentedControl` (unchanged names; still only when
       `canPreviewRight`), with the visible "Preview position" text removed
       (the control keeps its accessible name).
     - `onRemoveQuery`: `commitSearch('', searchMode)`.
     - `onRemoveFilter`: commit the filters without that index, resetting
       `activeRow`, `selectedRow`, and `scrollAnchor` like `onFiltersChange`.
     - Move the "More results may match" notice, `SearchCoverage`, and the
       semantic scope note below `ContextBar`, above the results.
  5. `AppShell`: `openContextControl('sort')` and `fixedSortNotice` target
     `[data-sort-menu] button` instead of `button[aria-label="Sort: newest first"]`.

- [ ] **Step 4: Run** `bunx vitest run src/lib/components/explore src/lib/components/shell`.

- [ ] **Step 5: Commit**

Subject: `feat(web): put Everything's controls in one toolbar row`.

---

### Task 5: Selection bar only when something is selected

**Files:**
- Modify: `web/src/lib/components/explore/SelectionBar.svelte`
- Modify: `web/src/lib/components/shell/EverythingWorkspace.svelte`
- Modify: `web/src/lib/components/shell/AppShell.svelte` (pass
  `onReviewDeletion`)
- Test: `SelectionBar.test.ts`, `AppShell.test.ts`

**Interfaces:**
- Consumes: `preflightReasonLabel` (Task 3); AppShell's existing
  `openDeletionReview(mode)`.
- Produces: `SelectionBar` prop `onReviewDeletion?: (mode: 'explicit' | 'all_matching') => void`.
  Rendered only when `selection.count > 0` or `selection.mode === 'all_matching'`.

- [ ] **Step 1: Write the failing tests**

`SelectionBar.test.ts`:
- With an empty explicit selection, nothing renders (no "No items selected").
- With 3 selected, "3 selected" and a button "Review for deletion…" render;
  clicking it calls `onReviewDeletion('explicit')`; after
  `selectAllMatching`, it calls `onReviewDeletion('all_matching')`.
- With a preflight whose `unavailable_actions` contains
  `{ action: 'open_in_source', reason: 'trusted_source_link_unavailable' }`,
  opening the menu "More selection actions" shows a disabled item "Open
  selection in source" and the text "Your sources don’t provide links to open
  these items."; the raw code is not visible text.
- With an export reason, the text reads the sentence from
  `preflightReasonLabel`, not `Export: browser_export_requires_single_message`.

`AppShell.test.ts`: select a row, click "Review for deletion…", assert the
workspace becomes `deletions` and the deletion preflight request is sent, the
same as pressing `d`.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**
  - `SelectionBar`: wrap the markup in `{#if selection.mode === 'all_matching' || selection.count > 0}`.
    Keep the status text, "Select all N matching items", "Export selection",
    meeting-context export, and "Clear selection". Add
    `Button label="Review for deletion…"` (neutral, not red) calling
    `onReviewDeletion(selection.mode === 'all_matching' ? 'all_matching' : 'explicit')`.
    Replace the "Open in source" text or button with a kit `Menu` whose trigger
    is an `IconButton` named "More selection actions" (lucide `ellipsis`); it
    holds `MenuItem` "Open selection in source" (disabled when a reason exists,
    enabled and calling `onOpenInSource` otherwise) and, when disabled, the
    reason sentence as muted text. Export's unavailable text becomes
    `Export unavailable: <sentence>` with the raw code in `title`.
  - `EverythingWorkspace`: move `<SelectionBar>` from above the table and
    timeline to below them inside `.results-primary`, as a
    `position: sticky; bottom: 0` strip with the surface background and a top
    border. Pass `onReviewDeletion`.
  - `AppShell`: thread `onReviewDeletion={openDeletionReview}` to
    `EverythingWorkspace`.

- [ ] **Step 4: Run** `bunx vitest run src/lib/components/explore src/lib/components/shell`.

- [ ] **Step 5: Commit**

Subject: `feat(web): show the selection bar only while items are selected`.

---

### Task 6: Reading-pane header

**Files:**
- Modify: `web/src/lib/components/reader/ReadingPane.svelte`
- Modify: `web/src/lib/components/tasks/TaskLinks.svelte`
- Test: `ReadingPane.test.ts` (find with `rg -l "ReadingPane" src --glob '*.test.ts'`), `TaskLinks.test.ts`

**Interfaces:**
- Produces: `TaskLinks` bindable prop `linkedCount: number | undefined`, set
  after its first successful lookup.

- [ ] **Step 1: Write the failing tests**
- The `<details>`/`<summary>` "Tasks for this message" is replaced by a button
  with the same accessible name and `aria-expanded`; clicking it shows the
  "Linked tasks" section; after the lookup resolves with 2 linked tasks, the
  button's visible text reads "Tasks 2".
- The close control is an icon button named "Close reading pane" with no
  visible "Close" text.
- The meta strip shows readable kinds ("Email", "Chat", "Calendar event",
  "Meeting") instead of raw `message_type` values; reuse the labels the
  `RowKind` component already uses rather than adding a second map.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**
  - Replace the disclosure with `Button size="sm" surface="outline"` named
    "Tasks for this message", visible label `Tasks` plus the count when known,
    toggling `tasksOpen`. Bind `linkedCount` from `TaskLinks`. The count
    appears after the sheet has loaded once for the open message; it resets
    when the selection changes (the existing effect that closes the sheet).
  - Replace the Close `Button` with `IconButton` (lucide `x`) named "Close
    reading pane".
  - Build the meta strip from the readable kind label.

- [ ] **Step 4: Run** `bunx vitest run src/lib/components/reader src/lib/components/tasks`.

- [ ] **Step 5: Commit**

Subject: `feat(web): make reading-pane actions visible and readable`.

---

### Task 7: One Files view

**Files:**
- Delete: `web/src/lib/components/explore/FilesPresentation.svelte` and its test
- Modify: `web/src/lib/components/files/FilesWorkspace.svelte`
- Modify: `web/src/lib/components/shell/AppShell.svelte` (files branch,
  `currentGrid()`, file count reset)
- Modify: `web/src/lib/components/shell/EverythingWorkspace.svelte`
  (remove the Files branch; Show as Files navigates)
- Test: `FilesWorkspace.test.ts`, `AppShell.test.ts`, `EverythingWorkspace.test.ts`

**Interfaces:**
- Consumes: `ContextBar` `sort`/`countLabel` (Task 4), `fileTypeLabel`,
  `FILE_FAMILY_LABELS` (Task 3), the Task 2 normalization.
- Produces: the Type menu lives in `FilesWorkspace`'s own `.file-controls` row,
  so the shell, Relationships, and Directory embeds all get it.

- [ ] **Step 1: Write the failing tests**
- `FilesWorkspace.test.ts`:
  - The "MIME families" checkboxes are gone; a `FilterDropdown` named "Type"
    lists "Images, PDFs, Audio, Video, Text, Documents, Archives, Other"
    (person-scoped Media shows Images and Video only; person-scoped Files the
    rest); choosing "PDFs" calls `onMIMEFamiliesChange(['pdf'])`.
  - The Type column shows "PDF" for `application/pdf` with `title="application/pdf"`.
  - Each row has a button named `Open containing item <containing title>`;
    clicking it calls `onOpenItem(entry_key)` and does not open the viewer.
  - The toggle is named "Visual search".
  - With `showHeader={false}`, no region named "Files" is rendered (the shell
    owns the landmark).
- `AppShell.test.ts`:
  - On Everything, choosing Show as "Files" lands in `main` "Files" with the
    query and filters kept; in Files, Show as "Table" returns to Everything.
  - In Files, the toolbar's Sort combobox lists "Newest first, Oldest first,
    Filename A–Z, Filename Z–A, Largest first, Smallest first"; choosing
    "Largest first" commits `fileSort` `{ field: 'size', direction: 'desc' }`.
  - In Files, the toolbar count reads "N files" (not "Count pending").
  - Typing in the global search on Files changes the Files search request's
    query (this also covers the PR 1 review gap on live typing in Files).
  - Leaving Files and returning does not flash the previous count.
- `EverythingWorkspace.test.ts`: no grid named "Files in current context" is
  ever rendered.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**
  - Delete `FilesPresentation.svelte` and its test; remove its import and
    branch from `EverythingWorkspace`. Remove `"Files in current context"`
    from `currentGrid()`. Keep `openContextualFile` only if another caller
    remains (`rg -n openContextualFile src`); delete it otherwise.
  - Everything's `onPresentationChange('files')` commits
    `{ workspace: 'files', presentation: 'files', activeRow: null, selectedRow: null, scrollAnchor: null }`.
  - Files toolbar (AppShell `files-shell`): pass `sort` built from
    `fileSort` with the six options above and `countLabel` from the bound
    `fileCount` ("N files", "Counting…" while `null`). Reset `fileCount` to
    `null` whenever the workspace changes.
  - `FilesWorkspace`:
    - Replace the MIME checkbox group with a kit `FilterDropdown` labelled
      "Type" using `FILE_FAMILY_LABELS`, preserving `visibleMIMEFamilies` and
      the effective-families logic.
    - Rename the toggle to "Visual search"; keep its row and disclosure text.
    - Type cell: `fileTypeLabel(row.mime_type, row.mime_family)` with
      `title={row.mime_type || row.mime_family}`.
    - Containing-item cell: keep the title text and add an `IconButton` or
      text button named `Open containing item ${row.containing_title || row.entry_key}`
      calling `onOpenItem?.(row.entry_key)`; the row click handler already
      skips clicks inside buttons.
    - Render the outer element as a `div` when `showHeader` is false and
      `embedded` is true, so the shell's `main` "Files" is the only named
      landmark.
    - Source stays `row.source_identifier`; file rows carry no account display
      name, which the spec allows ("when one is available").
  - Keep column-header sorting; it and the Sort menu drive the same `fileSort`.

- [ ] **Step 4: Run** `bunx vitest run src/lib/components` and `bun run check`.

- [ ] **Step 5: Commit**

Subject: `feat(web): make Files the single file view`.

---

### Task 8: Directory Media & files filters work

**Files:**
- Modify: `web/src/lib/components/directory/PersonDetail.svelte` (~line 154)
- Test: `PersonDetail.test.ts` (find with `rg -l "PersonDetail" src --glob '*.test.ts'`)

- [ ] **Step 1: Write the failing test**

Render `PersonDetail` on the Media & files tab with a fetch mock for the
person files endpoint; type a filename, choose a Type, and click "Sort by
size"; assert each triggers a new files request carrying the filename, family,
and `sort` parameters.

- [ ] **Step 2: Run to verify it fails** (no request changes today).

- [ ] **Step 3: Implement**

Hold `sort`, `filenameQuery`, and `mimeFamilies` in local `$state` in
`PersonDetail` and pass `onSortChange`, `onFilenameQueryChange` (debounced like
Relationships' embed; check `RelationshipsWorkspace.svelte` and copy its
approach), and `onMIMEFamiliesChange`. Reset them when `personID` changes.

- [ ] **Step 4: Run** `bunx vitest run src/lib/components/directory`.

- [ ] **Step 5: Commit**

Subject: `fix(web): make Directory media and file filters take effect`.

---

### Task 9: Save view dialog and saved-view library

**Files:**
- Create: `web/src/lib/saved-views/canonical.ts` (+ `canonical.test.ts`)
- Create: `web/src/lib/components/saved-views/SaveViewDialog.svelte` (+ test)
- Modify: `web/src/lib/components/saved-views/SavedViewsWorkspace.svelte`
- Modify: `EverythingWorkspace.svelte` (header action), `AppShell.svelte`
  (Files header action; stop passing `selection`)
- Test: `SavedViewsWorkspace.test.ts`, `AppShell.test.ts`

**Interfaces:**
- Produces from `canonical.ts`:

```ts
export type CanonicalState = /* moved from SavedViewsWorkspace unchanged */;
export function canonicalSavedViewState(state: ExploreURLState): CanonicalState;
export function exploreStateFromSavedView(saved: CanonicalState): Partial<ExploreURLState>;
export function savedViewSummary(saved: CanonicalState): string[];
```

  `exploreStateFromSavedView` returns `workspace: 'files'` when
  `presentation === 'files'`, else `'everything'`, with the same alias and
  default handling `open()` has today. `savedViewSummary` returns readable
  parts, for example `['Full text: “invoice”', 'Source: 7', 'Grouped by Year', 'Table']`.
- `SaveViewDialog` props: `client`, `state: ExploreURLState`,
  `onSaved: (view: SavedView) => void`, `onclose: () => void`. Kit `Modal`
  titled "Save view"; `TextInput` "Name" and "Description"; submit button
  "Save" (`tone="info" surface="solid"`, disabled until a name is typed); on
  Files (`state.workspace === 'files'`) a note "Filename, type, and file sort
  aren’t saved with the view."; API errors shown in the dialog.

- [ ] **Step 1: Write the failing tests**
- `canonical.test.ts`: round-trip an Everything state and a Files state; the
  Files one opens in `files`; the summary reads as above; legacy aliases
  `source_id` and `participant_id` map as today.
- `SaveViewDialog.test.ts`: Save is disabled with an empty name; saving posts
  the canonical state and calls `onSaved`; the Files note shows only for Files.
- `SavedViewsWorkspace.test.ts`: no "Save this view" form; each view shows its
  summary and the buttons "Open <name>", "Edit <name>", "Delete <name>"
  (names unchanged); the empty state reads "No saved views yet" and points to
  "Save view…" in Everything and Files.
- `AppShell.test.ts`: "Save view…" exists in the Everything and Files headers
  and opens the dialog; saving from Files and opening the view from Saved
  views lands in Files.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**
  - Move `CanonicalState`, `canonicalState()`, and the mapping in `open()`
    into `canonical.ts`; `SavedViewsWorkspace` imports them.
  - `SavedViewsWorkspace`: delete the "Save this view" card, the unused
    `selection` and `currentState` props (confirm with `rg`), and their
    styles; render the summary under each description; keep edit, delete, and
    incompatibility behavior unchanged.
  - Header actions: `Button surface="outline" label="Save view…"` in
    Everything's `PageHeader` and the Files `PageHeader` in AppShell, opening
    `SaveViewDialog` with `exploreState.current`. After saving, announce
    "Saved view <name>." through the existing operation status region.

- [ ] **Step 4: Run** `bunx vitest run src/lib/saved-views src/lib/components/saved-views src/lib/components/shell`.

- [ ] **Step 5: Commit**

Subject: `feat(web): save views from the page they describe`.

---

### Task 10: Escape closes menus before panes

Kit popovers let Escape reach the shell, which then closes the reading pane or
removes a grouping level while the user only meant to close a menu. Task 4
and Task 7 add several menus to Everything and Files, so this becomes common.

**Files:**
- Modify: `web/src/lib/components/shell/AppShell.svelte` (`handleEscape`)
- Test: `AppShell.test.ts`, `web/tests/shell-navigation.spec.ts`

- [ ] **Step 1: Write the failing tests**
- Unit: with a message open in the reading pane, open "Columns", press
  Escape: the menu closes and the reading pane stays. Same for the Sort
  combobox and "More selection actions".
- Unit: after the narrow navigation menu unmounts, Escape closes the reading
  pane again (the root scope is restored).
- Browser: the same Columns case in Chromium.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement**

At the start of `handleEscape`, return when the event target is inside an
open popover — the element that has focus is within `[role="menu"]`,
`[role="listbox"]`, `[role="dialog"]`, or a kit popover container — or when an
expanded trigger (`[aria-expanded="true"]`) has focus. Check how kit's
`dismissable` closes on Escape so the menu still closes; the shell must only
stop acting, not swallow the key.

- [ ] **Step 4: Run** the unit tests and
  `bunx playwright test tests/shell-navigation.spec.ts`.

- [ ] **Step 5: Commit**

Subject: `fix(web): let Escape close menus without closing panes`.

---

### Task 11: Browser tests

**Files:**
- Modify: `web/tests/presentations.spec.ts` (grid "Files in current context"
  and region "Files presentation" → the Files workspace grid "Files results"),
  `web/tests/archive-management.spec.ts` (open-in-source reason text, deletion
  entry), `web/tests/theme-keyboard.spec.ts` (Saved views save flow and any
  `kit-button--workflow` assertion), `web/tests/e2e/accessibility.spec.ts`
  (add the Save view dialog, the Columns and Type menus, and a selected-row
  selection bar), and any spec found in Step 1

- [ ] **Step 1: Find affected specs**

```bash
rg -ln "Files in current context|Files presentation|Open in source:|Save this view|getByLabel\('Name'\)|kit-button--workflow|Columns|MIME families|Hosted visual search|No items selected|Count pending|Newest first" tests
```

- [ ] **Step 2: Update each spec to the new names and flows** without weakening
  assertions. Add browser coverage for: Show as Files round trip and Back;
  an old Everything-as-Files link; Save view from Files then open it; Review
  for deletion from the selection bar.

- [ ] **Step 3: Run** `make web-test-browser` from the repository root.
  Expected: all pass, axe included.

- [ ] **Step 4: Commit**

Subject: `test(web): cover the Everything and Files toolbars and saved views`.
List every existing spec edit and why in the body.

---

### Task 12: Verify and prepare the pull request

- [ ] **Step 1:** From the repository root run
  `make web-check && make web-test && make web-test-browser && make lint-ci`.
- [ ] **Step 2:** Rebuild and capture every workspace (controller helper) plus
  Everything with a selection, the Columns menu open, the Save view dialog,
  Files with the Type menu open, and the login screen at 420px. View every
  image. Check: one toolbar row, count once, no raw codes, no purple or green
  solid buttons on Everything, Files, or Saved views, and the login button is
  not stretched oddly.
- [ ] **Step 3:** Read `git diff origin/main...HEAD` fully; remove leftovers.
- [ ] **Step 4:** Stop. The controller asks the user before pushing or opening
  the pull request.

## Self-review notes

- Spec coverage (delivery item 2): toolbar, chips, Sort, Columns, Preview
  position, count once, notices placement (Task 4); selection bar, Review for
  deletion, Open in source overflow (Task 5); reading-pane header (Task 6);
  one Files view, Show as symmetry, deletion of `FilesPresentation`, row
  action, Type menu, readable Type, Sort, count, Visual search (Task 7);
  normalization of old links, history, saved views (Task 2); Directory media
  filters (Task 8); Save view dialog, library, Files note, unused prop
  (Task 9).
- PR 1 follow-ups folded in: page gutter (Task 1), stale file count, nested
  Files region, live typing on Files test (Task 7), Escape in kit popovers and
  drawer-unmount Escape test (Task 10), purple Save button (Task 9), login
  button width check (Task 12).
- Kit limits: `SegmentedControl` has no icons, so Preview position keeps its
  text options "Below" and "Right" in the toolbar. Kit has no checkbox menu
  item, so Columns and Type use `FilterDropdown`.
- The Tasks count appears after the sheet loads once for a message; fetching
  on every row open only for a badge would add a request per selection.
