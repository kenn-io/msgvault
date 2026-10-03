---
title: "Web UI overhaul PR 4: Manage — implementation plan"
description: "Implementation plan for the Manage workspace in the Web UI."
last_edited: "2026-10-01"
---

# Web UI overhaul PR 4: Manage — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task by task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Sources, Operations, Deletions, and Settings the shared status
vocabulary and page structure, keep Settings' category in the URL, apply
saved appearance to the open tab, and bring the Web UI guide and its
screenshots up to date, without removing any capability.

**Architecture:** Three small modules hold the new shared logic:
`util/format.ts` (one `formatBytes`, date-time, and relative-time format),
`sources/labels.ts` (source types, sync reasons, status chips), and
`operations/labels.ts` (lane, kind, state, trigger, counter, and Set up
labels that four Operations components duplicate today). Operations replaces
its lane cards with a new `OperationStatusList`, and its controller gains a
status-only refresh. Settings reads its category from a new `settingsCategory`
field in the `explore` URL state and reports saved appearance to `App`.

**Tech stack:** Svelte 5 (runes), `@kenn-io/kit-ui` (`RefreshControl`,
`Chip`, `Notice`, `EmptyState`, `Table`, `IconButton`, `Card`),
`@lucide/svelte`, Vitest with Testing Library, Playwright with
`@axe-core/playwright`, Go for one metadata string.

**Spec:** [PR 4 spec](web-ui-overhaul-pr4-spec.md), which refines the Manage
sections of the [Web UI overhaul design](web-ui-overhaul-design.md). Read the
spec before each task; it is the authority where this plan is silent. Exact
copy, accessible names, URL keys, and tones in this plan are copied from it.

## Global constraints

- Keep every capability. A control that moves keeps its accessible name unless
  the spec names the change. The named changes are: "View source operations"
  becomes "Sync history"; "Refresh operations" becomes "Refresh operation
  status" (status) or "Reload run history" (history); "Save settings" becomes
  "Save changes"; "Stage deletion" becomes "Stage deletion…"; "Open document
  index settings" and "Open document vector settings" are replaced by a
  host-configuration line and guide link.
- Keep URL state keys, the `explore` JSON format, and API calls unchanged,
  except: the new `settingsCategory` field (Task 3) and the removal of the
  `document_index` and `document_vector` values of `settingsAuthority`
  (Task 4). An old link carrying one opens Settings on Appearance.
- No new npm dependencies. Icons come from `@lucide/svelte`.
- One solid primary button per screen: `tone="info" surface="solid"`. Purple
  (`workflow`) and green (`success`) are not button colors. Only destructive
  confirmation buttons ("Confirm stage deletion", "Confirm cancel manifest")
  are red.
- Status tones: green (`success`) healthy or finished, blue (`info`) in
  progress, amber (`warning`) attention, red (`danger`) failure, gray
  (`muted`) off. Every Operations status is a kit `Chip` with a word, never a
  dot alone (spec decision 2).
- Sentence case for every visible label. Monospace only for identifiers,
  hashes, code, keys, and cron text.
- Raw API codes are never the only text a person sees. Unknown codes fall back
  to `sentenceCase` from `explore/labels.ts`. Where the spec keeps a raw code
  "in a tooltip", use the element's `title` attribute, as Sources and the
  selection bar already do.
- `kit-ui-check` must pass. It forbids native date inputs, hand-rolled search
  inputs, empty states, icon buttons, status dots, cards (background + border
  + radius on one rule), tooltips, and `@media` widths other than 640, 760,
  and 900px. Use the kit component instead.
- Tests use Vitest and Testing Library or Playwright, query by role and
  accessible name, and use failure-safe cleanup (`afterEach` or
  `try`/`finally`). Synthetic names and `example.com` addresses only; the
  Enron fixture is for docs screenshots only.
- Run commands from `web/` unless a step says otherwise. After each task run
  `bun run check`, `bun run check:kit-ui`, and the focused tests.
- Commit after each task with the `kenn-io-commit` rules: new commits only,
  conventional imperative subject, a short why body, the attribution trailer
  the controller gives you, and a private-data check (public repository). Go
  hooks run on commit; export `GOTOOLCHAIN=go1.27.1` if the shell pins an
  older toolchain.

## File structure

| File | Responsibility |
|---|---|
| `web/src/lib/util/format.ts` (new) | `formatBytes`, `formatDateTime`, `formatRelativeTime` |
| `web/src/lib/components/{files/FilesWorkspace,files/FileViewer,explore/EverythingTable,reader/ReadingPane}.svelte` | Use the shared `formatBytes` |
| `web/src/lib/sources/labels.ts` (new) | Source-type labels, sync-reason labels, sync status chip |
| `web/src/lib/operations/labels.ts` (new) | Lane, kind, related-status, action, state, trigger, counter, duration, and Set up maps |
| `web/src/lib/components/sources/SourcesWorkspace.svelte` | Columns, status chip, reasons, row detail, status sentences |
| `web/src/lib/carddav/navigation.ts` | Settings authorities: add `semantic_search`, `person_embeddings`; remove `document_index`, `document_vector` |
| `web/src/lib/explore/{models.ts,state.svelte.ts}` | `settingsCategory` URL field |
| `web/src/lib/components/settings/SettingsWorkspace.svelte` | Category from props, save bar, posture copy, own-save notes, Notice, appearance callback |
| `web/src/lib/components/operations/OperationStatusList.svelte` (new) | Status list replacing `OperationLaneCards.svelte` (deleted) |
| `web/src/lib/components/operations/OperationHostSetup.svelte` (new) | Host-configuration line and guide link |
| `web/src/lib/components/operations/{OperationsWorkspace,OperationRunTable,OperationRunDetail,OperationRelatedStatus}.svelte` | Refresh control, Reload run history, runs table, detail, document panels |
| `web/src/lib/operations/{controller.svelte.ts,models.ts}` | `refreshStatus`, `statusUpdatedAt`, `statusRefreshing` |
| `web/src/lib/components/shell/PageHeader.svelte` | Optional `descriptionContent` snippet |
| `web/src/lib/components/deletions/DeletionsWorkspace.svelte` | Empty states, review panel, manifests table and detail |
| `web/src/lib/components/explore/SelectionBar.svelte` | `stage_deletion` reason |
| `web/src/lib/theme/preferences.svelte.ts` | `SavedAppearance`, `mergeSavedAppearance` |
| `web/src/App.svelte`, `web/src/lib/components/shell/AppShell.svelte` | Category and appearance wiring; boot screens |
| `web/src/lib/components/auth/Login.svelte`, `web/src/app.css` | Shared sign-in and boot screen rule |
| `internal/api/settings_metadata.go:100` | Default search mode description |
| `web/tests/**` | Browser and axe coverage |
| `docs/web-ui.md` | Guide for the shipped UI |

---

### Task 1: Shared format and label helpers

**Files:**
- Create: `web/src/lib/util/format.ts`, `web/src/lib/util/format.test.ts`
- Create: `web/src/lib/sources/labels.ts`, `web/src/lib/sources/labels.test.ts`
- Create: `web/src/lib/operations/labels.ts`, `web/src/lib/operations/labels.test.ts`
- Modify (delete the local `formatBytes`, import the shared one):
  `web/src/lib/components/files/FilesWorkspace.svelte:8-12`,
  `web/src/lib/components/files/FileViewer.svelte:2-6` (the function lives in
  the `<script module>` block; put the import there),
  `web/src/lib/components/explore/EverythingTable.svelte:242-246`,
  `web/src/lib/components/reader/ReadingPane.svelte:234-238`
- Modify (use the shared maps; delete the local copies):
  `web/src/lib/components/operations/OperationsWorkspace.svelte:62-81,91-101`
  (`laneOptions`, `kindOptions`, `kindLabels`),
  `OperationLaneCards.svelte:26-55,69-75` (`laneLabels`, `kindLabels`,
  `relatedLabels`, `actionLabels`, `formatTimestamp`),
  `OperationRunTable.svelte:19-28,45-61` (`kindLabels`, `formatTimestamp`,
  `duration`),
  `OperationRunDetail.svelte:26-48,63-82` (`kindLabels`, `relatedLabels`,
  `actionLabels`, `formatTimestamp`, `duration`). `titleCase` and `dotStatus`
  stay until Tasks 4 and 5 replace them with chips.

**Interfaces:**
- Produces, in `util/format.ts`:
  - `formatBytes(value: number): string` — "120 B", "3 KB", "1.5 MB" (the
    existing rule, unchanged).
  - `formatDateTime(value: string | null | undefined, timeStyle?: 'short' | 'long'): string` —
    `Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle })`;
    "Not available" when missing or unparseable.
  - `formatRelativeTime(value: string, now?: Date): string` — "in 2 hours",
    "in 5 minutes", "3 minutes ago", "now"; returns the input unchanged when
    it does not parse.
- Produces, in `sources/labels.ts`:
  - `sourceTypeLabel(code: string): string`
  - `syncUnavailableLabel(code: string | undefined): string`
  - `syncStatusChip(source: Pick<SourceStatus, 'active_sync' | 'latest_sync'>): { label: string; tone: ChipTone }`
- Produces, in `operations/labels.ts`:
  - `type RelatedStatus = NonNullable<OperationLaneStatus['related_status']>`
  - `OPERATION_LANE_LABELS: Readonly<Record<OperationLane, string>>`
  - `OPERATION_KIND_LABELS: Readonly<Record<OperationKind, string>>` (key
    order is the Kind filter's option order)
  - `RELATED_STATUS_LABELS: Readonly<Record<RelatedStatus, string>>`
  - `OPERATION_ACTION_LABELS: Readonly<Record<OperationAction, string>>`
  - `operationStateChip(state: string): { label: string; tone: ChipTone }`
  - `triggerLabel(trigger: string | undefined): string`
  - `counterSummary(counters: readonly OperationPublicCounter[]): string`
  - `counterLabel(name: string): string`, `counterValue(counter: OperationPublicCounter): string`
  - `operationDuration(run: Pick<OperationRunSummary, 'state' | 'started_at' | 'finished_at'>): string`

- [ ] **Step 1: Write the failing tests**

`util/format.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { formatBytes, formatDateTime, formatRelativeTime } from './format';

describe('format', () => {
  it.each([[120, '120 B'], [3 * 1024, '3 KB'], [1.5 * 1024 * 1024, '1.5 MB']])('formats %d bytes', (value, want) => {
    expect(formatBytes(value)).toBe(want);
  });
  it('names missing and unparseable times', () => {
    expect(formatDateTime(undefined)).toBe('Not available');
    expect(formatDateTime('not a time')).toBe('Not available');
    expect(formatDateTime('2026-07-19T10:00:00Z')).toMatch(/2026/);
  });
  it('states time relative to now', () => {
    const now = new Date('2026-07-19T10:00:00Z');
    expect(formatRelativeTime('2026-07-19T12:00:00Z', now)).toBe('in 2 hours');
    expect(formatRelativeTime('2026-07-19T10:05:00Z', now)).toBe('in 5 minutes');
    expect(formatRelativeTime('2026-07-19T09:57:00Z', now)).toBe('3 minutes ago');
    expect(formatRelativeTime('2026-07-19T10:00:00Z', now)).toBe('now');
    expect(formatRelativeTime('soon', now)).toBe('soon');
  });
});
```

`sources/labels.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { sourceTypeLabel, syncStatusChip, syncUnavailableLabel } from './labels';

const run = (status: string, errors = 0) => ({
  id: 1, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: null, status,
  messages_processed: 1, messages_added: 1, messages_updated: 0, errors_count: errors, error_message: null
});

describe('source labels', () => {
  it.each([
    ['gmail', 'Gmail'], ['imap', 'IMAP'], ['msmail', 'Microsoft mail'], ['teams', 'Teams'],
    ['discord', 'Discord'], ['meeting_import', 'Meeting import'], ['synctech_sms', 'SMS backup'],
    ['imazing_csv', 'iMazing CSV'], ['circleback', 'Circleback'], ['gcal', 'Google Calendar'],
    ['muesli', 'Muesli'], ['granola', 'Granola'], ['notion_meetings', 'Notion meetings'],
    ['pst', 'PST import'], ['apple-mail', 'Apple Mail'], ['mbox', 'Mbox import'],
    ['', 'Gmail'], ['future_source', 'Future source'], ['constructor', 'Constructor']
  ])('names source type %j', (code, want) => expect(sourceTypeLabel(code)).toBe(want));

  it.each([
    ['source_not_schedulable', 'Imported file — nothing to sync'],
    ['sync_already_running', 'Sync in progress'],
    ['scheduler_unavailable', 'Scheduler unavailable'],
    ['sync_not_configured', 'Sync not set up'],
    ['sync_unavailable', 'Sync unavailable'],
    ['future_reason', 'Sync unavailable'],
    [undefined, 'Sync unavailable']
  ])('names sync reason %j', (code, want) => expect(syncUnavailableLabel(code)).toBe(want));

  it('maps each latest result to one status chip', () => {
    expect(syncStatusChip({ active_sync: run('running'), latest_sync: run('completed') }))
      .toEqual({ label: 'Syncing', tone: 'info' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('completed') }))
      .toEqual({ label: 'Completed', tone: 'success' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('completed', 2) }))
      .toEqual({ label: 'Completed with errors', tone: 'warning' });
    expect(syncStatusChip({ active_sync: null, latest_sync: run('failed') }))
      .toEqual({ label: 'Failed', tone: 'danger' });
    expect(syncStatusChip({ active_sync: null, latest_sync: null }))
      .toEqual({ label: 'Never synced', tone: 'muted' });
  });
});
```

`operations/labels.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { counterLabel, counterSummary, counterValue, operationDuration, operationStateChip, triggerLabel } from './labels';

describe('operation labels', () => {
  it.each([
    ['queued', 'Queued', 'info'], ['running', 'Running', 'info'], ['succeeded', 'Succeeded', 'success'],
    ['partial', 'Partial', 'warning'], ['failed', 'Failed', 'danger'], ['cancelled', 'Cancelled', 'muted'],
    ['future_state', 'Future state', 'neutral']
  ])('chips state %s', (state, label, tone) => expect(operationStateChip(state)).toEqual({ label, tone }));

  it('names triggers and shows a dash when none was recorded', () => {
    expect(triggerLabel('manual')).toBe('Manual');
    expect(triggerLabel('scheduled')).toBe('Scheduled');
    expect(triggerLabel(undefined)).toBe('—');
  });

  it('names each unit once, on its first counter, and leaves out zeros', () => {
    expect(counterSummary([
      { name: 'processed', unit: 'messages', value: 20 },
      { name: 'added', unit: 'messages', value: 20 },
      { name: 'item_errors', unit: 'messages', value: 0 },
      { name: 'updated', unit: 'people', value: 3 }
    ])).toBe('20 messages processed · 20 added · 3 people updated');
  });

  it('reads counters named for their unit and single values naturally', () => {
    expect(counterSummary([{ name: 'projected_writes', unit: 'writes', value: 3 }])).toBe('3 projected writes');
    expect(counterSummary([{ name: 'books', unit: 'books', value: 1 }])).toBe('1 book');
    expect(counterSummary([{ name: 'processed', unit: 'messages', value: 1 }])).toBe('1 message processed');
  });

  it('says No counters when nothing is left to show', () => {
    expect(counterSummary([])).toBe('No counters');
    expect(counterSummary([{ name: 'failed', unit: 'messages', value: 0 }])).toBe('No counters');
  });

  it('labels detail counters in sentence case with every value', () => {
    expect(counterLabel('item_errors')).toBe('Item errors');
    expect(counterValue({ name: 'item_errors', unit: 'messages', value: 0 })).toBe('0 messages');
  });

  it('measures durations', () => {
    expect(operationDuration({ state: 'succeeded', started_at: '2026-08-30T10:00:00Z', finished_at: '2026-08-30T10:01:05Z' }))
      .toBe('1 minute 5 seconds');
    expect(operationDuration({ state: 'running', started_at: '2026-08-30T10:00:00Z' })).toBe('In progress');
    expect(operationDuration({ state: 'failed', started_at: '2026-08-30T10:00:00Z' })).toBe('Not available');
  });
});
```

- [ ] **Step 2: Run** `bunx vitest run src/lib/util/format.test.ts src/lib/sources src/lib/operations/labels.test.ts`.
  Expected: FAIL, modules not found.

- [ ] **Step 3: Implement**

`util/format.ts`:

```ts
export function formatBytes(value: number): string {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${Math.round(value / 1024)} KB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MB`;
}

