---
title: "Web UI overhaul PR 3: People — implementation plan"
description: "Implementation plan for the People workspace in the Web UI."
last_edited: "2026-09-30"
---

# Web UI overhaul PR 3: People — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task by task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Relationships, Directory, and Reviews the toolbar, chip, label,
and header patterns Everything and Files already use; split the person page
into sections; and link the three People workspaces to each other, without
removing any capability.

**Architecture:** Two small modules hold the new shared logic:
`directory/labels.ts` (readable labels and status tones) and
`directory/dates.ts` (calendar-date and partial-date validation). Directory's
filters move into a disclosure panel with chips. `PersonDetail` gets a header
and a data-driven section list; rename, delete, and profile history move from
`StructuredProfileSection` into a new `PersonRecordActions`. Reviews keeps its
controllers and changes only its presentation, plus a person picker in Facts.

**Tech stack:** Svelte 5 (runes), `@kenn-io/kit-ui` (`SelectDropdown`,
`Typeahead`, `Chip`, `Menu`, `IconButton`, `Button`), `@lucide/svelte`, Vitest
with Testing Library, Playwright.

**Spec:** [PR 3 spec](web-ui-overhaul-pr3-spec.md), which refines the People
sections of the [Web UI overhaul design](web-ui-overhaul-design.md). Read the
spec before each task; it is the authority where this plan is silent.

## Global constraints

- Keep every capability. A control that moves keeps its accessible name unless
  the spec names the change.
- Keep URL state keys, the `explore` JSON format, and API calls unchanged. The
  only state change is dropping an impossible Directory date when the URL is
  read (Task 2).
- No new npm dependencies. Icons come from `@lucide/svelte`.
- One solid primary button per screen: `tone="info" surface="solid"`. Purple
  (`workflow`) and green (`success`) are not button colors.
- Sentence case for every visible label. Monospace only for identifiers,
  hashes, code, keys, and cron text.
- Raw API codes are never the only text a person sees. Unknown codes fall back
  to `sentenceCase` from `explore/labels.ts`.