export function formatDateTime(value: string | null | undefined, timeStyle: 'short' | 'long' = 'short'): string {
  if (!value) return 'Not available';
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return 'Not available';
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle }).format(date);
}

const RELATIVE_UNITS: ReadonlyArray<readonly [Intl.RelativeTimeFormatUnit, number]> = [
  ['day', 86_400_000], ['hour', 3_600_000], ['minute', 60_000], ['second', 1_000]
];

export function formatRelativeTime(value: string, now: Date = new Date()): string {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return value;
  const difference = date.getTime() - now.getTime();
  const format = new Intl.RelativeTimeFormat('en-US', { numeric: 'auto' });
  for (const [unit, size] of RELATIVE_UNITS) {
    if (Math.abs(difference) >= size) return format.format(Math.round(difference / size), unit);
  }
  return format.format(0, 'second');
}
```

`sources/labels.ts`:

```ts
import type { ChipTone } from '@kenn-io/kit-ui';
import type { SourceStatus } from '../api/generated/models';
import { sentenceCase } from '../explore/labels';

// Source types the daemon stores (internal/api/scheduler_jobs.go and the
// importers). An empty type is a legacy Gmail account.
const SOURCE_TYPES: Readonly<Record<string, string>> = {
  '': 'Gmail', gmail: 'Gmail', imap: 'IMAP', msmail: 'Microsoft mail', teams: 'Teams',
  discord: 'Discord', meeting_import: 'Meeting import', synctech_sms: 'SMS backup',
  imazing_csv: 'iMazing CSV', circleback: 'Circleback', gcal: 'Google Calendar', muesli: 'Muesli',
  granola: 'Granola', notion_meetings: 'Notion meetings', pst: 'PST import',
  'apple-mail': 'Apple Mail', mbox: 'Mbox import'
};
const SYNC_UNAVAILABLE: Readonly<Record<string, string>> = {
  source_not_schedulable: 'Imported file — nothing to sync',
  sync_already_running: 'Sync in progress',
  scheduler_unavailable: 'Scheduler unavailable',
  sync_not_configured: 'Sync not set up'
};

export function sourceTypeLabel(code: string): string {
  return Object.hasOwn(SOURCE_TYPES, code) ? SOURCE_TYPES[code]! : sentenceCase(code);
}

export function syncUnavailableLabel(code: string | undefined): string {
  return code !== undefined && Object.hasOwn(SYNC_UNAVAILABLE, code) ? SYNC_UNAVAILABLE[code]! : 'Sync unavailable';
}

export function syncStatusChip(
  source: Pick<SourceStatus, 'active_sync' | 'latest_sync'>
): { label: string; tone: ChipTone } {
  const latest = source.latest_sync;
  if (source.active_sync || latest?.status === 'running') return { label: 'Syncing', tone: 'info' };
  if (!latest) return { label: 'Never synced', tone: 'muted' };
  if (latest.status === 'completed') {
    return latest.errors_count > 0
      ? { label: 'Completed with errors', tone: 'warning' }
      : { label: 'Completed', tone: 'success' };
  }
  if (latest.status === 'failed') return { label: 'Failed', tone: 'danger' };
  return { label: sentenceCase(latest.status), tone: 'neutral' };
}
```

`operations/labels.ts`:

```ts
import type { ChipTone } from '@kenn-io/kit-ui';
import type { OperationPublicCounter } from '../api/generated/models';
import { sentenceCase } from '../explore/labels';
import type { OperationAction, OperationKind, OperationLane, OperationLaneStatus, OperationRunSummary } from './models';

export type RelatedStatus = NonNullable<OperationLaneStatus['related_status']>;

export const OPERATION_LANE_LABELS: Readonly<Record<OperationLane, string>> = {
  messages: 'Messages', person_facts: 'Facts', contacts: 'Contacts',
  documents: 'Documents', visual_attachments: 'Attachments'
};
export const OPERATION_KIND_LABELS: Readonly<Record<OperationKind, string>> = {
  source_sync: 'Source sync', message_embedding: 'Message embedding', person_sweep: 'Person fact sweep',
  person_embedding: 'Person embedding', person_enrichment: 'Person enrichment', carddav_sync: 'CardDAV sync',
  document_extraction: 'Document extraction', document_embedding: 'Document embedding',
  visual_embedding: 'Visual embedding'
};
export const RELATED_STATUS_LABELS: Readonly<Record<RelatedStatus, string>> = {
  listSourceStatus: 'Sources status', getDocumentIndexStatus: 'Document index status',
  getDocumentVectorStatus: 'Document vector status', getVisualAttachmentStatus: 'Visual attachment status',
  getCardDAVStatus: 'CardDAV settings'
};
export const OPERATION_ACTION_LABELS: Readonly<Record<OperationAction, string>> = {
  carddav_sync: 'Start CardDAV sync', visual_build: 'Build visual index', visual_resume: 'Resume visual index'
};

const STATE_CHIPS: Readonly<Record<string, { label: string; tone: ChipTone }>> = {
  queued: { label: 'Queued', tone: 'info' },
  running: { label: 'Running', tone: 'info' },
  succeeded: { label: 'Succeeded', tone: 'success' },
  partial: { label: 'Partial', tone: 'warning' },
  failed: { label: 'Failed', tone: 'danger' },
  cancelled: { label: 'Cancelled', tone: 'muted' }
};

export function operationStateChip(state: string): { label: string; tone: ChipTone } {
  return Object.hasOwn(STATE_CHIPS, state) ? STATE_CHIPS[state]! : { label: sentenceCase(state), tone: 'neutral' };
}

export function triggerLabel(trigger: string | undefined): string {
  if (!trigger) return '—';
  if (trigger === 'manual') return 'Manual';
  if (trigger === 'scheduled') return 'Scheduled';
  return sentenceCase(trigger);
}

// Units come from the closed counter registry in internal/operations/types.go.
const UNIT_SINGULAR: Readonly<Record<string, string>> = {
  messages: 'message', people: 'person', writes: 'write', books: 'book', contacts: 'contact',
  documents: 'document', chunks: 'chunk', attachments: 'attachment'
};

function unitWord(unit: string, value: number): string {
  return value === 1 && Object.hasOwn(UNIT_SINGULAR, unit) ? UNIT_SINGULAR[unit]! : unit;
}

function counterPhrase(counter: OperationPublicCounter, nameUnit: boolean): string {
  const value = counter.value.toLocaleString();
  const words = counter.name.replaceAll('_', ' ');
  // A counter named for its unit ("books", "projected writes") already says what it counts.
  if (words.split(' ').at(-1) === counter.unit) {
    return `${value} ${words.replace(/\S+$/, unitWord(counter.unit, counter.value))}`;
  }
  return nameUnit ? `${value} ${unitWord(counter.unit, counter.value)} ${words}` : `${value} ${words}`;
}

export function counterSummary(counters: readonly OperationPublicCounter[]): string {
  const namedUnits = new Set<string>();
  const parts: string[] = [];
  for (const counter of counters) {
    if (counter.value === 0) continue;
    parts.push(counterPhrase(counter, !namedUnits.has(counter.unit)));
    namedUnits.add(counter.unit);
  }
  return parts.length > 0 ? parts.join(' · ') : 'No counters';
}

export const counterLabel = (name: string): string => sentenceCase(name);

export function counterValue(counter: OperationPublicCounter): string {
  return `${counter.value.toLocaleString()} ${unitWord(counter.unit, counter.value)}`;
}

function plural(value: number, unit: string): string {
  return `${value} ${value === 1 ? unit : `${unit}s`}`;
}