- `kit-ui-check` forbids native date inputs. The two Directory date fields are
  the approved exception (spec decision 1 and the design's Directory section);
  each carries a `kit-ui-check-ignore` comment with that reason.
- Tests use Vitest and Testing Library or Playwright, query by role and
  accessible name, and use failure-safe cleanup (`afterEach` or
  `try`/`finally`). Synthetic names and `example.com` addresses only.
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
| `web/src/lib/explore/labels.ts` | Export the existing `sentenceCase` |
| `web/src/lib/directory/labels.ts` (new) | Contact state, channel, and review-state labels and tones; date display |
| `web/src/lib/directory/dates.ts` (new) | `isCalendarDate`, `intervalDateError`, `profileDateError` |
| `web/src/lib/explore/state.svelte.ts` | Drop impossible Directory dates when parsing |
| `web/src/lib/components/directory/DirectoryWorkspace.svelte` | Toolbar, Filters panel, date fields, Sort, chips, count, primary Promote button |
| `web/src/lib/components/directory/DirectoryList.svelte` | Readable row text |
| `web/src/lib/components/directory/PersonDetail.svelte` | Header, seven sections, scrolling tablist |
| `web/src/lib/components/directory/PersonRecordActions.svelte` (new) | More actions menu, rename form, delete confirmation, profile history dialog |
| `web/src/lib/components/directory/StructuredProfileSection.svelte` | Loses rename, delete, and history |
| `web/src/lib/components/directory/PersonAgenda.svelte` | Hide the add form unless the integration is ready |
| `web/src/lib/components/directory/{EmploymentEditor,PersonRelationshipEditor,StructuredProfileEditor}.svelte` | Inline date validation |
| `web/src/lib/components/relationships/{RelationshipList,RelationshipHeader,RelationshipsWorkspace}.svelte` | Placeholder, header layout, counts line, People button |
| `web/src/lib/components/directory/{DirectoryReviewCentre,RelationshipReviewQueue,FactReviewPanel,IdentityCandidateCard,RelationshipReviewCard,DirectoryReviewWorkspace}.svelte` | Facts label, hidden headings with visible focus, Show selects, status chips, person picker |
| `web/src/lib/components/shell/AppShell.svelte` | Pass `openRelationship`, `openFacts`, and `client` down |

---

### Task 1: People labels and date rules

**Files:**
- Modify: `web/src/lib/explore/labels.ts:99` (export `sentenceCase`)
- Create: `web/src/lib/directory/labels.ts`, `web/src/lib/directory/labels.test.ts`
- Create: `web/src/lib/directory/dates.ts`, `web/src/lib/directory/dates.test.ts`

**Interfaces:**
- Produces, in `directory/labels.ts`:
  - `PRIMARY_CHANNELS: readonly ['email', 'phone', 'chat']`
  - `contactStateLabel(code: string): string`
  - `channelLabel(code: string): string`
  - `reviewStateChip(code: string): { label: string; tone: ChipTone }`
  - `formatContactDate(iso: string): string` — "Jan 2, 2002"; returns the
    input unchanged when it does not parse.
  - `formatDay(day: string): string` — formats a `YYYY-MM-DD` day in UTC,
    "Jan 5, 2024".
- Produces, in `directory/dates.ts`:
  - `isCalendarDate(value: string): boolean` — `YYYY-MM-DD` that exists.
  - `intervalDateError(value: string): string | null` — `null` for empty or
    valid.
  - `profileDateError(value: string): string | null`
  - `isTextProfileDate(value: string): boolean` — non-empty and not a
    structured shape, so it will be saved as text.

- [ ] **Step 1: Write the failing tests**

`directory/dates.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { intervalDateError, isCalendarDate, isTextProfileDate, profileDateError } from './dates';

describe('isCalendarDate', () => {
  it.each([
    ['2024-02-29', true], ['2023-02-29', false], ['2026-02-31', false],
    ['2026-13-01', false], ['2026-1-01', false], ['', false], ['last week', false],
  ])('%s → %s', (value, want) => expect(isCalendarDate(value)).toBe(want));
});

describe('intervalDateError', () => {
  it.each(['', '  ', '2019', '2019-04', '2019-04-12', '20190412', '2024-02-29'])('accepts %j', (value) => {
    expect(intervalDateError(value)).toBeNull();
  });
  it.each(['--04-12', '--04', '---12', 'spring 2019', '2019-13', '2019-02-30', '0000', '201904', '2019-4'])(
    'rejects %j', (value) => {
      expect(intervalDateError(value)).toBe(
        'Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.');
    });
});

describe('profileDateError', () => {
  it.each(['', '2019', '2019-04', '2019-04-12', '--04-12', '--02-29', '--04', '---12', 'spring 2019'])(
    'accepts %j', (value) => expect(profileDateError(value)).toBeNull());
  it.each(['2024-13', '2023-02-29', '--02-30', '--13', '---32', '0000-01'])('rejects %j', (value) => {
    expect(profileDateError(value)).toBe('This date does not exist. Check the month and day.');
  });
  it('marks free text as a text date only', () => {
    expect(isTextProfileDate('spring 2019')).toBe(true);
    expect(isTextProfileDate('--04-12')).toBe(false);
    expect(isTextProfileDate('2024-13')).toBe(false);
    expect(isTextProfileDate('  ')).toBe(false);
  });
});
```

`directory/labels.test.ts`:

```ts
import { describe, expect, it } from 'vitest';
import { channelLabel, contactStateLabel, formatContactDate, formatDay, reviewStateChip } from './labels';

describe('directory labels', () => {
  it('names known codes and sentence-cases unknown ones', () => {
    expect(contactStateLabel('active')).toBe('Active');
    expect(contactStateLabel('needs_follow_up')).toBe('Needs follow up');
    expect(channelLabel('email')).toBe('Email');
    expect(channelLabel('carrier_pigeon')).toBe('Carrier pigeon');
  });
  it.each([
    ['candidate', 'Candidate', 'warning'], ['conflict', 'Conflict', 'warning'],
    ['pending', 'Pending', 'warning'], ['accepted', 'Accepted', 'success'],
    ['rejected', 'Rejected', 'muted'], ['superseded', 'Superseded', 'neutral'],
  ])('review state %s', (code, label, tone) => {
    expect(reviewStateChip(code)).toEqual({ label, tone });
  });
  it('formats dates for display', () => {
    expect(formatDay('2024-01-05')).toBe('Jan 5, 2024');
    expect(formatContactDate('not a date')).toBe('not a date');
    expect(formatContactDate('2002-01-02T12:00:00Z')).toMatch(/2002/);
  });
});
```

- [ ] **Step 2: Run** `bunx vitest run src/lib/directory/dates.test.ts src/lib/directory/labels.test.ts`.
  Expected: FAIL, modules not found.

- [ ] **Step 3: Implement**

In `explore/labels.ts` change `function sentenceCase` to `export function sentenceCase`.

`directory/dates.ts`:

```ts
// Mirrors PartialDate.Validate (internal/store/partialdate.go): year 1–9999,
// month 1–12, and a day that exists in that month. A day without a year is
// checked against leap year 2000, as the store does.
const YEAR_FORMS = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$|^(\d{4})(\d{2})(\d{2})$/;
const PROFILE_FORMS = /^(\d{4})(?:-(\d{2})(?:-(\d{2}))?)?$|^--(\d{2})(?:-(\d{2}))?$|^---(\d{2})$/;
const INTERVAL_MESSAGE = 'Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.';
const RANGE_MESSAGE = 'This date does not exist. Check the month and day.';

function validParts(year: number | undefined, month: number | undefined, day: number | undefined): boolean {
  if (year !== undefined && (year < 1 || year > 9999)) return false;
  if (month !== undefined && (month < 1 || month > 12)) return false;
  if (day !== undefined && (day < 1 || day > 31)) return false;
  if (month === undefined || day === undefined) return true;
  const probeYear = year ?? 2000;
  const probe = new Date(Date.UTC(probeYear, month - 1, day));
  return probe.getUTCFullYear() === probeYear && probe.getUTCMonth() === month - 1 && probe.getUTCDate() === day;
}

const num = (value: string | undefined): number | undefined => (value === undefined ? undefined : Number(value));

export function isCalendarDate(value: string): boolean {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(value);
  return Boolean(match) && validParts(num(match![1]), num(match![2]), num(match![3]));
}

export function intervalDateError(value: string): string | null {
  const trimmed = value.trim();
  if (!trimmed) return null;
  const match = YEAR_FORMS.exec(trimmed);
  if (!match) return INTERVAL_MESSAGE;
  const [year, month, day] = match[1] ? [match[1], match[2], match[3]] : [match[4], match[5], match[6]];
  return validParts(num(year), num(month), num(day)) ? null : INTERVAL_MESSAGE;
}

function profileParts(value: string): [number | undefined, number | undefined, number | undefined] | undefined {
  const match = PROFILE_FORMS.exec(value);
  if (!match) return undefined;
  if (match[1]) return [num(match[1]), num(match[2]), num(match[3])];
  if (match[4]) return [undefined, num(match[4]), num(match[5])];
  return [undefined, undefined, num(match[6])];
}

export function profileDateError(value: string): string | null {
  const parts = profileParts(value.trim());
  if (!parts) return null;
  return validParts(...parts) ? null : RANGE_MESSAGE;
}

export function isTextProfileDate(value: string): boolean {
  const trimmed = value.trim();
  return trimmed !== '' && profileParts(trimmed) === undefined;
}
```

`directory/labels.ts`:

```ts
import type { ChipTone } from '@kenn-io/kit-ui';
import { sentenceCase } from '../explore/labels';

export const PRIMARY_CHANNELS = ['email', 'phone', 'chat'] as const;
const CONTACT_STATES: Record<string, string> = { active: 'Active', inactive: 'Inactive' };
const CHANNELS: Record<string, string> = { email: 'Email', phone: 'Phone', chat: 'Chat' };
// Shared status vocabulary: amber needs attention, green finished, gray off.
const REVIEW_STATES: Record<string, { label: string; tone: ChipTone }> = {
  candidate: { label: 'Candidate', tone: 'warning' },
  conflict: { label: 'Conflict', tone: 'warning' },
  pending: { label: 'Pending', tone: 'warning' },
  accepted: { label: 'Accepted', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'muted' },
};
const DATE_FORMAT: Intl.DateTimeFormatOptions = { year: 'numeric', month: 'short', day: 'numeric' };

export const contactStateLabel = (code: string): string => CONTACT_STATES[code] ?? sentenceCase(code);
export const channelLabel = (code: string): string => CHANNELS[code] ?? sentenceCase(code);
export const reviewStateChip = (code: string): { label: string; tone: ChipTone } =>
  REVIEW_STATES[code] ?? { label: sentenceCase(code), tone: 'neutral' };

export function formatContactDate(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.valueOf()) ? iso : date.toLocaleDateString('en-US', DATE_FORMAT);
}

export function formatDay(day: string): string {
  return new Date(`${day}T00:00:00Z`).toLocaleDateString('en-US', { ...DATE_FORMAT, timeZone: 'UTC' });
}
```

- [ ] **Step 4: Run the tests again.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): add People labels and partial-date rules`.

---

### Task 2: Directory toolbar, filters panel, chips, and readable rows

**Files:**
- Modify: `web/src/lib/explore/state.svelte.ts:463-464`
- Modify: `web/src/lib/components/directory/DirectoryWorkspace.svelte`
- Modify: `web/src/lib/components/directory/DirectoryList.svelte:93-94`
- Test: `web/src/lib/explore/state.test.ts`,
  `web/src/lib/components/directory/DirectoryWorkspace.test.ts`,
  `web/src/lib/components/directory/DirectoryList.test.ts` (create if absent;
  otherwise extend)

**Interfaces:**
- Consumes: `isCalendarDate`, `contactStateLabel`, `channelLabel`,
  `PRIMARY_CHANNELS`, `formatContactDate`, `formatDay` (Task 1).
- Produces: no new exports. `.filters` is renamed `.directory-toolbar`; the
  Playwright spec that used `.filters` is fixed in Task 8.

Target markup and behavior (spec "Directory list"):

- Toolbar row: `SearchInput` (unchanged), a `Button` "Filters" with
  `ariaExpanded`, the Sort `SelectDropdown`, and a count `<span>` at the right
  edge with `aria-live="polite"`.
- Filters button: `surface={filtersOpen || activeFilterCount > 0 ? 'soft' : 'outline'}`,
  matching `ContextBar.svelte`.
- Filter panel (`{#if filtersOpen}`, `<div class="filter-panel" role="group" aria-label="Directory filters">`):
  the Contact state select, Category and Organization text fields, the Primary
  channel select with `PRIMARY_CHANNELS.map((value) => ({ value, label: channelLabel(value) }))`,
  and two date fields.
- Date field markup (one per boundary):

```svelte
<label class="date-field">
  Last contacted after
  <!-- kit-ui-check-ignore: a one-sided boundary needs a single optional date; kit DateRangePicker only commits complete ranges (PR 3 spec, Directory list). -->
  <input type="date" aria-label="Last contacted after" value={textFilters.directoryLastContactAfter}
    onchange={(event) => selectFilter({ directoryLastContactAfter: event.currentTarget.value })} />
  {#if textFilters.directoryLastContactAfter}
    <IconButton size="sm" ariaLabel="Clear last contacted after"
      onclick={() => selectFilter({ directoryLastContactAfter: '' })}><XIcon size="12" aria-hidden="true" /></IconButton>
  {/if}
</label>
```

  Dates commit through `selectFilter` (no debounce): a native date input
  yields a complete value or empty. Remove both keys from `TextFilterKey` and
  `controllerTextFilters()`; read the values straight from `controller` instead.
  Keep the visible label text above each input as shown.
- Sort: keep `title="Directory order"`; add
  `triggerLabel: \`Sort: ${label}\`` to each `sortOptions` entry, as
  `ContextBar.svelte` does.
- Chips (`<div class="filter-chips">`), one per active filter, in panel order:
  "Contact state: Active", "Category: <text>", "Organization: <text>",
  "Primary channel: Email", "Last contacted after Jan 5, 2024",
  "Last contacted before Jan 5, 2024". Each chip has an `IconButton` named
  "Remove <chip text> filter" that commits only that key as empty through
  `selectFilter`. Reuse the `.chip` styling from `ContextBar.svelte`
  (copy the three rules; do not import its private CSS).
- Count: `${controller.rows.length.toLocaleString()}${controller.cursor !== null ? '+' : ''} ${controller.rows.length === 1 ? 'person' : 'people'}`;
  empty string while `controller.loading && controller.rows.length === 0`.
- Promote button: `tone="info" surface="solid"`.
- `DirectoryList.svelte`: line 93 becomes
  `{person.primary_channel ? channelLabel(person.primary_channel) : 'No primary channel'} · {contactStateLabel(person.contact_state)}`;
  line 94 becomes `Last contact ${formatContactDate(person.last_contact_at)}`.
- `state.svelte.ts:463-464`: keep a string only when it is empty or
  `isCalendarDate(value)`; otherwise `''`.

- [ ] **Step 1: Write the failing tests**

`state.test.ts`:

```ts
it('drops an impossible Directory date when reading the URL', () => {
  window.history.replaceState(null, '', `/?explore=${encodeURIComponent(JSON.stringify({
    workspace: 'directory', directoryLastContactAfter: '2026-02-31', directoryLastContactBefore: '2026-03-01'
  }))}`);
  const state = new ExploreState(window);
  try {
    expect(state.current.directoryLastContactAfter).toBe('');
    expect(state.current.directoryLastContactBefore).toBe('2026-03-01');
  } finally {
    state.destroy();
  }
});
```

`DirectoryWorkspace.test.ts` (adapt the existing render helper and fetch stub
in that file; replace the assertions at :119-131 that typed into the old text
date fields):

```ts
it('sets and clears each last-contacted boundary on its own', async () => {
  const { controller } = renderDirectory(); // existing helper; rename if the file uses another
  await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
  const after = screen.getByLabelText('Last contacted after') as HTMLInputElement;
  expect(after.type).toBe('date');
  await fireEvent.change(after, { target: { value: '2024-01-05' } });
  expect(controller.lastContactAfter).toBe('2024-01-05');
  expect(controller.lastContactBefore).toBe('');
  expect(screen.getByText('Last contacted after Jan 5, 2024')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Last contacted after Jan 5, 2024 filter' }));
  expect(controller.lastContactAfter).toBe('');
});

it('removes only the chip that was cleared', async () => {
  const { controller } = renderDirectory({ directoryContactState: 'active', directoryOrganization: 'Example Co' });
  await fireEvent.click(screen.getByRole('button', { name: 'Remove Contact state: Active filter' }));
  expect(controller.contactState).toBe('');
  expect(controller.organization).toBe('Example Co');
});

it('names primary channels and sorts with a visible label', async () => {
  renderDirectory();
  expect(screen.getByRole('combobox', { name: /^Directory order:/ }).textContent).toContain('Sort: Name');
  await fireEvent.click(screen.getByRole('button', { name: 'Filters' }));
  await fireEvent.click(screen.getByRole('combobox', { name: /^Primary channel/ }));
  expect(screen.getByRole('option', { name: 'Email' })).toBeTruthy();
});
```

Also assert the request: after setting the after-boundary, the latest
`/api/v1/people/directory` request carries
`last_contact_after=2024-01-05T00:00:00Z` and no `last_contact_before`.
Existing tests that read `textbox "Category filter"` / `"Organization filter"`
/ `combobox "Primary channel"` now click "Filters" first.

`DirectoryList` test: a row with `primary_channel: 'email'`,
`contact_state: 'active'`, `last_contact_at: '2002-01-02T12:00:00Z'` shows
"Email · Active" and a "Last contact" line containing "2002", and no `T12:00`.

- [ ] **Step 2: Run** `bunx vitest run src/lib/explore/state.test.ts src/lib/components/directory`.
  Expected: the new tests FAIL.
- [ ] **Step 3: Implement** the markup and behavior above.
- [ ] **Step 4: Run the tests, `bun run check`, and `bun run check:kit-ui`.** Expected: PASS, no findings.
- [ ] **Step 5: Commit.** Subject: `feat(web): give Directory one toolbar with filter chips`.

---

### Task 3: Person header, sections, and record actions

**Files:**
- Create: `web/src/lib/components/directory/PersonRecordActions.svelte`, `PersonRecordActions.test.ts`
- Modify: `web/src/lib/components/directory/StructuredProfileSection.svelte`
  (remove `historyOpen`, `renaming`, `renameValue`, `confirmingDelete`,
  `beginRename`, `saveRename`, `deletePerson`, the header buttons at
  :236-257, the rename and delete groups at :261-310, and the
  `ProfileHistoryDialog` block at :434-441; keep `reload()` but drop its
  `confirmingDelete = false` line)
- Modify: `web/src/lib/components/directory/PersonDetail.svelte`
- Modify: `web/src/lib/components/directory/PersonAgenda.svelte:209-243`
- Modify: `web/src/lib/components/directory/DirectoryWorkspace.svelte` (new props, pass through)
- Modify: `web/src/lib/components/shell/AppShell.svelte` (DirectoryWorkspace mount at ~1379)
- Test: `PersonDetail.test.ts`, `StructuredProfileSection.test.ts`,
  `PersonAgenda.test.ts`, `AppShell.test.ts`

**Interfaces:**
- `PersonRecordActions` props: `{ client: APIClient; controller: DirectoryProfileController; personID: number }`.
  Renders a kit `Menu` (`align="end"`) with `MenuTrigger ariaLabel="More actions"`
  (`Ellipsis` icon) and `MenuItem`s "Rename person" (disabled when
  `!controller.canWritePerson`), "View profile history", and "Delete person"
  (disabled when `!controller.canWritePerson`). It also renders the moved
  rename group, delete group, and `ProfileHistoryDialog` exactly as
  `StructuredProfileSection` did, with the same names, labels, pending text,
  and disabled rules. The groups render below the header row (the component
  returns a fragment: the menu, then the groups).
- New `PersonDetail` props: `onOpenRelationship?: (participantID: number) => void`
  and `onReviewFacts?: (personID: number) => void`. `DirectoryWorkspace` gets
  the same two props and passes them to both `PersonDetail` mounts.
- AppShell passes `onOpenRelationship={openRelationship}` and
  `onReviewFacts={(personID) => commitNavigation({ workspace: 'directory_review', reviewKind: 'fact', directoryPersonID: personID })}`.

`PersonDetail` changes:

- Replace the five tab buttons, five `bind:this` refs, and ten id constants
  with one list:

```ts
type DetailTab = 'overview' | 'profile' | 'organizations' | 'connections' | 'network' | 'media' | 'maintenance';
const SECTIONS: ReadonlyArray<{ id: DetailTab; label: string }> = [
  { id: 'overview', label: 'Overview' },
  { id: 'profile', label: 'Profile' },
  { id: 'organizations', label: 'Organizations' },
  { id: 'connections', label: 'Connections' },
  { id: 'network', label: 'Network' },
  { id: 'media', label: 'Media & files' },
  { id: 'maintenance', label: 'Maintenance' },
];
const tabButtons: Partial<Record<DetailTab, HTMLButtonElement>> = $state({});
const tabID = (tab: DetailTab) => `person-${personID}-${tab}-tab`;
const panelID = (tab: DetailTab) => `person-${personID}-${tab}-panel`;
```

  Render with `{#each SECTIONS as section (section.id)}` and
  `bind:this={tabButtons[section.id]}`. `handleTabKeydown` uses
  `SECTIONS.map((s) => s.id)` for order and `'maintenance'` for End.
  `selectTab` focuses `tabButtons[tab]`.
- Header above the tablist:

```svelte
<header class="person-header">
  <h2>{displayName}</h2>
  <div class="person-actions">
    {#if participantID !== undefined && onOpenRelationship}
      <Button label="Open relationship" surface="outline" size="sm" onclick={() => onOpenRelationship(participantID)} />
    {/if}
    {#if onReviewFacts}
      <Button label="Review facts" surface="outline" size="sm" onclick={() => onReviewFacts(personID)} />
    {/if}
    {#if profileController}<PersonRecordActions {client} controller={profileController} {personID} />{/if}
  </div>
</header>
```

  with `displayName = bundle.person?.display_name ?? profile?.person?.display_name ?? \`Person ${personID}\``
  and `participantID = bundle.person?.participant_ids.length ? Math.min(...bundle.person.participant_ids) : undefined`.
  The `h2` leaves the Overview panel.
- Panels, in the spec's order:
  - Overview: `PersonBriefCard`, `PersonAgenda`, `AttributeSummary`, Contact
    state, Activity, the organization and relationship summaries (the
    "Relationships" summary heading becomes "Connections"), `MeetingPanel`.
    Contact state shows
    `{contactStateLabel(cadence_status)} · N interactions · last contact {formatContactDate(...)}`.
  - Profile: `StructuredProfileSection` or the no-controller fallback
    sections, then `AttributeSection`.
  - Organizations, Connections (`RelationshipsTab`), Network, Media & files:
    unchanged content.
  - Maintenance: `PersonTrackingControl`, `CardDAVPublicationControl`,
    `PersonMergeHistory`.
- `AttributeSummary` `onEdit`: `await selectTab('profile'); await tick();`
  then scroll and focus `#person-attributes` as today.
- Tablist CSS: `overflow-x: auto; flex-wrap: nowrap;` and
  `[role="tab"] { white-space: nowrap; flex: none; }`.

`PersonAgenda`: wrap the `<form>` at :239-243 in `{#if mutationReady}`; give
the integration status paragraph `role="status"` instead of `role="alert"`.

- [ ] **Step 1: Write the failing tests**

`PersonDetail.test.ts` (replace the five-tab assertions at :258-284 and :473):

```ts
it('lists seven person sections in order', async () => {
  renderPersonDetail();
  expect(screen.getAllByRole('tab').map((tab) => tab.textContent?.trim())).toEqual(
    ['Overview', 'Profile', 'Organizations', 'Connections', 'Network', 'Media & files', 'Maintenance']);
  await fireEvent.click(screen.getByRole('tab', { name: 'Maintenance' }));
  expect(screen.getByRole('heading', { name: 'Merge history' })).toBeTruthy();
});

it('opens the relationship for the lowest participant ID and reviews facts', async () => {
  const onOpenRelationship = vi.fn();
  const onReviewFacts = vi.fn();
  renderPersonDetail({ person: { ...person, participant_ids: [42, 7, 19] } }, { onOpenRelationship, onReviewFacts });
  await fireEvent.click(screen.getByRole('button', { name: 'Open relationship' }));
  expect(onOpenRelationship).toHaveBeenCalledWith(7);
  await fireEvent.click(screen.getByRole('button', { name: 'Review facts' }));
  expect(onReviewFacts).toHaveBeenCalledWith(personID);
});

it('hides Open relationship when the person has no participants', () => {
  renderPersonDetail({ person: { ...person, participant_ids: [] } }, { onOpenRelationship: vi.fn() });
  expect(screen.queryByRole('button', { name: 'Open relationship' })).toBeNull();
});
```

Use the file's existing bundle fixture and render helper names. The existing
"Contact state before Last time we talked" assertion (:467-469) is reversed:
the brief now comes first.

`PersonRecordActions.test.ts`: move the rename, delete, and history tests
from `StructuredProfileSection.test.ts:256-307, 377`, opening each flow with
`fireEvent.click(screen.getByRole('button', { name: 'More actions' }))` then
the `menuitem` of the same name. Keep every assertion on group names,
buttons, and controller calls. Add: both write items are disabled when
`canWritePerson` is false.

`PersonAgenda.test.ts`: with integration state `disabled`, the "New agenda
item" field is absent and the status line is present.

`AppShell.test.ts`: from a Directory person, "Review facts" leaves
`state.current` with `workspace: 'directory_review'`, `reviewKind: 'fact'`,
and the person ID; "Open relationship" sets `relationshipTarget` to
`cluster:<lowest id>`.

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/directory src/lib/components/shell/AppShell.test.ts`.
  Expected: new tests FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): split the person page into sections with a header`.

---

### Task 4: Partial-date validation in the person editors

**Files:**
- Modify: `web/src/lib/components/directory/EmploymentEditor.svelte:182,189-190,207`
- Modify: `web/src/lib/components/directory/PersonRelationshipEditor.svelte:294-311,335`
- Modify: `web/src/lib/components/directory/StructuredProfileEditor.svelte:436-445` and its save button
- Test: `OrganizationEmploymentTab.test.ts` or `EmploymentEditor.test.ts`,
  `RelationshipsTab.test.ts` or `PersonRelationshipEditor.test.ts`,
  `StructuredProfileEditor.test.ts` (use whichever file already renders the
  editor)

**Interfaces:**
- Consumes: `intervalDateError`, `profileDateError`, `isTextProfileDate` (Task 1).

Pattern for each interval field (employment start and end, relationship start
and end):

```svelte
<script lang="ts">
  const startDateError = $derived(intervalDateError(startDate));
</script>
<label>Start date<TextInput ariaLabel="Employment start date" bind:value={startDate}
  placeholder="YYYY, YYYY-MM, or YYYY-MM-DD" block disabled={submitting}
  ariaDescribedby={startDateError ? 'employment-start-date-error' : undefined} /></label>
{#if startDateError}<p id="employment-start-date-error" class="field-error">{startDateError}</p>{/if}
```

Kit `TextInput` takes `ariaDescribedby`. Make each error id unique per editor
instance with `$props.id()` (for example `${uid}-start-date-error`).

- Add `|| Boolean(startDateError) || Boolean(endDateError)` to each editor's
  submit `disabled` expression.
- Profile "Date" field: `dateError = $derived(profileDateError(dateValue))`;
  show the error the same way; show `<p class="field-hint">Saved as text</p>`
  when `isTextProfileDate(dateValue)`. Disable the profile editor's save
  button while `section === 'dates' && dateError`.
- `.field-error { color: var(--text-danger); font-size: var(--font-size-xs); margin: 0; }`
  and `.field-hint { color: var(--text-muted); font-size: var(--font-size-xs); margin: 0; }`.

- [ ] **Step 1: Write the failing tests**

```ts
it('rejects a year-less employment date and keeps Save disabled', async () => {
  renderEmploymentEditor(); // existing helper
  await fireEvent.input(screen.getByRole('textbox', { name: 'Employment start date' }), { target: { value: '--04-12' } });
  expect(screen.getByText('Use a year, year and month, or full date, like 2019, 2019-04, or 2019-04-12.')).toBeTruthy();
  expect((screen.getByRole('button', { name: /Create employment|Save employment/ }) as HTMLButtonElement).disabled).toBe(true);
  await fireEvent.input(screen.getByRole('textbox', { name: 'Employment start date' }), { target: { value: '20190412' } });
  expect(screen.queryByText(/Use a year/)).toBeNull();
});
```

Same shape for "Relationship start date" (`2019-02-30` rejected, `2019`
accepted). Profile:

```ts
it.each(['--04-12', '--04', '---12'])('keeps the profile date form %s', async (value) => {
  renderDateEditor();
  await fireEvent.input(screen.getByRole('textbox', { name: 'Date' }), { target: { value } });
  expect(screen.queryByText(/does not exist/)).toBeNull();
  expect(screen.queryByText('Saved as text')).toBeNull();
});
it('rejects an impossible profile date and marks free text', async () => {
  renderDateEditor();
  const field = screen.getByRole('textbox', { name: 'Date' });
  await fireEvent.input(field, { target: { value: '--02-30' } });
  expect(screen.getByText('This date does not exist. Check the month and day.')).toBeTruthy();
  await fireEvent.input(field, { target: { value: 'spring 2019' } });
  expect(screen.getByText('Saved as text')).toBeTruthy();
});
```

Also assert a text date still submits `date_text: 'spring 2019'` with no
`date` field, so the existing text-date capability is covered.

- [ ] **Step 2: Run** the three test files. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): validate partial dates in person editors`.

---

### Task 5: Relationships header and labels

**Files:**
- Modify: `web/src/lib/components/relationships/RelationshipList.svelte:168`
- Modify: `web/src/lib/components/relationships/RelationshipsWorkspace.svelte:494-506`
- Modify: `web/src/lib/components/relationships/RelationshipHeader.svelte:302-360` and styles near :515-545
- Test: `RelationshipList.test.ts`, `RelationshipsWorkspace.test.ts`, `RelationshipHeader.test.ts`

Changes:

- Placeholder: `"Filter people and domains"`.
- Drawer toggle: `label="People"`, `ariaLabel` removed (the name becomes
  "People"); drawer `title="People"`; drawer `ariaLabel` unchanged.
- Header: the title row keeps avatar, `h2`, and `.actions` with only
  "Open in Directory" and "Same person…" (wrap them in
  `<div class="actions" role="group" aria-label="Person actions">`; render the
  group only for people). Move the `SegmentedControl ariaLabel="Relationship view"`
  out of `.actions` into its own `<div class="view-row">` after the title row
  and any stale banner, left-aligned. Options and handler unchanged.
- Counts line: drop the files segment and `data-mono`:
  `{activity_count} items · {formatDate(first_at)} – {formatDate(last_at)}`,
  plus `· N people` for domains. Change the local `formatDate` to
  `date.toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric' })`.
- Identity chip names already use the display name when known (`:376-378`,
  `:425`); leave them.

- [ ] **Step 1: Write the failing tests**

```ts
// RelationshipList.test.ts
expect(screen.getByRole('searchbox', { name: 'Search people and domains' }).getAttribute('placeholder'))
  .toBe('Filter people and domains');

// RelationshipsWorkspace.test.ts (replace "Show relationship list" at :617, :665)
await fireEvent.click(screen.getByRole('button', { name: 'People' }));
expect(screen.getByRole('dialog', { name: 'Relationship search and results' })).toBeTruthy();

// RelationshipHeader.test.ts
const actions = screen.getByRole('group', { name: 'Person actions' });
expect(within(actions).getByRole('button', { name: 'Open in Directory' })).toBeTruthy();
expect(within(actions).queryByRole('radio', { name: 'Messages' })).toBeNull();
expect(screen.getByText(/items · /).textContent).not.toMatch(/files/);
```

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/relationships`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): separate Relationships actions from its view switch`.

---

### Task 6: Reviews structure, Show selects, and status chips

**Files:**
- Modify: `web/src/lib/components/directory/DirectoryReviewCentre.svelte`
- Modify: `web/src/lib/components/directory/RelationshipReviewQueue.svelte`
- Modify: `web/src/lib/components/directory/FactReviewPanel.svelte:17` (heading text only; Task 7 does the rest)
- Modify: `web/src/lib/components/directory/IdentityCandidateCard.svelte:22`
- Modify: `web/src/lib/components/directory/RelationshipReviewCard.svelte:26,34`
- Test: `DirectoryReviewCentre.test.ts`, `RelationshipReviewQueue.test.ts`,
  `FactReviewPanel.test.ts`, `IdentityCandidateCard.test.ts`,
  `RelationshipReviewCard.test.ts`, `AppShell.test.ts:1246-1251`