export function operationDuration(run: Pick<OperationRunSummary, 'state' | 'started_at' | 'finished_at'>): string {
  if (!run.finished_at) return run.state === 'running' ? 'In progress' : 'Not available';
  const milliseconds = Date.parse(run.finished_at) - Date.parse(run.started_at);
  if (!Number.isFinite(milliseconds) || milliseconds < 0) return 'Not available';
  const totalSeconds = Math.floor(milliseconds / 1_000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return plural(seconds, 'second');
  return seconds ? `${plural(minutes, 'minute')} ${plural(seconds, 'second')}` : plural(minutes, 'minute');
}
```

Then refactor the components listed under **Files**:

- `OperationsWorkspace.svelte`: build the filter options from the maps:
  `const laneOptions = [{ value: '', label: 'All lanes' }, ...Object.entries(OPERATION_LANE_LABELS).map(([value, label]) => ({ value, label }))];`
  and the same for `kindOptions` with "All kinds". Replace `kindLabels[...]`
  with `OPERATION_KIND_LABELS[...]`.
- `OperationLaneCards.svelte`, `OperationRunTable.svelte`,
  `OperationRunDetail.svelte`: import the maps, `formatDateTime`, and
  `operationDuration` (detail uses `formatDateTime(value, 'long')`). Rendered
  text stays the same except that an unparseable run time now reads "Not
  available" instead of "Time unavailable".

- [ ] **Step 4: Run** `bunx vitest run src/lib/util src/lib/sources src/lib/operations src/lib/components/operations src/lib/components/files src/lib/components/explore src/lib/components/reader`,
  then `bun run check` and `bun run check:kit-ui`. Expected: PASS, no findings.
- [ ] **Step 5: Commit.** Subject: `refactor(web): share byte, time, source, and operation labels`.

---

### Task 2: Sources

**Files:**
- Modify: `web/src/lib/components/sources/SourcesWorkspace.svelte` (header
  action :307, notices :322-325, table :329-436, helpers :260-282, styles
  :488-515)
- Test: `web/src/lib/components/sources/SourcesWorkspace.test.ts`,
  `web/src/lib/components/shell/AppShell.test.ts:967`

**Interfaces:**
- Consumes: `sourceTypeLabel`, `syncUnavailableLabel`, `syncStatusChip`
  (Task 1), `formatDateTime` (Task 1).
- Produces: nothing new for later tasks.

Target behavior (spec "Sources"):

- Header action: `<Button size="sm" surface="soft" label="Sync history" onclick={onOpenOperations} />`.
- Columns: `Source`, `Schedule`, `Status`, `Last successful sync`, `Action`
  ("Latest result" becomes "Status").
- Source cell: optional disclosure button, display name, then
  `{sourceTypeLabel(source.source_type)} · {source.identifier}`, then the
  existing "Updated" line using `formatDateTime`.
- Schedule cell: delete the visible cron span (:355) and the scheduler error
  (:369-370, it moves to the detail row). `<strong title={source.schedule}>{scheduleSummary(source.schedule)}</strong>`,
  "Schedule unavailable", the "Next" line, "Not scheduled", and "On demand ·
  imported through the API" stay.
- Status cell: one `Chip` from `syncStatusChip(source)`; while syncing, the
  existing progress lines ("N processed", "N added · N errors") under it;
  otherwise the latest result time, then
  `<span class="stale">This result may be out of date.</span>` when
  `staleLastResult(source)`. Delete the Spinner, `statusLabel`, `statusTone`,
  "No prior sync result", and the inline error copy and `<details>`.
- Action cell: "Sync now {name}" and "On-demand API source" unchanged;
  otherwise
  `{@const reason = source.sync_unavailable_reason ?? 'sync_unavailable'}<span class="reason" title={reason}>{syncUnavailableLabel(reason)}</span>`.
  `.reason` uses `color: var(--text-muted)`, not red.
- Row detail: `const expanded = new SvelteSet<number>()` (from
  `svelte/reactivity`). A source has details when
  `Boolean(source.latest_sync?.error_message || source.latest_sync?.item_errors?.length || source.scheduler_last_error)`.
  Only then the Source cell starts with:

```svelte
<IconButton
  size="sm"
  ariaLabel={`Show details for ${label(source)}`}
  ariaExpanded={expanded.has(source.id)}
  ariaControls={expanded.has(source.id) ? detailID : undefined}
  onclick={() => (expanded.has(source.id) ? expanded.delete(source.id) : expanded.add(source.id))}
>
  {#if expanded.has(source.id)}<ChevronDown size={14} aria-hidden="true" />{:else}<ChevronRight size={14} aria-hidden="true" />{/if}
</IconButton>
```

  with `detailID = \`source-${source.id}-details\``. After the row, while
  expanded:

```svelte
<tr class="detail-row" id={detailID}>
  <td colspan="5">
    <div class="details">
      {#if source.latest_sync?.error_message}<p class="error-copy">{source.latest_sync.error_message}</p>{/if}
      {#if source.scheduler_last_error}<p class="error-copy">Scheduler: {source.scheduler_last_error}</p>{/if}
      {#if source.latest_sync?.item_errors?.length}
        {@const count = source.latest_sync.item_errors.length}
        <p>{count} item {count === 1 ? 'error' : 'errors'}</p>
        <ul>
          {#each source.latest_sync.item_errors as item (`${item.source_message_id}:${item.phase}:${item.created_at}`)}
            <li class="error-copy">{item.error_message}</li>
          {/each}
        </ul>
      {/if}
    </div>
  </td>
</tr>
```

  `aria-controls` is set only while the detail row exists, so axe never sees
  a dangling reference.
- Status sentences: replace the `not_observed` paragraph (:323-325) with

```svelte
<div class="notice" role="status">
  <span>The sync was requested, but it hasn't started yet. Refresh to check again.</span>
  <Button size="sm" surface="soft" label="Refresh" onclick={refresh} />
</div>
```

  Status notices (`.notice` without `--error`) use a neutral border
  (`var(--border-default)`) and background (`var(--bg-subtle)`); errors keep
  `.notice--error`. `.stale` uses `color: var(--status-warning-ink)`.
- Delete `.working` and `.schedule-expression` rules and the `Spinner`
  import.

- [ ] **Step 1: Write the failing tests** (`SourcesWorkspace.test.ts`; use the
  file's `source()` and `run()` helpers)

```ts
it('opens source-sync history from Sync history', async () => {
  const onOpenOperations = vi.fn();
  render(SourcesWorkspace, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ sources: [] }))), onOpenOperations });
  await fireEvent.click(screen.getByRole('button', { name: 'Sync history' }));
  expect(onOpenOperations).toHaveBeenCalledOnce();
});

it('shows a readable type and a muted reason with the raw code in its tooltip', async () => {
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [
    source({ id: 1, source_type: 'mbox', identifier: 'import@example.com', display_name: 'Imported',
      can_sync: false, sync_unavailable_reason: 'source_not_schedulable' }),
    source({ id: 2, identifier: 'busy@example.com', display_name: 'Busy', can_sync: false,
      sync_unavailable_reason: 'sync_already_running' }),
    source({ id: 3, identifier: 'down@example.com', display_name: 'Down', can_sync: false,
      sync_unavailable_reason: 'scheduler_unavailable' }),
    source({ id: 4, identifier: 'unset@example.com', display_name: 'Unset', can_sync: false,
      sync_unavailable_reason: 'sync_not_configured' }),
    source({ id: 5, identifier: 'other@example.com', display_name: 'Other', can_sync: false,
      sync_unavailable_reason: 'future_reason' })
  ] }));
  render(SourcesWorkspace, { client: createAPIClient(fetchFn) });

  expect(await screen.findByText('Mbox import · import@example.com')).toBeDefined();
  for (const [text, code] of [
    ['Imported file — nothing to sync', 'source_not_schedulable'],
    ['Sync in progress', 'sync_already_running'],
    ['Scheduler unavailable', 'scheduler_unavailable'],
    ['Sync not set up', 'sync_not_configured'],
    ['Sync unavailable', 'future_reason']
  ]) {
    expect(screen.getByText(text).getAttribute('title')).toBe(code);
  }
  expect(screen.queryByText('source_not_schedulable')).toBeNull();
  expect(screen.getByRole('columnheader', { name: 'Status' })).toBeDefined();
});

it('shows one status chip per latest result', async () => {
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [
    source({ id: 1, display_name: 'Active', active_sync: run('running', 5) }),
    source({ id: 2, identifier: 'b@example.com', display_name: 'Done', latest_sync: run('completed', 3, { completed_at: '2026-07-19T11:00:00Z' }) }),
    source({ id: 3, identifier: 'c@example.com', display_name: 'Partial', latest_sync: run('completed', 3, { errors_count: 2 }) }),
    source({ id: 4, identifier: 'd@example.com', display_name: 'Broken', latest_sync: run('failed', 0) }),
    source({ id: 5, identifier: 'e@example.com', display_name: 'New' })
  ] }));
  render(SourcesWorkspace, { client: createAPIClient(fetchFn), now: () => new Date('2026-07-19T12:00:00Z') });

  expect(await screen.findByText('Syncing')).toBeDefined();
  expect(screen.getByText('5 processed')).toBeDefined();
  for (const label of ['Completed', 'Completed with errors', 'Failed', 'Never synced']) {
    expect(screen.getByText(label)).toBeDefined();
  }
});

it('expands error details only for rows that have them', async () => {
  const failed = run('failed', 10, {
    completed_at: '2026-07-19T11:30:00Z', error_message: 'Mailbox unavailable', errors_count: 1,
    item_errors: [{ source_message_id: 'm-1', phase: 'ingest', error_kind: 'mime_error',
      error_message: 'Malformed MIME header', created_at: '2026-07-19T11:30:00Z' }]
  });
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [
    source({ display_name: 'Archive', latest_sync: failed, scheduler_last_error: 'Lock timeout' }),
    source({ id: 2, identifier: 'clean@example.com', display_name: 'Clean', latest_sync: run('completed', 1) })
  ] }));
  render(SourcesWorkspace, { client: createAPIClient(fetchFn), now: () => new Date('2026-07-19T12:00:00Z') });

  const toggle = await screen.findByRole('button', { name: 'Show details for Archive' });
  expect(screen.queryByRole('button', { name: 'Show details for Clean' })).toBeNull();
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  expect(screen.queryByText('Malformed MIME header')).toBeNull();
  await fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  expect(screen.getByText('Mailbox unavailable')).toBeDefined();
  expect(screen.getByText('Scheduler: Lock timeout')).toBeDefined();
  expect(screen.getByText('1 item error')).toBeDefined();
  expect(screen.getByText('Malformed MIME header')).toBeDefined();
});

it('keeps the cron text in the schedule tooltip only', async () => {
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ sources: [source({
    scheduled: true, schedule: '0 */6 * * *', next_sync_at: '2026-07-19T18:00:00Z'
  })] }));
  render(SourcesWorkspace, { client: createAPIClient(fetchFn) });
  const summary = await screen.findByText('At :00 past every 6th hour');
  expect(summary.getAttribute('title')).toBe('0 */6 * * *');
  expect(screen.queryByText('0 */6 * * *')).toBeNull();
});
```

Update existing tests: "View source operations" (:35) → the new test above
replaces it; `getByText('sync_not_configured')` (:54) → `getByText('Sync not set up')`;
"No prior sync result" (:56) → `getAllByText('Never synced')`; :92-97 drop
`getByText('0 */6 * * *')` and open "Show details for Archive" before
asserting the item error; :108 `findByText('stale_last_result')` →
`findByText('This result may be out of date.')`; :211 and :232
`sync_start_not_observed` → the sentence "The sync was requested, but it
hasn't started yet. Refresh to check again." `AppShell.test.ts:967`: "View
source operations" → "Sync history".

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/sources src/lib/components/shell/AppShell.test.ts`.
  Expected: new and edited tests FAIL.
- [ ] **Step 3: Implement** the markup above.
- [ ] **Step 4: Run tests, `bun run check`, `bun run check:kit-ui`.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): give Sources readable status, reasons, and row details`.

---

### Task 3: Settings category in the URL

**Files:**
- Modify: `web/src/lib/explore/models.ts:170` (add `settingsCategory: string;`)
- Modify: `web/src/lib/explore/state.svelte.ts` (`RESTORATION_INVALIDATING_FIELDS`
  :61-98, `defaultExploreURLState` :180, `normalize` :510,
  `WORKSPACE_FIELDS` :528-563)
- Modify: `web/src/lib/carddav/navigation.ts:1-11` (add two authorities)
- Modify: `web/src/lib/components/settings/SettingsWorkspace.svelte` (props
  :82-94, `activeCategory` :106, effects :127-154, layout :616-621)
- Modify: `web/src/lib/components/shell/AppShell.svelte` (`settings` snippet
  type :89-93, `openCardDAVConflict` :278, `openCardDAVSettings` :284, render
  :1304)
- Modify: `web/src/App.svelte:96-104`
- Test: `web/src/lib/explore/state.test.ts`,
  `web/src/lib/components/settings/SettingsWorkspace.test.ts`,
  `web/src/lib/components/shell/AppShell.test.ts`

**Interfaces:**
- Produces:
  - `ExploreURLState.settingsCategory: string`, default `'browser'`, shared in
    links only for `workspace: 'settings'`.
  - `SettingsNavigationAuthority` gains `'semantic_search'`
    (`{ categoryID: 'search', settingKey: 'vector.enabled' }`) and
    `'person_embeddings'` (`{ categoryID: 'search', settingKey: 'vector.people.enabled' }`).
    `document_index` and `document_vector` stay until Task 4.
  - `SettingsWorkspace` props `category?: string` (default `'browser'`) and
    `onCategoryChange?: (categoryID: string) => void`.
  - AppShell's `settings` snippet takes five arguments:
    `(cardDAVRequest, onCardDAVRequestConsumed, navigationTarget, category: string, onCategoryChange: (categoryID: string) => void)`.

Rules:

- `normalize` derives the category:

```ts
const SETTINGS_CATEGORY_PATTERN = /^[a-z][a-z0-9_-]{0,63}$/;

// A Settings authority link chooses its category; otherwise keep a
// well-formed category id. Settings shows Appearance for ids the daemon
// does not list.
function settingsCategory(value: unknown, authority: SettingsNavigationAuthority | ''): string {
  const target = settingsNavigationTarget(authority);
  if (target) return target.categoryID;
  return typeof value === 'string' && SETTINGS_CATEGORY_PATTERN.test(value) ? value : 'browser';
}
```

  In `normalize`, compute `const settingsAuthority = normalizeSettingsNavigationAuthority(value.settingsAuthority);`
  once and set both `settingsAuthority` and
  `settingsCategory: settingsCategory(value.settingsCategory, settingsAuthority)`.
  Add `'settingsCategory'` to `RESTORATION_INVALIDATING_FIELDS` and
  `settingsCategory: ['settings']` to `WORKSPACE_FIELDS`. `commitWorkspace`
  does not reset it, so returning to Settings shows the last category.
- `SettingsWorkspace`:

```ts
let activeCategory = $state(untrack(() => category));
$effect(() => {
  activeCategory = category;
});
const resolvedCategory = $derived(
  categories.some((candidate) => candidate.id === activeCategory) ? activeCategory : 'browser'
);
function selectCategory(categoryID: string): void {
  activeCategory = categoryID;
  onCategoryChange(categoryID);
}
```

  `<SettingsLayout {categories} bind:active={() => resolvedCategory, selectCategory} ...>`;
  the `footer` condition reads `resolvedCategory`. In the navigation-target
  effect keep `activeCategory = target.categoryID` and the focus logic, and
  delete the branch that resets `activeCategory = 'browser'` (:140-143); keep
  only `focusedNavigationSettingKey = undefined` there.
- AppShell:

```ts
function selectSettingsCategory(categoryID: string): void {
  commitNavigation({ settingsCategory: categoryID, settingsAuthority: '' });
}
```

  Render `settings(cardDAVSettingsRequest, consumeCardDAVSettingsRequest, settingsNavigationTarget, exploreState.current.settingsCategory, selectSettingsCategory)`.
  In `openCardDAVConflict` and `openCardDAVSettings`, follow
  `commitWorkspace('settings')` with
  `replaceCommittedNavigation({ settingsCategory: 'carddav' })` so the URL
  names the category the request opens without a second history entry.
- `App.svelte`: the snippet gains `category, onCategoryChange` and passes
  `{category} {onCategoryChange}` to `SettingsWorkspace`.

- [ ] **Step 1: Write the failing tests**

`state.test.ts`:

```ts
it('round-trips the Settings category and shares it only from Settings', () => {
  const restored = parseExploreURLState(serializeExploreURLState({
    ...defaultExploreURLState, workspace: 'settings', settingsCategory: 'search'
  }));
  expect(restored.settingsCategory).toBe('search');
  const elsewhere = serializeExploreURLState({ ...defaultExploreURLState, workspace: 'everything', settingsCategory: 'search' });
  expect(elsewhere).not.toContain('settingsCategory');
  for (const invalid of ['<script>', 42, '', 'Search']) {
    expect(parseExploreURLState(serializeExploreURLState({
      ...defaultExploreURLState, workspace: 'settings', settingsCategory: invalid
    } as unknown as ExploreURLState)).settingsCategory).toBe('browser');
  }
});

it('lets a Settings authority choose its category', () => {
  const restored = parseExploreURLState(serializeExploreURLState({
    ...defaultExploreURLState, workspace: 'settings', settingsAuthority: 'person_embeddings', settingsCategory: 'server'
  }));
  expect(restored.settingsCategory).toBe('search');
});

it('restores the Settings category on Back', async () => {
  window.history.replaceState(null, '', '/');
  const state = new ExploreState(window);
  try {
    state.commitNavigation({ workspace: 'settings', settingsCategory: 'search' });
    state.commitNavigation({ settingsCategory: 'server' });
    const restored = new Promise((resolve) => window.addEventListener('popstate', resolve, { once: true }));
    window.history.back();
    await restored;
    expect(state.current.settingsCategory).toBe('search');
  } finally {
    state.destroy();
  }
});
```

Extend "round-trips only closed Settings authorities" (:222) to include
`semantic_search` and `person_embeddings`.

`SettingsWorkspace.test.ts`:

```ts
it('opens the category it is given, reports changes, and falls back to Appearance', async () => {
  const onCategoryChange = vi.fn();
  const client = createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"')));
  const rendered = render(SettingsWorkspace, { client, category: 'search', onCategoryChange });
  expect(await screen.findByRole('heading', { level: 2, name: 'Search' })).toBeDefined();
  await openSettingsCategory('Daemon');
  expect(onCategoryChange).toHaveBeenCalledWith('server');
  rendered.unmount();

  render(SettingsWorkspace, { client, category: 'retired_category' });
  expect(await screen.findByRole('heading', { level: 2, name: 'Appearance' })).toBeDefined();
});
```

`AppShell.test.ts` (follow the raw-snippet test at :1053):

```ts
it('passes the URL Settings category to Settings and commits a chosen one', async () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'settings', settingsCategory: 'search'
  }))}`);
  const categories: string[] = [];
  let choose: ((categoryID: string) => void) | undefined;
  const settings = createRawSnippet<[unknown, (key: number) => void, unknown, string, (categoryID: string) => void]>(
    (_request, _consumed, _target, getCategory, getChoose) => ({
      render: () => '<main aria-label="Settings category fixture"></main>',
      setup: () => { categories.push(getCategory()); choose = getChoose(); }
    })
  );
  const state = new ExploreState(window);
  const rendered = render(AppShell, { client: createAPIClient(vi.fn()), state, enabled: false, settings: settings as never });
  try {
    expect(await screen.findByRole('main', { name: 'Settings category fixture' })).toBeDefined();
    expect(categories.at(-1)).toBe('search');
    choose!('server');
    expect(state.current).toMatchObject({ workspace: 'settings', settingsCategory: 'server', settingsAuthority: '' });
  } finally {
    rendered.unmount();
    state.destroy();
  }
});
```

Also: from Operations, opening CardDAV settings leaves `state.current.settingsCategory`
`'carddav'` (extend the existing CardDAV-from-Operations test in
`AppShell.test.ts` if one exists; otherwise add one that clicks "Open CardDAV
settings" in the status list fixture).

- [ ] **Step 2: Run** `bunx vitest run src/lib/explore/state.test.ts src/lib/components/settings src/lib/components/shell/AppShell.test.ts src/App.test.ts`.
  Expected: new tests FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): keep the Settings category in the URL`.

---

### Task 4: Operations status list, Set up targets, and document panels

**Files:**
- Create: `web/src/lib/components/operations/OperationStatusList.svelte`
- Create: `web/src/lib/components/operations/OperationHostSetup.svelte`
- Delete: `web/src/lib/components/operations/OperationLaneCards.svelte`
- Modify: `web/src/lib/operations/labels.ts` (Set up map)
- Modify: `web/src/lib/components/operations/OperationsWorkspace.svelte`
  (props :34-50, lane cards :312-319, related status :272-281)
- Modify: `web/src/lib/components/operations/OperationRelatedStatus.svelte`
  (props :184-196, `settingsLabels` :216-220, configured-false branch
  :270-274, document buttons :294-296 and :312-314)
- Modify: `web/src/lib/carddav/navigation.ts` (remove `document_index`,
  `document_vector`)
- Modify: `web/src/lib/components/shell/AppShell.svelte`
  (`openOperationConfiguration` :330-337, Operations mount :1315-1330)
- Test: `OperationsWorkspace.test.ts`, `OperationRelatedStatus.test.ts`,
  `web/src/lib/explore/state.test.ts:223-265`,
  `SettingsWorkspace.test.ts:225-249`, `AppShell.test.ts:1027-1050`

**Interfaces:**
- Consumes: `OPERATION_*` maps, `RELATED_STATUS_LABELS`, `operationStateChip`,
  `formatDateTime` (Task 1); `SettingsNavigationAuthority` with
  `semantic_search` and `person_embeddings`, and `settingsCategory` (Task 3).
- Produces, in `operations/labels.ts`:

```ts
import type { SettingsNavigationAuthority } from '../carddav/navigation';

export interface OperationSettingsTarget {
  settingsCategory: string;
  settingsAuthority: SettingsNavigationAuthority | '';
}
export interface OperationHostSetup {
  kind: 'host';
  text: string;
  guideLabel: string;
  guideHref: string;
}
export type OperationSetup = { kind: 'settings'; target: OperationSettingsTarget } | OperationHostSetup;

export const DOCUMENT_INDEX_SETUP: OperationHostSetup = {
  kind: 'host',
  text: 'Configured in config.toml on the daemon host.',
  guideLabel: 'Document indexing setup',
  guideHref: 'https://msgvault.io/docs/usage/document-indexing/#configure-the-policy'
};
export const DOCUMENT_SEARCH_SETUP: OperationHostSetup = {
  kind: 'host',
  text: 'Configured in config.toml on the daemon host. Also needs semantic search.',
  guideLabel: 'Document search setup',
  guideHref: 'https://msgvault.io/docs/usage/document-indexing/#semantic-and-hybrid-document-search'
};

// Where an Off kind is turned on (spec "Set up targets"). source_sync and
// carddav_sync have no entry: their related-status buttons already lead there.
export const OPERATION_SETUP: Readonly<Partial<Record<OperationKind, OperationSetup>>> = {
  message_embedding: { kind: 'settings', target: { settingsCategory: 'search', settingsAuthority: 'semantic_search' } },
  person_embedding: { kind: 'settings', target: { settingsCategory: 'search', settingsAuthority: 'person_embeddings' } },
  visual_embedding: { kind: 'settings', target: { settingsCategory: 'search', settingsAuthority: 'visual_attachments' } },
  person_enrichment: { kind: 'settings', target: { settingsCategory: 'enrichment', settingsAuthority: '' } },
  person_sweep: { kind: 'settings', target: { settingsCategory: 'people', settingsAuthority: '' } },
  document_extraction: DOCUMENT_INDEX_SETUP,
  document_embedding: DOCUMENT_SEARCH_SETUP
};
```

- Produces: `OperationsWorkspace` prop `onSetUp?: (target: OperationSettingsTarget) => void`;
  `OperationsWorkspace` and `OperationRelatedStatus` prop `onConfigure` becomes
  `() => void` (it now only opens visual attachment settings).

`OperationHostSetup.svelte`:

```svelte
<script lang="ts">
  import type { OperationHostSetup } from '../../operations/labels';
  let { setup }: { setup: OperationHostSetup } = $props();
</script>

<p class="host-setup">
  {setup.text}
  <a href={setup.guideHref} target="_blank" rel="noreferrer">{setup.guideLabel}</a>
</p>

<style>
  .host-setup { margin: 0; color: var(--text-muted); font-size: var(--font-size-xs); }
</style>
```

`OperationStatusList.svelte` (props `lanes`, `actionPending`, `onNavigate`,
`onAction` as `OperationLaneCards` had, plus `onSetUp`):

```svelte
<script lang="ts">
  import { Button, Chip, type ChipTone } from '@kenn-io/kit-ui';
  import {
    OPERATION_ACTION_LABELS, OPERATION_KIND_LABELS, OPERATION_LANE_LABELS, OPERATION_SETUP,
    RELATED_STATUS_LABELS, operationStateChip, type OperationSettingsTarget, type RelatedStatus
  } from '../../operations/labels';
  import type { OperationAction, OperationLaneStatus, OperationStatusLane } from '../../operations/models';
  import { formatDateTime } from '../../util/format';
  import OperationHostSetup from './OperationHostSetup.svelte';

  let {
    lanes,
    actionPending = null,
    onNavigate = () => undefined,
    onAction = () => undefined,
    onSetUp = () => undefined
  }: {
    lanes: readonly OperationStatusLane[];
    actionPending?: OperationAction | null;
    onNavigate?: (target: RelatedStatus, button: HTMLButtonElement) => void;
    onAction?: (action: OperationAction) => void;
    onSetUp?: (target: OperationSettingsTarget) => void;
  } = $props();

  function statusChip(kind: OperationLaneStatus): { label: string; tone: ChipTone } {
    if (!kind.configured) return { label: 'Off', tone: 'muted' };
    const run = kind.active ?? kind.latest;
    return run ? operationStateChip(run.state) : { label: 'No runs yet', tone: 'muted' };
  }
</script>

<section class="status-list" aria-label="Operation lanes">
  {#each lanes as lane (lane.lane)}
    {@const headingID = `operation-lane-${lane.lane}`}
    <div class="lane">
      <h2 id={headingID}>{OPERATION_LANE_LABELS[lane.lane]}</h2>
      {#if lane.kinds.length === 0}
        <Chip size="sm" tone="muted" uppercase={false}>Status unavailable</Chip>
      {:else}
        <ul aria-labelledby={headingID}>
          {#each lane.kinds as kind (kind.kind)}
            {@const nameID = `operation-kind-${kind.kind}`}
            {@const chip = statusChip(kind)}
            {@const run = kind.active ?? kind.latest}
            {@const setup = kind.configured ? undefined : OPERATION_SETUP[kind.kind]}
            <li aria-labelledby={nameID}>
              <span class="name" id={nameID}>{OPERATION_KIND_LABELS[kind.kind]}</span>
              <span class="status">
                <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
                {#if kind.history_availability !== 'available'}<span class="history-note">History unavailable</span>{/if}
              </span>
              <span class="time">
                {#if run}<time datetime={run.started_at}>{formatDateTime(run.started_at)}</time>{/if}
                {#if kind.latest_successful && run?.state !== 'succeeded'}
                  <span>Last succeeded <time datetime={kind.latest_successful.started_at}>{formatDateTime(kind.latest_successful.started_at)}</time></span>
                {/if}
              </span>
              <span class="actions">
                {#if kind.related_status}
                  <Button size="sm" surface="soft" label={`Open ${RELATED_STATUS_LABELS[kind.related_status]}`}
                    onclick={(event) => onNavigate(kind.related_status!, event.currentTarget as HTMLButtonElement)} />
                {/if}
                {#each kind.supported_actions as action (action)}
                  <Button size="sm" tone="info"
                    label={actionPending === action ? `${OPERATION_ACTION_LABELS[action]}…` : OPERATION_ACTION_LABELS[action]}
                    disabled={actionPending !== null} onclick={() => onAction(action)} />
                {/each}
                {#if setup?.kind === 'settings'}
                  <Button size="sm" tone="info" surface="soft" label="Set up"
                    ariaLabel={`Set up ${OPERATION_KIND_LABELS[kind.kind]}`} onclick={() => onSetUp(setup.target)} />
                {:else if setup?.kind === 'host'}
                  <OperationHostSetup {setup} />
                {/if}
              </span>
            </li>
          {/each}
        </ul>
      {/if}
    </div>
  {/each}
</section>
```

Styles: lanes stack vertically (`display: grid; gap: var(--space-4)`); each
`li` is a four-column grid `minmax(10rem, 1fr) auto minmax(10rem, 1fr) auto`
with a `border-top: 1px solid var(--border-muted)`; under
`@media (max-width: 760px)` it becomes one column. `.history-note` uses
`color: var(--status-warning-ink)`. Do not give one rule background, border,
and radius together (`hand-rolled-card`). "Last succeeded" appears when the
row's shown run did not succeed and a successful run exists (state comparison;
run IDs are not stable to compare).

`OperationsWorkspace.svelte`: import `OperationStatusList` instead of
`OperationLaneCards`; pass `onSetUp`; change `onConfigure` to `() => void`
and pass it through to `OperationRelatedStatus`.

`OperationRelatedStatus.svelte`:

- Delete `settingsLabels`. Props: `onConfigure?: () => void`.
- Configured-false branch:

```svelte
{:else if configured === false}
  <div class="summary" aria-label={`${labels[authority]} configuration`}>
    <p><Chip size="sm" tone="muted" uppercase={false}>Off</Chip> {configurationLabels[authority]}</p>
  </div>
  {#if authority === 'getVisualAttachmentStatus'}
    <Button label="Open visual attachment settings" onclick={() => onConfigure()} />
  {:else}
    <OperationHostSetup setup={authority === 'getDocumentIndexStatus' ? DOCUMENT_INDEX_SETUP : DOCUMENT_SEARCH_SETUP} />
  {/if}
```

- Replace the "Open document index settings" button (:294-296) with
  `<OperationHostSetup setup={DOCUMENT_INDEX_SETUP} />` under the same
  condition, and "Open document vector settings" (:312-314) with
  `<OperationHostSetup setup={DOCUMENT_SEARCH_SETUP} />`.

`navigation.ts`: remove `document_index` and `document_vector`.

AppShell:

```ts
function openVisualAttachmentSettings(): void {
  commitNavigation({ workspace: 'settings', settingsAuthority: 'visual_attachments' });
}
function setUpOperation(target: OperationSettingsTarget): void {
  commitNavigation({ workspace: 'settings', ...target });
}
```

Pass `onConfigure={openVisualAttachmentSettings}` and
`onSetUp={setUpOperation}`; delete `openOperationConfiguration` and the
now-unused `SettingsNavigationAuthority` import.

- [ ] **Step 1: Write the failing tests**