**Interfaces:**
- Consumes: `reviewStateChip` (Task 1).
- Produces: CSS class `review-heading` on each review `h2`, and
  `data-review-section` on each review section, used by Task 7's Facts panel.

Changes:

- `reviewKindOptions`: `{ value: 'fact', label: 'Facts' }`.
- Each review `h2` (`identity-review-heading`, `relationship-review-heading`,
  `fact-review-heading`) keeps its id and `tabindex="-1"`, gets
  `class="review-heading"`, and its text becomes the option label
  ("Identity matches", "Imported relationships", "Facts"). Delete the
  description paragraphs under the identity and imported headings. Keep the
  imported read-only sentence as a one-line `<p class="queue-note">` in the
  queue toolbar.
- Hidden heading with visible focus, in each component's styles:

```css
.review-heading {
  position: absolute; width: 1px; height: 1px; margin: -1px; padding: 0;
  overflow: hidden; clip: rect(0 0 0 0); white-space: nowrap; border: 0;
}
[data-review-section] { position: relative; border-radius: var(--radius-md); }
[data-review-section]:has(> .review-heading:focus-visible),
[data-review-section]:has(> * > .review-heading:focus-visible) {
  outline: 2px solid var(--focus-color); outline-offset: 4px;
}
```

  `--focus-color` is the focus token in `src/styles/tokens.css`. Add `data-review-section` to the
  identity `<section>`, the imported `<section>`, and the Facts `<section>`.
- Show selects replace both `SegmentedControl`s:

```svelte
<SelectDropdown
  title="Identity review state"
  value={controller.identityState}
  options={identityStateOptions.map((option) => ({ ...option, triggerLabel: `Show: ${option.label}` }))}
  onchange={selectIdentityState}
  disabled={!!activeDecision}
/>
```

  and the same for `title="Imported relationship review state"` with
  `disabled={controller.loading}`. Place each at the start of its toolbar row.
  Delete the now-unused `.kit-segmented` mobile rules.
- Cards: replace `<p class="state">{candidate.state}</p>` and
  `<p class="state">{review.status}</p>` with
  `<Chip size="sm" tone={chip.tone} uppercase={false}>{chip.label}</Chip>`
  where `chip = reviewStateChip(...)`. Delete the `Status` row from the
  imported card's metadata list so the state shows once.
- The imported empty state uses `reviewStateChip(controller.state).label`
  instead of the local `stateLabel` (delete `stateLabel`).

- [ ] **Step 1: Write the failing tests**

```ts
// DirectoryReviewCentre.test.ts (update :116-118, :153-162, :249-253)
expect(screen.getByRole('radio', { name: 'Facts' })).toBeTruthy();
const show = screen.getByRole('combobox', { name: /^Identity review state/ });
expect(show.textContent).toContain('Show: Candidate');
await fireEvent.click(show);
await fireEvent.click(screen.getByRole('option', { name: 'Conflict' }));
expect(commit).toHaveBeenCalledWith(expect.objectContaining({ identityState: 'conflict' }));

it('shows a focus ring on the section when its hidden heading takes keyboard focus', async () => {
  renderCentre();
  const heading = screen.getByRole('heading', { name: 'Identity matches', level: 2 });
  expect(heading.className).toContain('review-heading');
  expect(heading.closest('[data-review-section]')).not.toBeNull();
});
```