`OperationsWorkspace.test.ts` (replace the lane-card assertions at :131-176;
keep the file's `snapshot()`, `controller()`, and `urlState()` helpers):

```ts
import { OPERATION_KIND_LABELS } from '../../operations/labels';
import type { OperationKind, OperationLane } from '../../operations/models';

const off = (kind: OperationKind, lane: OperationLane, related?: string) => ({
  lane, kind, configured: false, history_availability: 'available' as const, supported_actions: [],
  ...(related ? { related_status: related } : {})
});

function renderKinds(kinds: Array<ReturnType<typeof off>>, props: Record<string, unknown> = {}) {
  const lanes = ['messages', 'person_facts', 'contacts', 'documents', 'visual_attachments'] as const;
  const statusLanes = lanes.map((lane) => ({ lane, kinds: kinds.filter((kind) => kind.lane === lane) }));
  return render(OperationsWorkspace, {
    controller: controller(snapshot({ statusLanes: statusLanes as never })) as never,
    state: urlState(),
    ...props
  });
}

it('lists every lane with one row per kind and no History available text', () => {
  render(OperationsWorkspace, { controller: controller() as never, state: urlState() });
  const region = screen.getByRole('region', { name: 'Operation lanes' });
  expect(within(region).getAllByRole('heading', { level: 2 }).map((heading) => heading.textContent))
    .toEqual(['Messages', 'Facts', 'Contacts', 'Documents', 'Attachments']);
  expect(within(screen.getByRole('list', { name: 'Messages' })).getAllByRole('listitem')).toHaveLength(2);
  const embedding = screen.getByRole('listitem', { name: 'Message embedding' });
  expect(within(embedding).getByText('Running')).toBeDefined();
  expect(within(embedding).getByText('History unavailable')).toBeDefined();
  expect(region.textContent).not.toContain('History available');
  expect(within(screen.getByRole('listitem', { name: 'Person fact sweep' })).getByText('Off')).toBeDefined();
  expect(within(screen.getByRole('listitem', { name: 'Person embedding' })).getByText('No runs yet')).toBeDefined();
});

it('shows Status unavailable for a lane without kinds', () => {
  renderKinds([]);
  expect(screen.getAllByText('Status unavailable')).toHaveLength(5);
});

it.each([
  ['message_embedding', 'messages', { settingsCategory: 'search', settingsAuthority: 'semantic_search' }],
  ['person_embedding', 'person_facts', { settingsCategory: 'search', settingsAuthority: 'person_embeddings' }],
  ['visual_embedding', 'visual_attachments', { settingsCategory: 'search', settingsAuthority: 'visual_attachments' }],
  ['person_enrichment', 'person_facts', { settingsCategory: 'enrichment', settingsAuthority: '' }],
  ['person_sweep', 'person_facts', { settingsCategory: 'people', settingsAuthority: '' }]
] as const)('sends an Off %s row to Settings', async (kind, lane, target) => {
  const onSetUp = vi.fn();
  renderKinds([off(kind, lane)], { onSetUp });
  const row = screen.getByRole('listitem', { name: OPERATION_KIND_LABELS[kind] });
  expect(within(row).getByText('Off')).toBeDefined();
  await fireEvent.click(within(row).getByRole('button', { name: `Set up ${OPERATION_KIND_LABELS[kind]}` }));
  expect(onSetUp).toHaveBeenCalledWith(target);
});

it.each([
  ['document_extraction', 'Configured in config.toml on the daemon host.', 'Document indexing setup',
    'https://msgvault.io/docs/usage/document-indexing/#configure-the-policy'],
  ['document_embedding', 'Configured in config.toml on the daemon host. Also needs semantic search.', 'Document search setup',
    'https://msgvault.io/docs/usage/document-indexing/#semantic-and-hybrid-document-search']
] as const)('explains host configuration for an Off %s row', (kind, text, linkName, href) => {
  renderKinds([off(kind, 'documents')]);
  const row = screen.getByRole('listitem', { name: OPERATION_KIND_LABELS[kind] });
  expect(within(row).queryByRole('button', { name: /^Set up/ })).toBeNull();
  expect(within(row).getByText(text, { exact: false })).toBeDefined();
  const link = within(row).getByRole('link', { name: linkName });
  expect(link.getAttribute('href')).toBe(href);
  expect(link.getAttribute('target')).toBe('_blank');
});

it('keeps the related-status button and no Set up for Off CardDAV and source rows', () => {
  renderKinds([off('carddav_sync', 'contacts', 'getCardDAVStatus'), off('source_sync', 'messages', 'listSourceStatus')]);
  expect(screen.getByRole('button', { name: 'Open CardDAV settings' })).toBeDefined();
  expect(screen.getByRole('button', { name: 'Open Sources status' })).toBeDefined();
  expect(screen.queryByRole('button', { name: /^Set up/ })).toBeNull();
});

it('shows when the last success differs from the latest run', () => {
  const partial = { id: 'x', kind: 'source_sync' as const, lane: 'messages' as const, state: 'partial' as const,
    started_at: '2026-08-30T12:00:00Z', counters: [] };
  const success = { ...partial, id: 'y', state: 'succeeded' as const, started_at: '2026-08-29T12:00:00Z' };
  renderKinds([{ ...off('source_sync', 'messages'), configured: true, latest: partial, latest_successful: success } as never]);
  const row = screen.getByRole('listitem', { name: 'Source sync' });
  expect(within(row).getByText('Partial')).toBeDefined();
  expect(within(row).getByText(/Last succeeded/)).toBeDefined();
});
```

Replace "passes the authoritative unconfigured document state" (:196-215):
with `document_extraction` unconfigured and `operationStatus:
'getDocumentIndexStatus'`, the panel shows "Off", the host line, and the
"Document indexing setup" link, has no "Open document index settings"
button, and sends no request.

`OperationRelatedStatus.test.ts`: a loaded document index status with
`profile_enabled: false` shows the "Document indexing setup" link and no
settings button; a document vector status with `enabled: false` shows
"Document search setup"; a `configured: false` visual panel's "Open visual
attachment settings" calls `onConfigure` with no arguments.

`state.test.ts`: replace `document_index`/`document_vector` in :223, :244-245,
:259, :264 with `semantic_search`/`person_embeddings`, and add:

```ts
it('opens Settings on Appearance for a retired document authority link', () => {
  const restored = parseExploreURLState(`?workspace=settings&explore=${encodeURIComponent(JSON.stringify({
    schemaVersion: 2, settingsAuthority: 'document_index'
  }))}`);
  expect(restored.settingsAuthority).toBe('');
  expect(restored.settingsCategory).toBe('browser');
});
```

`SettingsWorkspace.test.ts:225-227`: replace the two document cases with
`semantic_search` (`vector.enabled`) and `person_embeddings`
(`vector.people.enabled`, add that setting to the fixture).
`AppShell.test.ts:1027-1050`: rewrite as "opens visual attachment Settings
without a live status request" using `visual_embedding` unconfigured, "Open
Visual attachment status", then "Open visual attachment settings", expecting
`settingsAuthority: 'visual_attachments'` and `settingsCategory: 'search'`.
Add: clicking "Set up Person fact sweep" leaves `state.current` at
`{ workspace: 'settings', settingsCategory: 'people', settingsAuthority: '' }`.

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/operations src/lib/explore/state.test.ts src/lib/components/settings src/lib/components/shell/AppShell.test.ts`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): show Operations as a status list with Set up targets`.

---

### Task 5: Operations refresh, run history reload, runs table, and detail

**Files:**
- Modify: `web/src/lib/operations/controller.svelte.ts` (state :29-49,
  `snapshot` :77-100, `loadPageOne` :286-338, `invalidateContext` :385-399,
  `destroy` :276-284; new `refreshStatus`)
- Modify: `web/src/lib/operations/models.ts:41-62` (`OperationsSnapshot`)
- Modify: `web/src/lib/components/operations/OperationsWorkspace.svelte`
  (`Controller` type :32, header :306-310, filters :321-332)
- Modify: `web/src/lib/components/operations/OperationRunTable.svelte`
- Modify: `web/src/lib/components/operations/OperationRunDetail.svelte`
- Create: `web/src/lib/components/operations/OperationsRefresh.test.ts`
- Test: `web/src/lib/operations/controller.svelte.test.ts`,
  `OperationsWorkspace.test.ts`

**Interfaces:**
- Consumes: `operationStateChip`, `triggerLabel`, `counterSummary`,
  `counterLabel`, `counterValue`, `operationDuration` (Task 1).
- Produces:
  - `OperationsController.refreshStatus(): Promise<boolean>` — loads
    `GET /api/v1/operations/status` only; never touches rows, cursor, paging,
    detail, or conflict state.
  - `OperationsSnapshot.statusUpdatedAt: number | null` (epoch ms of the last
    successful status load, from page one or a status refresh) and
    `OperationsSnapshot.statusRefreshing: boolean`.
  - `refresh()` keeps its meaning: status plus page one of runs, replacing the
    loaded rows. "Reload run history" calls it.

Controller additions:

```ts
private statusUpdatedAt = $state<number>();
private statusRefreshing = $state(false);
private statusGeneration = 0;
private statusAbort?: AbortController;

async refreshStatus(): Promise<boolean> {
  if (this.disposed || !this.loaded || this.initialLoading || this.backgroundLoading || this.statusRefreshing) {
    return false;
  }
  const request = new AbortController();
  this.statusAbort = request;
  const context = this.contextGeneration;
  const generation = ++this.statusGeneration;
  this.statusRefreshing = true;
  try {
    const result = await generatedGetOperationStatus({ ...this.client, signal: request.signal });
    if (!this.ownsStatus(request, generation, context)) return false;
    if (!result.data) {
      this.statusError = 'Unable to load operation status.';
      return false;
    }
    this.statusKinds = result.data.lanes;
    this.statusReadable = true;
    this.statusError = null;
    this.statusUpdatedAt = Date.now();
    return true;
  } catch {
    if (this.ownsStatus(request, generation, context)) this.statusError = 'Unable to load operation status.';
    return false;
  } finally {
    if (this.ownsStatus(request, generation, context)) {
      this.statusAbort = undefined;
      this.statusRefreshing = false;
    }
  }
}

private cancelStatus(): void {
  this.statusAbort?.abort();
  this.statusAbort = undefined;
  this.statusGeneration += 1;
  this.statusRefreshing = false;
}

private ownsStatus(owner: AbortController, generation: number, context: number): boolean {
  return !this.disposed && !owner.signal.aborted && this.statusAbort === owner &&
    this.statusGeneration === generation && this.contextGeneration === context;
}
```

Call `this.cancelStatus()` at the start of `loadPageOne` (page one's status
answer wins), in `invalidateContext`, and in `destroy`. In `loadPageOne`, set
`this.statusUpdatedAt = Date.now()` beside `statusReadable = true`. Add both
fields to `snapshot` (`statusUpdatedAt: this.statusUpdatedAt ?? null`).

`OperationsWorkspace.svelte`:

- `type Controller = Pick<OperationsController, 'snapshot' | 'refresh' | 'refreshStatus' | 'loadMore' | 'restart' | 'runAction'>;`
- Header actions:

```svelte
<RefreshControl
  label="Refresh operation status"
  lastUpdatedAt={current.statusUpdatedAt}
  busy={current.statusRefreshing}
  onRefresh={() => void refreshStatus()}
/>
```

```ts
async function refreshStatus(): Promise<void> {
  const control = root?.querySelector<HTMLElement>('.kit-refresh-control');
  const hadFocus = Boolean(control?.contains(document.activeElement));
  await controller.refreshStatus();
  // Kit disables the button while busy, and a disabled button drops focus.
  if (!hadFocus) return;
  await tick();
  control?.querySelector<HTMLButtonElement>('button')?.focus();
}
```

- End of the filter row (after "Clear operation dates"):
  `<Button size="sm" surface="soft" label="Reload run history" disabled={current.backgroundLoading} onclick={() => void controller.refresh()} />`.
  The "Refreshing operations in the background…" notice stays for history
  reloads.

`OperationRunTable.svelte` (desktop and narrow):

- Trigger: `{triggerLabel(run.trigger)}`.
- State cell:

```svelte
{@const chip = operationStateChip(run.state)}
<div class="state">
  <Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>
  {#if run.error && (run.state === 'failed' || run.state === 'partial')}<span class="error">{run.error.message}</span>{/if}
</div>
```

- Counters: `{counterSummary(run.counters)}`. Narrow list uses the same three
  helpers. Delete `titleCase`, `dotStatus`, `counters`, and the `StatusDot`
  import.

`OperationRunDetail.svelte`:

- State: the same `Chip`; Trigger: `triggerLabel(detail.trigger)`.
- Counters: `<dt>{counterLabel(counter.name)}</dt><dd>{counterValue(counter)}</dd>`;
  delete `text-transform: capitalize` from `dt`. Every counter stays,
  including zeros.
- Error:

```svelte
{#if detail.error}
  <section class="error" role="alert" aria-label="Operation error">
    <p>{detail.error.message}</p>
    <code>Code: {detail.error.code}</code>
  </section>
{/if}
```

- Delete `titleCase`, `dotStatus`, and the `StatusDot` import.

- [ ] **Step 1: Write the failing tests**

`OperationsRefresh.test.ts` (real controller, fake daemon):

```ts
import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { createAPIClient } from '../../api/client';
import { OperationsController } from '../../operations/controller.svelte';
import type { OperationRunSummary, OperationsURLState } from '../../operations/models';
import OperationsWorkspace from './OperationsWorkspace.svelte';

// The daemon encrypts run IDs with a fresh nonce per response
// (internal/api/operation_tokens.go), so the same run never carries the same
// ID twice. These fixtures do the same; a test cannot pass by matching IDs.
let encodings = 0;
const encode = (run: number) => `op2.${String(++encodings).padStart(32, '0')}.run${run}`;

function summary(run: number): OperationRunSummary {
  return {
    id: encode(run), kind: 'source_sync', lane: 'messages', trigger: 'manual', state: 'succeeded',
    started_at: `2026-08-30T1${run}:00:00Z`, finished_at: `2026-08-30T1${run}:01:00Z`,
    counters: [{ name: 'processed', unit: 'messages', value: run }]
  };
}

const state = (overrides: Partial<OperationsURLState> = {}): OperationsURLState => ({
  operationLane: '', operationKind: '', operationState: '', operationStartedFrom: '',
  operationStartedBefore: '', operationRunID: null, operationStatus: '', ...overrides
});

function daemon(pageTwo: () => Response = () => Response.json({ runs: [summary(3), summary(4)], membership_revision: 7, unavailable_kinds: [] })) {
  const requests: string[] = [];
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    requests.push(`${url.pathname}${url.search}`);
    if (url.pathname === '/api/v1/operations/status') {
      return Response.json({ lanes: [{ lane: 'messages', kind: 'source_sync', configured: true,
        history_availability: 'available', supported_actions: [], latest: summary(1) }] });
    }
    if (url.pathname === '/api/v1/operations/runs') {
      if (url.searchParams.get('cursor') === 'page-two') return pageTwo();
      return Response.json({ runs: [summary(1), summary(2)], membership_revision: 7, unavailable_kinds: [], next_cursor: 'page-two' });
    }
    return Response.json({ ...summary(3), related_status: 'listSourceStatus', supported_actions: [] });
  });
  return { requests, client: createAPIClient(fetchFn) };
}

const runButtons = () => screen.getAllByRole('button', { name: 'Open Source sync run' });

afterEach(() => vi.useRealTimers());

describe('Operations refresh', () => {
  it('refreshes only status on its timer and on click, keeping paged rows, detail, and focus', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const { requests, client } = daemon();
    const controller = new OperationsController(client);
    try {
      await controller.applyURLState(state());
      await controller.loadMore();
      const selected = controller.snapshot.rows[2]!.id;
      await controller.applyURLState(state({ operationRunID: selected }));
      render(OperationsWorkspace, { controller, state: state({ operationRunID: selected }) });
      expect(runButtons()).toHaveLength(4);
      expect(screen.getByRole('region', { name: 'Operation run detail' })).toBeDefined();
      runButtons()[2]!.focus();
      const focused = document.activeElement;

      requests.length = 0;
      await vi.advanceTimersByTimeAsync(5 * 60 * 1000);
      await waitFor(() => expect(requests).toEqual(['/api/v1/operations/status']));
      expect(runButtons()).toHaveLength(4);
      expect(screen.getByRole('region', { name: 'Operation run detail' })).toBeDefined();
      expect(document.activeElement).toBe(focused);

      requests.length = 0;
      await fireEvent.click(screen.getByRole('button', { name: 'Refresh operation status' }));
      await waitFor(() => expect(requests).toEqual(['/api/v1/operations/status']));
      expect(runButtons()).toHaveLength(4);
      expect(screen.getByRole('region', { name: 'Operation run detail' })).toBeDefined();
    } finally {
      controller.destroy();
    }
  });

  it('reloads status and page one of runs from Reload run history', async () => {
    const { requests, client } = daemon();
    const controller = new OperationsController(client);
    try {
      await controller.applyURLState(state());
      await controller.loadMore();
      const before = controller.snapshot.rows.map((row) => row.id);
      render(OperationsWorkspace, { controller, state: state() });
      requests.length = 0;
      await fireEvent.click(screen.getByRole('button', { name: 'Reload run history' }));
      await waitFor(() => expect(runButtons()).toHaveLength(2));
      expect(requests.sort()).toEqual(['/api/v1/operations/runs?limit=25', '/api/v1/operations/status']);
      expect(controller.snapshot.rows.some((row) => before.includes(row.id))).toBe(false);
    } finally {
      controller.destroy();
    }
  });

  it('keeps the conflict notice when Load more meets a changed history', async () => {
    const { client } = daemon(() => Response.json(
      { error: 'operation_history_conflict', message: 'Operation history changed.' }, { status: 409 }));
    const controller = new OperationsController(client);
    try {
      await controller.applyURLState(state());
      render(OperationsWorkspace, { controller, state: state() });
      await fireEvent.click(screen.getByRole('button', { name: 'Load more operation history' }));
      const conflict = await screen.findByRole('alert', { name: 'Operation history conflict' });
      expect(conflict.textContent).toContain('Operation history changed. Restart from the first page.');
      expect(screen.getByRole('button', { name: 'Restart operation history' })).toBeDefined();
    } finally {
      controller.destroy();
    }
  });
});
```

`controller.svelte.test.ts`: `refreshStatus` after a loaded page two sends
one `/status` request, leaves `rows`, `nextCursor`, and `detail` unchanged,
updates `statusLanes`, and sets `statusUpdatedAt`; a failed status refresh
sets `statusError` and keeps the previous lanes.

`OperationsWorkspace.test.ts`: add `refreshStatus: vi.fn(async () => true)`
to `controller()` and `statusUpdatedAt: null, statusRefreshing: false` to
`snapshot()`. Add:

```ts
it('shows triggers, counters, and failure sentences in the runs table', () => {
  const failed = run({ id: RUN_TWO, state: 'failed', trigger: undefined,
    counters: [
      { name: 'processed', unit: 'messages', value: 20 }, { name: 'added', unit: 'messages', value: 20 },
      { name: 'updated', unit: 'messages', value: 0 }, { name: 'item_errors', unit: 'messages', value: 0 }
    ],
    error: { code: 'source_sync_failed', message: 'Source sync failed.' } });
  render(OperationsWorkspace, { controller: controller(snapshot({ rows: [failed], unavailableKinds: [] })) as never, state: urlState() });
  const row = within(screen.getByRole('table', { name: 'Operation history' })).getAllByRole('row')[1]!;
  expect(row.textContent).toContain('—');
  expect(row.textContent).toContain('20 messages processed · 20 added');
  expect(row.textContent).not.toContain('item errors');
  expect(row.textContent).toContain('Failed');
  expect(row.textContent).toContain('Source sync failed.');
});

it('leads the run detail error with the server sentence and keeps every counter', () => {
  const detail = { ...run({ state: 'failed', counters: [{ name: 'item_errors', unit: 'messages', value: 0 }],
    error: { code: 'source_sync_failed', message: 'Source sync failed.' } }), supported_actions: [] };
  render(OperationsWorkspace, { controller: controller(snapshot({ detail })) as never, state: urlState({ operationRunID: RUN_ONE }) });
  const error = screen.getByRole('alert', { name: 'Operation error' });
  expect(error.firstElementChild?.textContent).toBe('Source sync failed.');
  expect(within(error).getByText('Code: source_sync_failed').tagName).toBe('CODE');
  expect(screen.getByText('Item errors')).toBeDefined();
  expect(screen.getByText('0 messages')).toBeDefined();
});
```

Update the existing expectation "2 failed writes" (:254) to "2 writes failed".

- [ ] **Step 2: Run** `bunx vitest run src/lib/operations src/lib/components/operations`.
  Expected: new tests FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): refresh Operations status on its own and readable run history`.
  In the body, state why history keeps a manual reload: run IDs are
  re-encrypted per response and the cursor belongs to one snapshot (spec
  decision 1).

---

### Task 6: Deletions

**Files:**
- Modify: `web/src/lib/components/shell/PageHeader.svelte` (add
  `descriptionContent?: Snippet`)
- Modify: `web/src/lib/components/deletions/DeletionsWorkspace.svelte`
  (props :25-35, markup :269-371, styles :429-499)
- Test: `web/src/lib/components/deletions/DeletionsWorkspace.test.ts`

**Interfaces:**
- Consumes: `formatBytes`, `formatDateTime`, `formatRelativeTime` (Task 1);
  `preflightReasonLabel`, `sentenceCase` (`explore/labels.ts`).
- Produces: `PageHeader` prop `descriptionContent?: Snippet`, rendered inside
  the description `<p>` instead of `description` when given.
  `DeletionsWorkspace` prop `now?: () => Date` (default `() => new Date()`).

`PageHeader.svelte`:

```svelte
{#if descriptionContent}<p>{@render descriptionContent()}</p>{:else if description}<p>{description}</p>{/if}
```

`DeletionsWorkspace.svelte`:

- Module script, status chips:

```ts
const MANIFEST_STATUS: Readonly<Record<string, { label: string; tone: ChipTone }>> = {
  pending: { label: 'Pending', tone: 'info' },
  in_progress: { label: 'In progress', tone: 'info' },
  completed: { label: 'Completed', tone: 'success' },
  failed: { label: 'Failed', tone: 'danger' },
  cancelled: { label: 'Cancelled', tone: 'muted' }
};
function manifestStatusChip(status: string): { label: string; tone: ChipTone } {
  return Object.hasOwn(MANIFEST_STATUS, status) ? MANIFEST_STATUS[status]! : { label: sentenceCase(status), tone: 'neutral' };
}
```

- Header:

```svelte
<PageHeader title="Deletions">
  {#snippet descriptionContent()}Deletions you've staged. Nothing is deleted until you run <code>msgvault delete-staged</code>.{/snippet}
</PageHeader>
```

- Staging (replaces :276-327; delete the `KbdBadge` import and hints; `d`
  and `D` shortcuts stay):

```svelte
{#if !selection}
  <EmptyState title="Nothing selected for deletion" description="Select items in Everything, then choose Review for deletion…" />
{:else}
  <Card padding="sm">
    <section class="staging" aria-labelledby="deletion-review-title">
      <div class="staging-header">
        <h2 id="deletion-review-title">Review selection</h2>
        <Button tone="info" surface="soft" label="Review selection" disabled={pending} onclick={() => void reviewSelection()} />
      </div>
      {#if reviewed}
        {@const stageReason = unavailableReason('stage_deletion')}
        <div class="review" role="status">
          <strong>{reviewed.count.toLocaleString()} {reviewed.count === 1 ? 'item' : 'items'} · {formatBytes(reviewed.estimated_bytes)}</strong>
          <span>{reviewed.deletable_count.toLocaleString()} can be staged · {(reviewed.count - reviewed.deletable_count).toLocaleString()} will be skipped.{selectionExclusions()}</span>
          <span>Review expires <time datetime={reviewed.expires_at} title={formatDateTime(reviewed.expires_at, 'long')}>{formatRelativeTime(reviewed.expires_at, now())}</time></span>
          {#if reviewed.search_deletion_scope === 'active'}<span>Semantic search covers active messages only.</span>{/if}
          {#if stageReason}<span class="reason" title={stageReason}>{preflightReasonLabel('stage_deletion', stageReason)}</span>{/if}
        </div>
        <div class="actions">
          <Button surface="outline" label="Dry run" disabled={pending} onclick={() => void dryRun()} />
          <Button tone="info" surface="solid" label="Stage deletion…" disabled={pending || Boolean(stageReason)}
            onclick={() => { if (reviewedIsCurrent()) confirmStage = selection?.mode; }} />
        </div>
      {/if}
      {#if preview}
        <p class="result" role="status">{resultSummary(preview)}</p>
        {#if stageCounts(preview).skipped > 0}<p class="warning" role="alert">{partialWarning(preview)}</p>{/if}
      {/if}
    </section>
  </Card>
{/if}
```

  Arriving from Everything keeps `reviewOnMount` and the immediate
  confirmation (spec decision 5); the summary renders behind the modal.
- Manifests (replaces :329-370):

```svelte
{#if loading}<p role="status">Loading deletion manifests…</p>
{:else if manifests.length === 0}
  <EmptyState title="No staged deletions" description="Deletions you stage appear here, along with their execution status." />
{:else}
  <div class="manifests" class:has-detail={Boolean(detail)}>
    <Table ariaLabel="Deletion manifests" zebra={false} class="manifest-table">
      {#snippet header()}
        <TableHeaderCell label="ID" /><TableHeaderCell label="Description" /><TableHeaderCell label="Items" />
        <TableHeaderCell label="Status" /><TableHeaderCell label="Created" /><TableHeaderCell label="Actions" />
      {/snippet}
      {#each manifests as manifest (manifest.id)}
        {@const chip = manifestStatusChip(manifest.status)}
        <tr>
          <td><code>{manifest.id}</code></td>
          <td>{manifest.description}</td>
          <td>{manifest.message_count.toLocaleString()} {manifest.message_count === 1 ? 'item' : 'items'}</td>
          <td><Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip></td>
          <td><time datetime={manifest.created_at} title={manifest.created_at}>{formatDateTime(manifest.created_at)}</time></td>
          <td class="row-actions">
            <Button size="sm" surface="outline" label={`Inspect ${manifest.id}`} onclick={() => void inspect(manifest)} />
            {#if manifest.status === 'pending' || manifest.status === 'in_progress'}
              <Button size="sm" surface="outline" label={`Cancel ${manifest.id}`} onclick={() => { confirmCancel = manifest; }} />
            {/if}
          </td>
        </tr>
      {/each}
    </Table>
    {#if detail}
      {@const detailChip = manifestStatusChip(detail.status)}
      <Card padding="sm" ariaLabel={`Deletion manifest ${detail.id}`}>
        <aside class="detail">
          <div class="detail-header">
            <h2><code>{detail.id}</code></h2>
            <Button size="sm" surface="soft" label="Close manifest detail" onclick={() => { detail = undefined; }} />
          </div>
          <Chip size="sm" tone={detailChip.tone} uppercase={false}>{detailChip.label}</Chip>
          <span>{detail.account || 'Account unavailable'}</span>
          <span>{detail.message_count.toLocaleString()} items · {detail.description}</span>
          {#if detail.execution}
            <span>{detail.execution.succeeded} succeeded · {detail.execution.failed} failed</span>
            {#each detail.execution.failed_ids ?? [] as id}<code>{id}</code>{/each}
          {/if}
        </aside>
      </Card>
    {/if}
  </div>
{/if}
```

  "Confirm stage deletion" and "Confirm cancel manifest" keep
  `tone="danger" surface="solid"`.
- Styles: `.manifests.has-detail { display: grid; grid-template-columns: minmax(0, 2fr) minmax(16rem, 1fr); gap: var(--space-4); align-items: start; }`
  and `@media (max-width: 900px) { .manifests.has-detail { grid-template-columns: 1fr; } }`.
  `.result` is neutral (`border: 1px solid var(--border-default); background: var(--bg-subtle)`,
  no radius on the same rule as a background if `kit-ui-check` flags it).
  `.warning` keeps the amber border. `.reason` uses `var(--text-secondary)`.

- [ ] **Step 1: Write the failing tests**

```ts
it('explains how to start when nothing is selected', async () => {
  render(DeletionsWorkspace, { client: createAPIClient(vi.fn<typeof fetch>(async () => Response.json({ manifests: [] }))) });
  expect(screen.getByText('Nothing selected for deletion')).toBeDefined();
  expect(screen.getByText('Select items in Everything, then choose Review for deletion…')).toBeDefined();
  expect(screen.queryByRole('button', { name: 'Review selection' })).toBeNull();
  expect(await screen.findByText('No staged deletions')).toBeDefined();
  expect(screen.getByText('msgvault delete-staged').tagName).toBe('CODE');
});

it('summarizes a review with size, relative expiry, and only the staging reason', async () => {
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/explore/preflight')) return Response.json(preflight({
      count: 2, deletable_count: 2, estimated_bytes: 3 * 1024, expires_at: '2026-07-19T12:00:00Z',
      unavailable_actions: [
        { action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' },
        { action: 'export', reason: 'browser_export_requires_single_message' },
        { action: 'open_in_source', reason: 'trusted_source_link_unavailable' }
      ]
    }));
    return Response.json({ manifests: [] });
  });
  render(DeletionsWorkspace, { client: createAPIClient(fetchFn), selection: explicit, now: () => new Date('2026-07-19T10:00:00Z') });
  await fireEvent.click(await screen.findByRole('button', { name: 'Review selection' }));

  expect(await screen.findByText('2 items · 3 KB')).toBeDefined();
  expect(screen.getByText('in 2 hours').closest('span')?.textContent).toBe('Review expires in 2 hours');
  const reason = screen.getByText('None of the selected items can be deleted from their source.');
  expect(reason.getAttribute('title')).toBe('selection_contains_items_that_cannot_be_deleted_from_source');
  expect(screen.queryByText(/Export works for one message/)).toBeNull();
  expect(screen.queryByText(/provide links to open/)).toBeNull();
  expect((screen.getByRole('button', { name: 'Stage deletion…' }) as HTMLButtonElement).disabled).toBe(true);
});

it('makes only the confirmation button red', async () => {
  render(DeletionsWorkspace, { client: createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    return new URL(request.url).pathname.endsWith('/explore/preflight') ? Response.json(preflight()) : Response.json({ manifests: [] });
  })), selection: explicit });
  await fireEvent.click(await screen.findByRole('button', { name: 'Review selection' }));
  const stage = await screen.findByRole('button', { name: 'Stage deletion…' });
  expect(stage.className).toContain('kit-button--info');
  expect(stage.className).toContain('kit-button--solid');
  expect(screen.getByRole('button', { name: 'Dry run' }).className).toContain('kit-button--outline');
  await fireEvent.click(stage);
  expect(screen.getByRole('button', { name: 'Confirm stage deletion' }).className).toContain('kit-button--danger');
});

it('lists manifests with status chips and closes the detail', async () => {
  render(DeletionsWorkspace, { client: createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    if (new URL(request.url).pathname.endsWith('/batch-1')) return Response.json({ ...listResponse().manifests[0], account: 'archive@example.com' });
    return Response.json(listResponse());
  })) });
  const table = await screen.findByRole('table', { name: 'Deletion manifests' });
  expect(within(table).getAllByRole('columnheader').map((cell) => cell.textContent?.trim()))
    .toEqual(['ID', 'Description', 'Items', 'Status', 'Created', 'Actions']);
  expect(within(table).getByText('Pending')).toBeDefined();
  expect(within(table).getByRole('button', { name: 'Cancel batch-1' }).className).not.toContain('kit-button--danger');
  await fireEvent.click(within(table).getByRole('button', { name: 'Inspect batch-1' }));
  expect(await screen.findByText('archive@example.com')).toBeDefined();
  await fireEvent.click(screen.getByRole('button', { name: 'Close manifest detail' }));
  expect(screen.queryByText('archive@example.com')).toBeNull();
});
```

Existing tests: rename "Stage deletion" to "Stage deletion…" (:70, :102, :148,
:253, :399, :428); "1 item · 120 bytes" (:374) → "1 item · 120 B"; :398
`/selection_contains_items_that_cannot_be_deleted_from_source/` → the
sentence; :359 `findAllByText('cancelled')` → `findAllByText('Cancelled')`.
The partial-staging assertions (:146, :156) still find `role="alert"`. Import
`within` from Testing Library.

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/deletions src/lib/components/shell`.
  Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): give Deletions empty states, a review panel, and a manifests table`.

---

### Task 7: Selection bar staging reason

**Files:**
- Modify: `web/src/lib/components/explore/SelectionBar.svelte:44-46,111-118`
- Test: `web/src/lib/components/explore/SelectionBar.test.ts`

**Interfaces:**
- Consumes: `preflightReasonLabel` (already imported).

Implementation:

```ts
const stageReason = $derived(preflight?.unavailable_actions.find((item) => item.action === 'stage_deletion')?.reason);
const stageReasonID = $props.id();
```

```svelte
{#if onReviewDeletion}
  <Button
    size="sm"
    surface="soft"
    label="Review for deletion…"
    disabled={Boolean(stageReason)}
    ariaDescribedby={stageReason ? stageReasonID : undefined}
    onclick={() => onReviewDeletion(selection.mode === 'all_matching' ? 'all_matching' : 'explicit')}
  />
  {#if stageReason}
    <span id={stageReasonID} class="action-reason" title={stageReason}>{preflightReasonLabel('stage_deletion', stageReason)}</span>
  {/if}
{/if}
```

- [ ] **Step 1: Write the failing test**

```ts
it('disables Review for deletion with the staging reason as a sentence', async () => {
  const selection = new ExploreSelectionState();
  selection.selectVisible(['message:1']);
  const onReviewDeletion = vi.fn();
  render(SelectionBar, {
    selection, totalCount: 2, onReviewDeletion,
    preflight: preflight([{ action: 'stage_deletion', reason: 'selection_contains_items_that_cannot_be_deleted_from_source' }])
  });
  const review = screen.getByRole('button', { name: 'Review for deletion…' }) as HTMLButtonElement;
  expect(review.disabled).toBe(true);
  const reason = screen.getByText('None of the selected items can be deleted from their source.');
  expect(review.getAttribute('aria-describedby')).toBe(reason.id);
  expect(reason.getAttribute('title')).toBe('selection_contains_items_that_cannot_be_deleted_from_source');
});

it('keeps Review for deletion enabled when staging is available', () => {
  const selection = new ExploreSelectionState();
  selection.selectVisible(['message:1']);
  render(SelectionBar, { selection, totalCount: 2, onReviewDeletion: vi.fn(), preflight: preflight() });
  expect((screen.getByRole('button', { name: 'Review for deletion…' }) as HTMLButtonElement).disabled).toBe(false);
});
```

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/explore/SelectionBar.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): explain unavailable deletion review in the selection bar`.

---

### Task 8: Settings save bar, posture copy, own-save notes, and Notice

**Files:**
- Modify: `web/src/lib/components/settings/SettingsWorkspace.svelte`
  (`postureText` :355-366, footer :451-465, layout `footer` :620, notices
  :623-636, category header :652-665, section loop :667-683, enrichment
  :685-689, CardDAV and People :638-648, styles :753-769)
- Test: `web/src/lib/components/settings/SettingsWorkspace.test.ts`,
  `web/src/App.test.ts:115,141`

**Interfaces:**
- Consumes: `resolvedCategory` (Task 3).

Changes:

- Module script constant:
  `const OWN_SAVE_NOTE = 'These save immediately when you use their buttons — not with Save changes.';`
- Footer snippet:

```svelte
{#snippet settingsFooter()}
  <span class="unsaved" role="status">
    {`${dirtyCount} unsaved ${dirtyCount === 1 ? 'change' : 'changes'}${incompleteDrafts > 0 ? '. Enter a number to save.' : ''}`}
  </span>
  <Button label="Discard" disabled={saving} onclick={discard} />
  <Button disabled={saving || incompleteDrafts > 0} tone="info" surface="solid"
    label={saving ? 'Saving…' : 'Save changes'} onclick={() => void saveSettings()} />
{/snippet}
```

  `footer={resolvedCategory === 'carddav' || resolvedCategory === 'people' || dirtyCount === 0 ? undefined : settingsFooter}`.
- Focus: the save bar disappears after a successful save or a discard, taking
  the focused button with it. Give the category `h2` `tabindex="-1"` and
  `id="settings-category-heading"`, and focus it after `discardChanges()` in a
  new `discard()` handler and after a successful save:
  `await tick(); root?.querySelector<HTMLElement>('#settings-category-heading')?.focus();`.
- `postureText`:

```ts
switch (posture) {
  case 'live':
    return 'Saved changes apply right away — no restart needed.';
  case 'restart':
    return 'Saved changes apply after the daemon restarts.';
  case 'mixed':
    return 'Most saved changes apply after the daemon restarts. Rows that differ are marked.';
  default:
    return 'Set in config.toml on the daemon host.';
}
```

- Own-save notes, each `<p class="own-save">{OWN_SAVE_NOTE}</p>`:
  - Provider credentials: in both the sectioned loop and the card loop, before
    the first row whose setting is a credential control
    (`setting.kind === 'secret' && setting.credential_id && !isReadOnly(setting)`).
    Compute the key once per section or group:
    `{@const firstOwnSave = section.settings.find(isCredentialControl)?.key}`.
  - Enrichment providers: directly under the existing "Provider API keys apply
    right away." line, above `.provider-list`.
  - CardDAV account: above `<CardDAVSettingsWorkspace>`.
  - People sweep: above `<PeopleInferenceSettings>`.
  Existing apply notes stay.
- Plain-HTTP warning:

```svelte
{#if plainHTTPWarning}
  <Notice tone="warning" message="This browser session uses plain HTTP, so its cookie cannot use the Secure flag. Prefer HTTPS for remote access." />
{/if}
```

  Compact it in this component: `.notices :global(.kit-notice) { padding: var(--space-3) var(--space-4); gap: var(--space-3); }`.
  Delete the `.notice--warning` usage; the `.notice` rules stay for the error
  and pending-restart notices.

- [ ] **Step 1: Write the failing tests**

```ts
it('shows the save bar only while drafts exist', async () => {
  render(SettingsWorkspace, { client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"'))) });
  await screen.findByRole('heading', { level: 2, name: 'Appearance' });
  expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
  expect(screen.queryByText(/unsaved/)).toBeNull();

  await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
  expect(screen.getByText('1 unsaved change')).toBeDefined();
  const save = screen.getByRole('button', { name: 'Save changes' });
  expect(save.className).toContain('kit-button--info');
  expect(save.className).toContain('kit-button--solid');

  await fireEvent.click(screen.getByRole('button', { name: 'Discard' }));
  expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull();
  expect(document.activeElement).toBe(screen.getByRole('heading', { level: 2, name: 'Appearance' }));
});

it('states when saved changes apply for each posture', async () => {
  render(SettingsWorkspace, { client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"'))) });
  expect(await screen.findByText('Saved changes apply right away — no restart needed.')).toBeDefined();
  await openSettingsCategory('Daemon');
  expect(screen.getByText('Set in config.toml on the daemon host.')).toBeDefined();
  await openSettingsCategory('Integrations');
  expect(screen.getByText('Saved changes apply after the daemon restarts.')).toBeDefined();
});

it('marks controls that save on their own', async () => {
  const document = {
    ...initialSettings,
    settings: [...initialSettings.settings, setting('vector.embeddings.api_key', undefined, {
      group: 'search', section: 'provider', label: 'Text embedding API key', kind: 'secret',
      credential_id: 'vector.embeddings', secret: { configured: false }
    })]
  };
  const client = createAPIClient(vi.fn<typeof fetch>(async (input) => {
    const path = new URL(input instanceof Request ? input.url : String(input)).pathname;
    return path === '/api/v1/settings' ? settingsResponse(document, '"etag-a"') : Response.json({}, { status: 404 });
  }));
  render(SettingsWorkspace, { client, category: 'search' });
  const note = 'These save immediately when you use their buttons — not with Save changes.';
  expect(await screen.findAllByText(note)).toHaveLength(1);
  await openSettingsCategory('CardDAV account');
  expect(screen.getByText(note)).toBeDefined();
  await openSettingsCategory('People sweep');
  expect(screen.getByText(note)).toBeDefined();
});

it('shows the plain-HTTP warning as a status notice', async () => {
  render(SettingsWorkspace, {
    client: createAPIClient(vi.fn<typeof fetch>(async () => settingsResponse(initialSettings, '"etag-a"'))),
    plainHTTPWarning: true
  });
  const warning = (await screen.findByText(/uses plain HTTP/)).closest('[role="status"]');
  expect(warning?.getAttribute('data-tone')).toBe('warning');
  expect(screen.queryByRole('alert')).toBeNull();
});
```

Update existing tests: every `'Save settings'` → `'Save changes'`; every
"No unsaved changes" plus disabled-Save assertion (:306, :340, :376-377, :384,
:396, :588-589, :597-599, :620-622, :652-653) → the save button is absent
(`queryByRole('button', { name: 'Save changes' })` is `null`); posture strings
at :263 and :273; :274 `getByRole('alert')` → the status check above.
`App.test.ts:115,141`: "Save settings" → "Save changes".

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/settings src/App.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): show the Settings save bar only with drafts`.

---

### Task 9: Saved appearance reaches the open tab

**Files:**
- Modify: `web/src/lib/theme/preferences.svelte.ts` (add `SavedAppearance`,
  `mergeSavedAppearance`)
- Modify: `web/src/lib/components/settings/SettingsWorkspace.svelte`
  (`saveSettings` :249-299, props, Appearance note in the category header)
- Modify: `web/src/App.svelte` (snippet :96-104, new handler)
- Modify: `internal/api/settings_metadata.go:100`
- Test: `web/src/lib/theme/preferences.test.ts`,
  `SettingsWorkspace.test.ts`, `web/src/App.test.ts`

**Interfaces:**
- Produces:

```ts
export interface SavedAppearance {
  theme?: string;
  density?: string;
  defaultSearchMode?: string;
}

export function mergeSavedAppearance(current: AppearanceDefaults, saved: SavedAppearance): AppearanceDefaults {
  return {
    theme: saved.theme !== undefined && isTheme(saved.theme) ? saved.theme : current.theme,
    density: saved.density !== undefined && isDensity(saved.density) ? saved.density : current.density
  };
}
```

- `SettingsWorkspace` prop `onAppearanceSaved?: (saved: SavedAppearance) => void`.
  After a successful PATCH, if `updates` contained `web.theme`, `web.density`,
  or `web.default_search_mode`, call it with the saved string value of each
  such key from `result.settings` (field names `theme`, `density`,
  `defaultSearchMode`):

```ts
const APPEARANCE_FIELDS: Readonly<Record<string, keyof SavedAppearance>> = {
  'web.theme': 'theme',
  'web.density': 'density',
  'web.default_search_mode': 'defaultSearchMode'
};

function savedAppearance(saved: readonly SettingState[], keys: ReadonlySet<string>): SavedAppearance | undefined {
  const result: SavedAppearance = {};
  for (const setting of saved) {
    const field = Object.hasOwn(APPEARANCE_FIELDS, setting.key) ? APPEARANCE_FIELDS[setting.key] : undefined;
    const value = setting.value && 'string' in setting.value ? setting.value.string : undefined;
    if (field && keys.has(setting.key) && typeof value === 'string') result[field] = value;
  }
  return Object.keys(result).length > 0 ? result : undefined;
}
```

- Appearance note, in the category header after the posture line when
  `group.id === 'browser'`:
  `<p class="posture-note">Use daemon theme and Density: Auto in the Display menu return this tab to these saved values.</p>`.
- `App.svelte`:

```ts
function appearanceSaved(saved: SavedAppearance): void {
  appearanceDefaults = mergeSavedAppearance(appearanceDefaults, saved);
  const mode = parseSearchMode(saved.defaultSearchMode);
  // The open view keeps its mode and URL; tabs opened later without a mode
  // in their link read this browser's remembered mode first.
  if (mode) rememberSearchMode(mode, availableSearchModeStorage());
}
```

  Pass `onAppearanceSaved={appearanceSaved}` to `SettingsWorkspace`. Do not
  change `searchModeDefault`: AppShell re-resolves the open view's mode when
  it changes (`AppShell.svelte:614-617`,
  `ExploreState.setConfiguredDefaultSearchMode` at
  `explore/state.svelte.ts:673-683`). Theme and density need no AppShell
  change: its effect at `AppShell.svelte:552-555` already calls
  `appearance.setDefaults`, and `AppearancePreferences.current` keeps a
  Display menu override ahead of the defaults.
- `settings_metadata.go:100`:
  `"web.default_search_mode": {"Default search mode", "Used when a tab opens without a search mode in its link. Your current search keeps its mode.", ""},`

- [ ] **Step 1: Write the failing tests**

`preferences.test.ts`:

```ts
it('merges only valid saved appearance values', () => {
  const current = { theme: 'system', density: 'compact' } as const;
  expect(mergeSavedAppearance(current, { theme: 'dark' })).toEqual({ theme: 'dark', density: 'compact' });
  expect(mergeSavedAppearance(current, { density: 'comfortable', defaultSearchMode: 'hybrid' }))
    .toEqual({ theme: 'system', density: 'comfortable' });
  expect(mergeSavedAppearance(current, { theme: 'neon', density: 'roomy' })).toEqual(current);
});
```

`SettingsWorkspace.test.ts`:

```ts
it('reports saved appearance values and nothing for other settings', async () => {
  const onAppearanceSaved = vi.fn();
  const fetchFn = vi.fn<typeof fetch>()
    .mockResolvedValueOnce(settingsResponse(initialSettings, '"etag-a"'))
    .mockResolvedValueOnce(settingsResponse({
      ...initialSettings,
      settings: initialSettings.settings.map((item) => item.key === 'web.theme' ? { ...item, value: { string: 'dark' } } : item)
    }, '"etag-b"'));
  render(SettingsWorkspace, { client: createAPIClient(fetchFn), onAppearanceSaved });
  await chooseSelectOption(await screen.findByLabelText('Theme'), 'Dark');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(onAppearanceSaved).toHaveBeenCalledWith({ theme: 'dark' }));
  expect(screen.getByText('Use daemon theme and Density: Auto in the Display menu return this tab to these saved values.')).toBeDefined();
});
```

  Add a second case that saves `vector.embeddings.endpoint` and asserts
  `onAppearanceSaved` was not called.

`App.test.ts` (add the helper below and three tests):

```ts
function appearanceDaemon() {
  const current = { theme: 'system', density: 'compact', mode: 'full_text' };
  const document = () => Response.json({
    groups: [{ id: 'browser', label: 'Appearance', description: 'How the web app looks.' }],
    settings: [
      { key: 'web.theme', group: 'browser', label: 'Theme', kind: 'string', value: { string: current.theme },
        options: ['system', 'light', 'dark'], restart_required: false },
      { key: 'web.density', group: 'browser', label: 'Density', kind: 'string', value: { string: current.density },
        options: ['compact', 'comfortable'], restart_required: false },
      { key: 'web.default_search_mode', group: 'browser', label: 'Default search mode', kind: 'string',
        value: { string: current.mode }, options: ['full_text', 'semantic', 'hybrid'], restart_required: false }
    ],
    pending_restart: false
  }, { headers: { ETag: '"appearance"' } });
  return vi.fn<typeof fetch>(async (input) => {
    const request = input instanceof Request ? input : new Request(input);
    const path = new URL(request.url).pathname;
    if (path === '/api/session') return Response.json({ auth_mode: 'loopback', https: false, plain_http_warning: false });
    if (path === '/api/v1/settings' && request.method === 'PATCH') {
      const body = (await request.json()) as { updates: Array<{ key: string; value: { string: string } }> };
      for (const { key, value } of body.updates) {
        if (key === 'web.theme') current.theme = value.string;
        if (key === 'web.density') current.density = value.string;
        if (key === 'web.default_search_mode') current.mode = value.string;
      }
      return document();
    }
    if (path === '/api/v1/settings') return document();
    if (path === '/api/v1/explore') return Response.json({ rows: [], total_count: 0, cache_revision: 'appearance', search_provenance: {} });
    return Response.json({}, { status: 404 });
  });
}

async function openAppearance(fetchFn: ReturnType<typeof appearanceDaemon>) {
  window.history.replaceState(null, '', '/?workspace=settings');
  const session = createSessionController(fetchFn);
  render(App, { session });
  await session.bootstrap();
  await screen.findByRole('heading', { level: 2, name: 'Appearance' });
}

it('applies a saved theme and density to the open tab', async () => {
  sessionStorage.removeItem('msgvault.appearance.override');
  await openAppearance(appearanceDaemon());
  await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
  await chooseSelectOption(screen.getByLabelText('Density'), 'Comfortable');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(document.documentElement.classList.contains('dark')).toBe(true));
  expect(document.documentElement.dataset.density).toBe('comfortable');
});

it('keeps a Display menu theme override ahead of a saved theme', async () => {
  sessionStorage.setItem('msgvault.appearance.override', JSON.stringify({ theme: 'light' }));
  try {
    await openAppearance(appearanceDaemon());
    await chooseSelectOption(screen.getByLabelText('Theme'), 'Dark');
    await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Save changes' })).toBeNull());
    expect(document.documentElement.classList.contains('dark')).toBe(false);
  } finally {
    sessionStorage.removeItem('msgvault.appearance.override');
  }
});

it('remembers a saved default search mode without changing the open view', async () => {
  localStorage.removeItem(SEARCH_MODE_PREFERENCE_KEY);
  await openAppearance(appearanceDaemon());
  const before = window.location.search;
  await chooseSelectOption(screen.getByLabelText('Default search mode'), 'Hybrid');
  await fireEvent.click(screen.getByRole('button', { name: 'Save changes' }));
  await waitFor(() => expect(localStorage.getItem(SEARCH_MODE_PREFERENCE_KEY)).toBe('hybrid'));
  expect(window.location.search).toBe(before);
  expect(screen.getByRole('radio', { name: 'Full text' }).getAttribute('aria-checked')).toBe('true');
  expect(resolveInitialSearchMode(undefined, localStorage, 'full_text')).toBe('hybrid');
});
```

  Clean up with the file's existing `afterEach`; add
  `localStorage.removeItem(SEARCH_MODE_PREFERENCE_KEY)` and
  `document.documentElement.classList.remove('dark')` there if it does not
  already reset them. Import `resolveInitialSearchMode` from
  `./lib/search/modes`.

- [ ] **Step 2: Run** `bunx vitest run src/lib/theme src/lib/components/settings src/App.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement**, including the Go string.
- [ ] **Step 4: Run** the Vitest files, `bun run check`, `bun run check:kit-ui`,
  and from the repository root
  `GOTOOLCHAIN=go1.27.1 go test -tags "fts5 sqlite_vec" ./internal/api/ -run 'Settings'`,
  `go fmt ./internal/api/`, `go vet -tags "fts5 sqlite_vec" ./internal/api/`.
  Expected: PASS, no diff from `go fmt`.
- [ ] **Step 5: Commit.** Subject: `feat(web): apply saved appearance to the open tab`.

---

### Task 10: Sign-in and boot screens

**Files:**
- Modify: `web/src/app.css` (shared rule)
- Modify: `web/src/App.svelte:87-119` (markup), `:121-156` (delete styles)
- Modify: `web/src/lib/components/auth/Login.svelte:15-17` (classes),
  `:46-91` (delete duplicated rules)
- Test: `web/src/App.test.ts`, `web/src/lib/components/auth/Login.test.ts`

Shared rule in `app.css`:

```css
/* Sign-in and boot screens share one narrow column with a brand line. */
.boot-screen,
.boot-screen > form {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: var(--space-5);
}

.boot-screen {
  max-width: 28rem;
  margin: 0 auto;
  padding: var(--space-8) var(--space-6);
  font-size: var(--font-size-md);
}

.boot-screen p,
.boot-screen h1 {
  margin: 0;
}

.boot-screen h1 {
  font-size: var(--font-size-xl);
  font-weight: 650;
}

.boot-screen__brand {
  color: var(--text-primary);
  font-weight: 650;
}

.boot-screen p:not(.boot-screen__brand) {
  color: var(--text-muted);
}

.boot-screen p[role='alert'] {
  color: var(--text-danger);
}
```

Markup:

- OAuth callback:
  `<main class="boot-screen"><p class="boot-screen__brand">msgvault</p><p>Return to CardDAV settings to finish connecting. You can close this window.</p></main>`.
- Connection error and connecting: `class="boot-screen"` and
  `class="boot-screen__brand"`; copy and `aria-label`s unchanged.
- Login: `<main class="boot-screen" aria-label="Authentication">`, the form
  keeps `aria-label="Log in"`, the brand paragraph uses
  `class="boot-screen__brand"`. Login keeps only its form-specific rules
  (`form { align-self: stretch; }`, `form > :global(*) { align-self: stretch; }`,
  `form > :global(button) { align-self: flex-start; }`).

- [ ] **Step 1: Write the failing test** (`App.test.ts`)

```ts
it('shows the OAuth callback in the same layout as connecting', () => {
  vi.stubGlobal('BroadcastChannel', class { postMessage(): void {} close(): void {} });
  const close = vi.spyOn(window, 'close').mockImplementation(() => undefined);
  window.history.replaceState(null, '', '/?state=msgvault-carddav-synthetic&code=synthetic');
  try {
    render(App, { session: createSessionController(vi.fn<typeof fetch>()) });
    const main = screen.getByRole('main');
    expect(main.className).toContain('boot-screen');
    expect(within(main).getByText('msgvault').className).toContain('boot-screen__brand');
    expect(within(main).getByText('Return to CardDAV settings to finish connecting. You can close this window.')).toBeDefined();
  } finally {
    close.mockRestore();
    vi.unstubAllGlobals();
    window.history.replaceState(null, '', '/');
  }
});
```

  In `Login.test.ts`, assert the `main` named "Authentication" has class
  `boot-screen` and the brand paragraph has `boot-screen__brand`.

- [ ] **Step 2: Run** `bunx vitest run src/App.test.ts src/lib/components/auth`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `refactor(web): share one layout for sign-in and boot screens`.

---

### Task 11: Browser tests and axe

**Files:**
- Modify: `web/tests/operations.spec.ts:60` ("View source operations" →
  "Sync history")
- Modify: `web/tests/e2e/operations.spec.ts` (:80-94 document case, :124
  "Refresh operations" → "Reload run history"; new refresh test)
- Modify: `web/tests/e2e/fixtures/operations.ts` (count operation status
  reads)
- Modify: `web/tests/e2e/accessibility.spec.ts:34` ("Refresh operations" →
  "Reload run history"; new Manage test)
- Modify: `web/tests/session-navigation.spec.ts:113` ("Save settings" →
  "Save changes"; new Settings tests)
- Modify: `web/tests/e2e/security.spec.ts:151`
  (`expect(page.getByRole('button', { name: 'Save changes' })).toHaveCount(0)`)
- Modify: `web/tests/archive-management.spec.ts` and any spec Step 1 finds

- [ ] **Step 1: Find affected specs**

```bash
rg -ln "View source operations|Refresh operations|Save settings|No unsaved changes|'Stage deletion'|stale_last_result|sync_start_not_observed|Open document (index|vector) settings|No deletion manifests yet|Reviewed staging|History available|Unspecified|Latest result" tests
```

- [ ] **Step 2: Update each spec** to the new names without weakening
  assertions. In `e2e/operations.spec.ts:80-94`, the `document_extraction`
  case expects "Document indexing setup" (link, `target="_blank"`) and no
  settings button; the visual case still clicks "Open visual attachment
  settings" and lands on Settings with the Search category heading.

- [ ] **Step 3: Add coverage**

Fixture: in `installOperations`, count `**/api/v1/operations/status` reads
and expose `operationStatusReads: number` on `InstalledOperations` (a getter
over the counter).

`e2e/operations.spec.ts`:

```ts
test('the refresh control reloads status only and keeps paged history, detail, and focus', async ({ page }) => {
  await page.clock.install();
  const fixture = await installOperations(page);
  await page.goto(operationsURL);
  await page.getByRole('button', { name: 'Load more operation history' }).click();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toBeVisible();
  const sourceRow = page.getByRole('button', { name: 'Open Source sync run' });
  await sourceRow.click();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toBeVisible();
  await sourceRow.focus();
  const listed = fixture.listQueries.length;
  const reads = fixture.operationStatusReads;

  await page.clock.fastForward('05:00');
  await expect.poll(() => fixture.operationStatusReads).toBe(reads + 1);
  expect(fixture.listQueries.length).toBe(listed);
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toBeVisible();
  await expect(page.getByRole('region', { name: 'Operation run detail' })).toBeVisible();
  await expect(sourceRow).toBeFocused();

  const refresh = page.getByRole('button', { name: 'Refresh operation status' });
  await refresh.click();
  await expect.poll(() => fixture.operationStatusReads).toBe(reads + 2);
  expect(fixture.listQueries.length).toBe(listed);
  await expect(refresh).toBeFocused();

  await page.getByRole('button', { name: 'Reload run history' }).click();
  await expect.poll(() => fixture.listQueries.length).toBe(listed + 1);
  expect(fixture.listQueries.at(-1)?.get('cursor')).toBeNull();
  await expect(page.getByRole('button', { name: 'Open Person fact sweep run' })).toHaveCount(0);
});
```

`session-navigation.spec.ts` (routes on the context so a second tab shares
them):

```ts
// Merge into the file's existing imports.
import { expect, test, type BrowserContext } from '@playwright/test';
import { expectKitTheme, selectKitOption, setKitTheme } from './kit-ui';

async function installSettingsDaemon(context: BrowserContext): Promise<void> {
  const current: Record<string, string> = {
    'web.theme': 'system', 'web.density': 'compact', 'web.default_search_mode': 'full_text'
  };
  const options: Record<string, string[]> = {
    'web.theme': ['system', 'light', 'dark'],
    'web.density': ['compact', 'comfortable'],
    'web.default_search_mode': ['full_text', 'semantic', 'hybrid']
  };
  const labels: Record<string, string> = {
    'web.theme': 'Theme', 'web.density': 'Density', 'web.default_search_mode': 'Default search mode'
  };
  const document = () => ({
    groups: [
      { id: 'browser', label: 'Appearance', description: 'How the web app looks.' },
      { id: 'server', label: 'Daemon', description: 'How the daemon runs.' },
      { id: 'search', label: 'Search', description: 'Semantic search.' }
    ],
    settings: [
      ...Object.keys(current).map((key) => ({
        key, group: 'browser', label: labels[key], kind: 'string', value: { string: current[key] },
        options: options[key], restart_required: false
      })),
      { key: 'server.log_level', group: 'server', label: 'Log level', kind: 'string', value: { string: 'info' }, restart_required: true },
      { key: 'vector.enabled', group: 'search', label: 'Semantic search', kind: 'boolean', value: { boolean: false }, restart_required: true }
    ],
    pending_restart: false
  });
  await context.route('**/api/session', (route) => route.fulfill({ json: {
    auth_mode: 'session', csrf_token: 'csrf-token', https: true, plain_http_warning: false
  } }));
  await context.route('**/api/v1/settings', async (route) => {
    if (route.request().method() === 'PATCH') {
      const body = route.request().postDataJSON() as { updates: Array<{ key: string; value: { string: string } }> };
      for (const { key, value } of body.updates) current[key] = value.string;
    }
    await route.fulfill({ headers: { ETag: '"settings"' }, json: document() });
  });
  await context.route('**/api/v1/explore', (route) => route.fulfill({ json: {
    rows: [], total_count: 0, cache_revision: 'settings', search_provenance: {}
  } }));
}
```

```ts
test('Settings keeps its category across reload and Back', async ({ context, page }) => {
  await installSettingsDaemon(context);
  await page.goto('/?workspace=settings');
  const settings = page.getByRole('main', { name: 'Settings' });
  await settings.getByRole('button', { name: 'Search', exact: true }).click();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search' })).toBeVisible();
  await expect.poll(() => JSON.parse(new URL(page.url()).searchParams.get('explore') ?? '{}').settingsCategory).toBe('search');
  await page.reload();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search' })).toBeVisible();
  await settings.getByRole('button', { name: 'Appearance', exact: true }).click();
  await expect(settings.getByRole('heading', { level: 2, name: 'Appearance' })).toBeVisible();
  await page.goBack();
  await expect(settings.getByRole('heading', { level: 2, name: 'Search' })).toBeVisible();
});

test('saved appearance reaches this tab, and the default mode only new tabs', async ({ context, page }) => {
  await installSettingsDaemon(context);
  await page.goto('/?workspace=settings');
  await selectKitOption(page, 'Theme', 'Dark');
  await selectKitOption(page, 'Default search mode', 'Hybrid');
  const before = page.url();
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expectKitTheme(page, 'dark');
  expect(page.url()).toBe(before);
  await expect(page.getByRole('radio', { name: 'Full text' })).toBeChecked();
  expect(await page.evaluate(() => localStorage.getItem('msgvault-search-mode'))).toBe('hybrid');

  const next = await context.newPage();
  await next.goto('/?workspace=everything');
  await expect(next.getByRole('radio', { name: 'Hybrid' })).toBeChecked();
});

test('a Display menu theme override wins over a saved theme', async ({ context, page }) => {
  await installSettingsDaemon(context);
  await page.goto('/?workspace=settings');
  await setKitTheme(page, 'light');
  await selectKitOption(page, 'Theme', 'Dark');
  await page.getByRole('button', { name: 'Save changes' }).click();
  await expect(page.getByRole('button', { name: 'Save changes' })).toHaveCount(0);
  await expectKitTheme(page, 'light');
});
```

`e2e/accessibility.spec.ts`, a new test per theme:

```ts
for (const theme of ['light', 'dark'] as const) {
  test(`${theme} Manage pages and their states have no axe violations`, async ({ page }) => {
    test.slow();
    await installMixedArchive(page);
    await page.route('**/api/v1/sources/status', (route) => route.fulfill({ json: { sources: [{
      id: 1, source_type: 'mbox', identifier: 'import@example.com', display_name: 'Synthetic import',
      last_sync_at: null, updated_at: '2026-07-19T10:00:00Z', active_sync: null, last_successful_sync: null,
      can_sync: false, sync_unavailable_reason: 'source_not_schedulable', scheduled: false, next_sync_at: null,
      latest_sync: { id: 9, source_id: 1, started_at: '2026-07-19T10:00:00Z', completed_at: '2026-07-19T10:01:00Z',
        status: 'failed', messages_processed: 1, messages_added: 0, messages_updated: 0, errors_count: 1,
        error_message: 'Synthetic failure', item_errors: [{ source_message_id: 'm-1', phase: 'ingest',
          error_kind: 'mime_error', error_message: 'Malformed MIME header', created_at: '2026-07-19T10:01:00Z' }] }
    }] } }));
    await page.goto('/');
    await setKitTheme(page, theme);

    await selectWorkspace(page, 'Sources');
    await page.getByRole('button', { name: 'Show details for Synthetic import' }).click();
    await expect(page.getByText('Malformed MIME header')).toBeVisible();
    await assertNoViolations(page, `Sources row detail ${theme}`);

    await selectWorkspace(page, 'Deletions');
    await expect(page.getByText('Nothing selected for deletion')).toBeVisible();
    await assertNoViolations(page, `Deletions empty ${theme}`);

    await selectWorkspace(page, 'Everything');
    const grid = page.getByRole('grid', { name: 'Everything results' });
    await grid.focus();
    await page.keyboard.press('Space');
    await page.keyboard.press('d');
    await page.getByRole('button', { name: 'Cancel' }).click();
    await expect(page.getByRole('button', { name: 'Stage deletion…' })).toBeVisible();
    await assertNoViolations(page, `Deletions review ${theme}`);

    await selectWorkspace(page, 'Settings');
    await selectKitOption(page, 'Theme', theme === 'light' ? 'Dark' : 'Light');
    await expect(page.getByRole('button', { name: 'Save changes' })).toBeVisible();
    const nav = page.getByRole('main', { name: 'Settings' }).getByRole('navigation');
    for (const category of await nav.getByRole('button').all()) {
      await category.click();
      await assertNoViolations(page, `Settings ${await category.textContent()} with a draft ${theme}`);
    }
  });
}
```

  Inspect kit `SettingsLayout` for the category navigation's actual role and
  name before relying on `getByRole('navigation')`; adjust the locator to
  what it renders. If `installMixedArchive`'s Settings fixture has no
  `web.theme` row, route `**/api/v1/settings` with Task 9's document first.
  The existing Operations axe test already covers the status list; add a
  pass after opening "Open Document index status" with document extraction
  unconfigured (`fixture.setOperationConfigured('document_extraction', false)`).

- [ ] **Step 4: Run** `make web-test-browser` from the repository root.
  Expected: all pass.
- [ ] **Step 5: Commit.** Subject: `test(web): cover the Manage pages in the browser`.
  List every existing spec edit and why in the body.

---

### Task 12: Web UI guide and docs screenshots

**Files:**
- Modify: `docs/web-ui.md`
- No committed images: docs media live on the `docs-assets` branch
  (`docs/README.md`, "Media"); `make docs-check` fails on tracked PNGs.

- [ ] **Step 1: Update the guide.** Keep its structure and voice
  (`AGENTS.md` "Documentation"). Verify each statement against the running
  branch build before writing it. Set `last_edited` to the edit date. Edits:
  - Workspace table (:14-24): "Saved Views" → "Saved views"; add a sentence
    that the sidebar groups workspaces as People (Relationships, Directory,
    Reviews), Archive (Everything, Files, Saved views), and Manage (Sources,
    Operations, Deletions, Settings), collapses to an icon rail, and becomes
    a menu below 900px; the top bar holds global search, the theme toggle,
    and the Display menu (temporary density and "Use daemon theme").
  - "Explore and search" (:80-120): describe the one toolbar (Filters, Show
    as, Group by, Sort, Columns, count), removable context chips, and the
    selection bar's "Review for deletion…" and its disabled reason.
  - "Files and containing context" (:162-179): Files is the single file view
    with Type and Sort menus and a Visual search toggle.
  - "Directory and Reviews" (:210-243): Filters panel with two date fields
    and chips, the person header actions ("Open relationship", "Review
    facts", More actions), and the Facts person picker.
  - "Saved views" (:250-270): "Save view…" in Everything and Files; the
    library's Open, Edit, and Delete actions.
  - "Sources and sync status" (:272-303): Status chips (Syncing, Completed,
    Completed with errors, Failed, Never synced); readable reasons with the
    code in a tooltip; "Show details for {name}"; replace
    `stale_last_result` and `sync_start_not_observed` with their sentences;
    "Sync history".
  - "Operations" (:305-336): the status list with Off, "No runs yet", and Set
    up; host-configured document kinds with their guide links; the refresh
    control refreshes status every five minutes and on demand, while "Reload
    run history" restarts history from page one; the counters and trigger
    format; the error sentence with its code in the detail.
  - "Deletions" (:338-345): empty states, the review panel, relative expiry,
    "Stage deletion…" then a red confirmation, and the manifests table.
  - "Keyboard controls" (:347-366): regenerate the table from
    `web/src/lib/commands/registry.ts` (for example `A` selects visible rows,
    `Shift+Space` extends the selection) and point to the "Keyboard
    shortcuts" dialog.
  - "Settings and restart behavior" (:368-416): the category is in the link;
    the save bar appears only with unsaved changes; the posture lines; the
    own-save line; saved theme and density apply to the open tab unless a
    Display menu override is active; a saved default search mode applies to
    tabs opened later without a mode in their link.
- [ ] **Step 2: Run** `make docs-check` from the repository root. Expected: PASS.
- [ ] **Step 3: Regenerate the docs screenshots.**
  `docs/screenshots/generate-web-fixture-screenshots.sh` (also
  `make docs-web-screenshots`):
  - Needs `bun`, `go`, `gzip`, `python3`, `curl`, `shasum`, and Playwright's
    Chromium (it uses `PLAYWRIGHT_BROWSERS_PATH`, `~/Library/Caches/ms-playwright`,
    or `/ms-playwright`). It needs network access to
    `git fetch origin docs-fixtures`, unless `MSGVAULT_DOCS_FIXTURE_DIR`
    points at a reviewed local copy of that branch. It does not use Docker;
    Docker is only for the TUI captures in `generate-all.sh`.
  - `hydrate-fixture.sh` refuses a fixture whose commit is not an ancestor of
    `origin/docs-fixtures` or whose mbox or manifest SHA-256 differs from
    `docs/fixtures/fixture.lock.json`. Do not edit the lock or the fixture.
  - It builds the embedded UI (`make web-embed`) and the binary, imports the
    fixture into a private `/tmp/msgvault-docs-capture.*` home, runs
    `serve`, and captures Everything (four theme × density variants) and,
    on darwin only, two Relationships variants.
  - On Linux run
    `MSGVAULT_DOCS_SCREENSHOT_PLATFORM=linux MSGVAULT_DOCS_SCREENSHOT_OUTPUT_DIR=<scratchpad>/docs-shots make docs-web-screenshots`.
    This produces the four `analytical-*-linux.png` files. The six
    `-darwin.png` files (the guide embeds two, the website three) need a
    macOS run with the default platform.
  - View every image. Then hydrate the current assets
    (`bash docs/assets/hydrate-assets.sh`), copy the new PNGs into
    `docs/assets/static/`, and run
    `bash docs/assets/update-static-assets-branch.sh` to build the local
    `docs-assets` branch. `--push` force-pushes that orphan branch; run it
    only with explicit maintainer authorization.
- [ ] **Step 4: Commit** the guide. Subject: `docs: describe the overhauled Web UI`.

---

### Task 13: Verify and open the pull request

- [ ] **Step 1:** From the repository root run
  `make web-check && make web-test && make web-test-browser && make lint-ci`,
  and `GOTOOLCHAIN=go1.27.1 make test` for the Go change.
- [ ] **Step 2:** Capture before and after screenshots at 1440×900 and
  420×860, light and dark, from the Enron docs fixture with the
  `kenn-io-isolate-prod` and `kenn-io-capture-playwright` skills: build
  `origin/main` in a temporary worktree for "before" and this branch for
  "after", and run each daemon with the isolated home and config that
  `generate-web-fixture-screenshots.sh` uses (vector search off, so
  Operations shows Off rows). Pages: Sources with a row detail open,
  Operations, Deletions empty and with a review behind a cancelled
  confirmation, and Settings on Appearance and Search with a draft. View
  every image. Check: no purple or green button, no raw code as the only
  text, gray Off chips, one solid blue button per screen, the save bar only
  with a draft.
- [ ] **Step 3:** Read `git diff origin/main...HEAD` fully; remove leftovers.
  Run the `kenn-io-scrub-private-data` check.
- [ ] **Step 4:** The user has authorized the pull request. Push the branch
  and open it with the `kenn-io-commit-push-pr` and `kenn-io-pr-desc` rules,
  attaching the cleared screenshots. Do not merge.

## Self-review notes

- Spec coverage: Sources (Task 2); Operations status list, Set up targets,
  document panels, and authority removal (Task 4); refresh control, Reload
  run history, runs table, and detail (Task 5); Deletions (Task 6); selection
  bar reason (Task 7); Settings category URL (Task 3); save bar, posture,
  own-save notes, and Notice (Task 8); appearance and the metadata string
  (Task 9); sign-in and boot screens (Task 10); the spec's test table (each
  task's Step 1 and Task 11); guide and screenshots (Task 12).
- Task order differs from the suggested order: the Settings category URL
  state (Task 3) comes before the Operations status list (Task 4) because Set
  up for Person enrichment and People sweep commits `settingsCategory`.
  `document_index` and `document_vector` leave in Task 4, with the panels
  that used them, so every commit compiles.
- Decisions where the spec is silent:
  - New Settings authorities `semantic_search` and `person_embeddings` focus
    `vector.enabled` and `vector.people.enabled`.
  - `settingsCategory` defaults to `'browser'`, accepts
    `/^[a-z][a-z0-9_-]{0,63}$/`, and is not rewritten when the daemon does not
    list it; Settings shows Appearance (kit `SettingsLayout` also falls back
    to the first category).
  - Tooltips use `title`, as Sources and the selection bar already do.
  - An empty `source_type` reads "Gmail" (the daemon treats it as Gmail in
    `classifySourceScheduling`).
  - Counter units are singular for a value of 1, and a counter named for its
    unit is not repeated ("3 projected writes", "1 book"). A table row whose
    counters are all zero reads "No counters".
  - "Last succeeded" shows when the row's shown run did not succeed, because
    run IDs differ per response and cannot be compared.
  - Set up buttons show "Set up" and are named "Set up {kind}" so each is
    distinguishable.
  - An unconfigured related-status panel shows a gray "Off" chip instead of
    the red dot.
  - The Sources "not observed" sentence carries the existing "Refresh"
    button, since the sentence asks the reader to refresh.
  - After a save or discard removes the save bar, focus moves to the
    category heading.
  - After a click on the refresh control, focus returns to it, because kit
    disables the button while busy.
  - The Deletions review panel heading is "Review selection" (was "Reviewed
    staging"); the manifests detail stacks at the shared 900px breakpoint.
  - Sign-in uses the boot screens' 28rem column (was 24rem); the OAuth
    callback screen gets no new `aria-label`.
  - Unparseable operation times read "Not available" everywhere (one read
    "Time unavailable").