jsdom cannot evaluate `:focus-visible`, so the unit test asserts the hook
(class and section attribute); Task 8 asserts the visible ring in a browser.

```ts
// IdentityCandidateCard.test.ts
expect(screen.getByText('Candidate')).toBeTruthy();
expect(screen.queryByText('candidate')).toBeNull();
// RelationshipReviewCard.test.ts
expect(screen.getAllByText('Pending')).toHaveLength(1);
```

Update `RelationshipReviewQueue.test.ts:105` (radio "Pending" → combobox
`/^Imported relationship review state/` showing "Show: Pending") and
`AppShell.test.ts:1251` (radio "Accepted" → choose the "Accepted" option).

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/directory src/lib/components/shell/AppShell.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): give Reviews one heading and Show filters`.

---

### Task 7: Facts person picker and person name

**Files:**
- Modify: `web/src/lib/components/directory/FactReviewPanel.svelte`
- Modify: `web/src/lib/components/directory/DirectoryReviewCentre.svelte`,
  `DirectoryReviewWorkspace.svelte` (new props, pass through)
- Modify: `web/src/lib/components/shell/AppShell.svelte` (~1400 mount)
- Test: `FactReviewPanel.test.ts`, `AppShell.test.ts`

**Interfaces:**
- New props down the chain `AppShell → DirectoryReviewWorkspace →
  DirectoryReviewCentre → FactReviewPanel`: `client: APIClient` and
  `onSelectFactPerson: (personID: number) => void`. AppShell passes
  `onSelectFactPerson={(personID) => commitNavigation({ directoryPersonID: personID })}`.
- `FactReviewPanel` drops `onOpenDirectory` (and the chain drops it too, with
  AppShell's `onOpenDirectory` prop on this mount).

`FactReviewPanel` behavior:

- Always render a "Person" field above the ledger: kit `Typeahead` with
  `title="Fact person"`, `placeholder="Search Directory people"`,
  `fallbackLabel={personName ?? 'Choose a person'}`, `remote`,
  `loading={searching}`, `loadingLabel="Searching…"`,
  `emptyLabel="No matching people"`, `error={searchError}`,
  `onquery={debouncedSearch}`, and `onselect={(value) => onSelectFactPerson(Number(value))}`.
  Options are `people.map((person) => ({ value: String(person.id), label: person.display_name ?? \`Person ${person.id}\` }))`.
- Search copies `PersonRelationshipEditor.searchPeople` (`:88-121`):
  `generatedListDirectoryPeople({ q, limit: 20 }, { ...client, signal })`, an
  `AbortController` per query, a generation counter, a 250 ms `debounce`
  imported the way that editor imports it, and abort plus `cancel()` in
  `onDestroy`. Do not filter out the current person.
- With no person: the field and the sentence "Choose a person to see the facts
  recorded about them." replace the EmptyState and "Open Directory".
- With a person: the context row shows `<strong>{personName ?? \`Person ${personID}\`}</strong>`
  and "Open person profile". `personName` is set from the chosen option on
  select; when `personID` changes and no name is known, load it with
  `generatedGetPersonProfile({ id: personID }, { ...client, signal })` and use
  `data.display_name`. Ignore a stale response with the same generation
  pattern.
- Keep the two unavailable-feature notices and `FactLedger`.

- [ ] **Step 1: Write the failing tests**

```ts
it('picks a person for Facts from the Directory search', async () => {
  const fetchFn = vi.fn<typeof fetch>(async (input) => {
    const url = new URL(input instanceof Request ? input.url : String(input), 'http://localhost');
    if (url.pathname === '/api/v1/people/directory') return Response.json({
      people: [{ id: 12, display_name: 'Alex Example', categories: [], organizations: [], contact_state: 'active', revision: 1 }],
    });
    return Response.json({});
  });
  const onSelectFactPerson = vi.fn();
  renderFactPanel({ client: createAPIClient(fetchFn), personID: null, onSelectFactPerson });
  expect(screen.queryByRole('button', { name: 'Open Directory' })).toBeNull();
  await fireEvent.input(screen.getByRole('combobox', { name: /Fact person/ }), { target: { value: 'Alex' } });
  await fireEvent.click(await screen.findByRole('option', { name: 'Alex Example' }));
  expect(onSelectFactPerson).toHaveBeenCalledWith(12);
});

it('shows the selected person by name after a reload', async () => {
  const fetchFn = vi.fn<typeof fetch>(async () => Response.json({ id: 12, display_name: 'Alex Example', participant_ids: [] }));
  renderFactPanel({ client: createAPIClient(fetchFn), personID: 12 });
  expect(await screen.findByText('Alex Example')).toBeTruthy();
  expect(screen.queryByText('Person ID 12')).toBeNull();
});
```

Inspect kit `Typeahead`'s rendered roles first (`node_modules/@kenn-io/kit-ui/src/lib/components/Typeahead.svelte`
and its tests) and use the role and name it actually exposes; the
`PersonRelationshipEditor` tests show a working query. Use fake timers or
`waitFor` for the debounce.

`AppShell.test.ts`: choosing a person in Facts sets
`state.current.directoryPersonID` and keeps `reviewKind: 'fact'`.

- [ ] **Step 2: Run** `bunx vitest run src/lib/components/directory/FactReviewPanel.test.ts src/lib/components/shell/AppShell.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run tests and checks.** Expected: PASS.
- [ ] **Step 5: Commit.** Subject: `feat(web): choose a person from Facts`.

---

### Task 8: Browser tests

**Files:**
- Modify: `web/tests/e2e/directory.spec.ts` (`.filters` at :86, tabs at
  :51-57 and :144, merge history region at :81 and maintenance switch at
  :156-207 now under Maintenance), `web/tests/directory-review.spec.ts`
  (:99-117 and :395-440 radios → Show selects; :301-308 "Fact review" →
  "Facts"), `web/tests/e2e/accessibility.spec.ts` (region "Fact review" →
  "Facts" at :248, :297, :315; tab "Network" at :187; maintenance switch at
  :204), `web/tests/directory-network.spec.ts` (:44-58 tab order), and any
  spec Step 1 finds

- [ ] **Step 1: Find affected specs**

```bash
rg -ln "Fact review|Identity review state|Imported relationship review state|Show relationship list|Media & Files|'Relationships' \}|\.filters|Last contacted (after|before)|Profile maintenance|Person merge history|Rename person|Delete person|View profile history" tests
```

- [ ] **Step 2: Update each spec** to the new names and flows without
  weakening assertions. Add browser coverage for:
  - Directory: set only "Last contacted after" with the native date field; the
    chip appears; reload keeps it; removing the chip clears it.
  - Person page: "Review facts" lands on Facts with the person's name; "Open
    relationship" lands on the person's relationship.
  - Reviews keyboard focus: change the imported Show select with the keyboard
    so focus moves to the hidden heading, then assert the section's computed
    `outline-style` is not `none`.
  - Phone width (420px): the person tablist does not wrap (every tab's
    `offsetTop` is equal).
  - axe on Directory with the Filters panel open, a person page on each
    section, and Facts with the picker, in both themes.
- [ ] **Step 3: Run** `make web-test-browser` from the repository root.
  Expected: all pass.
- [ ] **Step 4: Commit.** Subject: `test(web): cover the People workspaces in the browser`.
  List every existing spec edit and why in the body.

---

### Task 9: Verify and open the pull request

- [ ] **Step 1:** From the repository root run
  `make web-check && make web-test && make web-test-browser && make lint-ci`.
- [ ] **Step 2:** Rebuild the fixture daemon and capture before and after
  screenshots at 1440×900 and 420×860: Relationships with a person,
  Directory with the Filters panel open and a chip, a person on Overview,
  Profile, and Maintenance, and Reviews on each type including Facts with the
  picker. View every image. Check: no purple button, no raw codes in rows or
  chips, tabs on one line at 420px, one heading per review type.
- [ ] **Step 3:** Read `git diff origin/main...HEAD` fully; remove leftovers.
- [ ] **Step 4:** The user has authorized the pull request. Push the branch and
  open it with the `kenn-io-commit-push-pr` and `kenn-io-pr-desc` rules,
  attaching the cleared screenshots.

## Self-review notes

- Spec coverage: Relationships (Task 5); Directory toolbar, dates, chips,
  count, rows, Promote (Task 2); person header, sections, menu, agenda,
  narrow tablist, attributes Edit (Task 3); partial dates for intervals and
  profiles (Task 4); Reviews label, headings, focus ring, Show selects, chips
  (Task 6); Facts picker and name (Task 7); tests and screenshots (Tasks 8, 9).
- The spec's identity-chip naming item is already true in the code
  (`RelationshipHeader.svelte:376-378, 425` use the display name when known),
  so Task 5 leaves it.
- Task 2 renames `.filters`; Task 8 fixes the one Playwright locator that used
  it. Tasks 3 and 6 both touch `AppShell.svelte` mounts but different blocks.
