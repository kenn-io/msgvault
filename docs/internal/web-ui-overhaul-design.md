# Web UI overhaul

Status: design approved 2026-09-28; not yet implemented. Delivery is four
stacked pull requests, described under [Delivery](#delivery). This record
describes the intended end state; the current source remains authoritative
until each pull request lands.

## Summary

The Web UI exposes a large, capable archive, but its screens grew one feature
at a time. Ten peer tabs share one centered top bar, each workspace invents its
own header and toolbar, several controls do nothing, and many labels are raw
API codes. People who use msgvault every day have to learn each screen
separately.

This overhaul gives every workspace the same shell, page structure, visual
language, and status vocabulary. It keeps every existing capability. Controls
move to predictable places; none are removed. The visual language follows the
docbank web restyle (kenn-io/docbank#722) so the kenn tools feel like one
family.

## Problems observed

Observed on `main` at 5d1ce392 with the Enron docs fixture at 1440×900 and
420×860, light and dark.

- **Navigation has no hierarchy.** Relationships, Directory, Reviews,
  Everything, Files, Saved Views, Sources, Operations, Deletions, and Settings
  are equal-weight tabs. On a phone they collapse into a dropdown beside the
  density selector.
- **Each page invents its header.** Sources, Deletions, and Saved Views show an
  orange "ARCHIVE WORKSPACE" eyebrow; Operations shows "ARCHIVE OPERATIONS";
  Everything and Directory show none. Saved Views uses a centered column while
  other pages fill the width. In Files the page title sits below its toolbar.
- **Everything stacks four control strips.** Search, the context bar, an
  always-visible "No items selected" bar, and a Columns strip sit above the
  results, with a hard-coded keyboard-hint footer below. The result count
  appears twice ("20 items" and "20 results").
- **Files is two different views.** Everything's "Show as: Files" renders
  `explore/FilesPresentation.svelte` (grid "Files in current context"). The
  Files workspace renders `FilesWorkspace` (grid "Files results") with other
  columns and filters. In the Files workspace, "Show as: Table" silently moves
  to Everything.
- **Directory shows seven inline filters.** The date filters are free-text
  `YYYY-MM-DD` fields whose placeholder is truncated; an invalid date is
  silently ignored.
- **Accent colors compete.** Search is blue, Save is purple, Save settings is
  green, and eyebrows are orange. Timestamps and counts use monospace as
  decoration.
- **Status is alarming or raw.** Operations shows a red dot for every feature
  that is simply not configured. Sources shows `source_not_schedulable` in red
  as its "Action". Deletions shows "No deletion manifests yet" in an orange
  warning box. Raw codes also appear in Files (MIME families), Operations
  (error codes), Deletions (reasons, ISO timestamps), Tasks, Meetings, and the
  context-bar crumbs (`full_text`).
- **Some controls do nothing.** "Newest first" and the `s`/`r` shortcuts only
  announce that sort is fixed; in Files the button has no handler.
  "Open selection in source" never receives a handler. Directory → Media &
  Files mounts `FilesWorkspace` without change handlers, so its filename
  filter, type checkboxes, and sort headers are inert. Saved Views declares an
  unused `selection` prop. `search/SearchBar.svelte` is imported only by its
  test.
- **Settings contradicts itself.** Appearance says "Changes apply right away"
  above a Save settings bar. The phrase means "no daemon restart after saving".
  The shell reads `web.theme` and `web.density` only at sign-in, so saving them
  may not update the open tab.
- **Useful actions are hidden.** Staging a deletion is reachable only with `d`
  or `D` in Everything. Tasks are a collapsed disclosure in the reading pane.
  "Save this view" lives on the Saved Views page, away from the view it saves.
- **Cross-links are missing.** Directory cannot open a person's Relationships
  view. Nothing links to Reviews → Facts for a person, and Facts with no person
  selected is a dead end.
- **The browser tab title is always "Everything · msgvault".**

## Goals

- One navigation model, one page structure, and one toolbar pattern across
  every workspace.
- One accent color. Status colors mean status only.
- Human-readable labels for every code the UI displays, with the raw code kept
  in a tooltip or detail view for diagnosis.
- Every existing capability reachable, with a visible entry point for actions
  that are keyboard-only today.
- Existing URLs, URL state keys, API calls, and accessible names preserved
  unless this document names the change.
- Light and dark themes, both densities, keyboard use, and phone widths work on
  every screen.

## Non-goals

- New archive capabilities. The follow-ups listed below need backend work and
  ship separately.
- Changes to the TUI, CLI, API, or MCP server.
- Replacing kit-ui components with local ones. Where kit-ui lacks a token,
  `app.css` overrides the kit class, as docbank does.

## Visual language

These rules match docbank#722.

- **Palette.** A new `web/palette.css` retunes kit-ui tokens: neutral surfaces,
  one blue accent, and green, amber, and red for status only. It lives outside
  `src/` because `kit-ui-check` guards component styles, not palette files.
  `src/styles/tokens.css` keeps the semantic aliases it defines today.
- **Buttons.** One solid primary action per screen, in blue. Purple and green
  are no longer button colors. Destructive confirmation buttons are red; the
  button that opens a destructive review is not.
- **Type.** Platform system fonts. Sentence case for every label, heading,
  table header, and chip ("Saved views", not "Saved Views"; no uppercase
  letter-spaced section labels). `[data-section-label]` becomes sentence case.
- **Monospace** is for identifiers, hashes, code, keys, and cron text only.
  `[data-mono]` becomes tabular-figure sans, so timestamps and counts stop
  switching typeface. Identifiers that use `[data-mono]` today move to
  `<code>` or `[data-metadata]`, which stay monospace.
- **Status vocabulary.** Every status dot and chip uses one mapping:

  | Meaning | Tone | Examples |
  |---|---|---|
  | Healthy or finished | green | Succeeded, Completed, Ready |
  | In progress | blue | Running, Syncing, Queued |
  | Needs attention | amber | Partial, Stale, Conflict |
  | Failed | red | Failed |
  | Off or not set up | gray | Not configured, Disabled, Cancelled |

- **Empty states** use kit `EmptyState` in a neutral tone and name the next
  step. A missing item is never a warning.
- **Code labels.** Each enum the UI displays gets one label map next to the
  component that renders it. Unknown codes fall back to a sentence-cased form of
  the code. The raw code stays available in a tooltip or detail panel.

## Information architecture

### Sidebar

A labeled left sidebar replaces the top-bar tabs. It is a `nav` still named
"Primary", and the active item keeps `aria-current="page"`, which history focus
restoration depends on.

| Group | Items (icon) | Workspace id |
|---|---|---|
| People | Relationships (users), Directory (contact), Reviews (check-check) | `relationships`, `directory`, `directory_review` |
| Archive | Everything (inbox), Files (paperclip), Saved views (bookmark) | `everything`, `files`, `saved_views` |
| Manage | Sources (plug), Operations (activity), Deletions (trash-2), Settings (settings) | `sources`, `operations`, `deletions`, `settings` |

People comes first because Relationships is the default landing workspace.
Icons come from `@lucide/svelte`, which is already a dependency; final icon
choices may change during implementation.

- **Layout.** kit `CollapsibleSidebar` provides the layout, narrow-screen
  overlay, and resize behavior. Expanded, the sidebar is about 232px with group
  headings. Collapsed, it is an icon rail; each item shows a tooltip and keeps
  its full accessible name. The collapsed state is stored per browser in
  localStorage `msgvault.sidebar.collapsed`.
- **Narrow screens.** Below 900px the sidebar becomes a slide-out menu opened by
  a menu button in the top bar, with a scrim that closes it.
- **Footer.** The archive status indicator (dot plus "Local archive",
  "Searching", or "Attention") and a "Keyboard shortcuts" entry showing `?`
  move to the sidebar footer.
- **Tab title.** The browser tab title follows the workspace, for example
  "Directory · msgvault".

### Top bar

The top bar holds only global search and appearance controls.

- **Global search.** The search form moves from Everything into the top bar and
  reuses `search/SearchBar.svelte` (today imported only by its test). It keeps
  the names form "Search Everything", searchbox "Search everything", radiogroup
  "Search mode" with "Full text", "Semantic", and "Hybrid", and button
  "Search". Query and mode state are unchanged (`query` in the `explore` URL
  state, top-level `mode`, localStorage `msgvault-search-mode`).
  - On Everything and Files, typing updates the current view as it does today.
  - On any other workspace, typing is a local draft; Enter or Search commits the
    query and opens Everything.
  - `/` focuses the global search from any workspace. AppShell's `searchInput`
    reference binds to this input, so "Refine search" and the saved-view focus
    fallback keep working.
  - Below 900px the mode control becomes a compact select inside the search
    field; the radio semantics are kept at wider widths.
- **Theme.** kit `ThemeToggle` stays, matching the other kenn tools.
- **Display menu.** A new "Display" menu button holds the per-tab density
  override (Auto, Compact, Comfortable) and "Use daemon theme". Both keep their
  sessionStorage behavior (`msgvault.appearance.override`). The combobox
  "Temporary density" becomes a radio group named "Temporary density" inside
  this menu.

### Page structure

A shared local `PageHeader` component renders the title (h1), an optional
one-line description, right-aligned actions, and an optional row below for view
tabs. Every workspace uses it. Eyebrows are removed. Horizontal padding is the
same on every page. Data workspaces fill the width; form pages (Settings,
Saved views editing) use the same left edge with a maximum content width.

- **View switches** (different views of the same data, such as Messages or
  Files, and review type) use kit `SegmentedControl`.
- **Record sections** (sections of one record, such as person detail) use the
  ARIA tablist pattern that `PersonDetail` already implements.

### Toolbars

Data workspaces use one toolbar row: search or filter input first, then
Filters, view options, and sort, with the result count at the right edge. When
filters, groupings, or a query are active, a second line shows them as
removable chips with readable labels. Notices (semantic coverage, "More results
may match") appear between the toolbar and the results only when they apply.

### Keyboard

The hard-coded footer in Everything and the key badges in the selection bar are
removed. The existing "Keyboard shortcuts" dialog, generated from
`lib/commands/registry.ts`, becomes the single reference. Before removing the
footer, the registry gains the entries only the footer documents today:
Shift+Space (extend selection) and plain `a` (select visible rows).

## Workspaces

### Everything

```
Everything                                                    [Save view…]
[Filters 2] [Show as: Table] [Group by] [Sort: Newest first] [Columns]  [▭|▯] 20 items
Full text: "network" ×   Source: example@example.com ×   Grouped by Year ×
┌ results ────────────────────────────────────────────────────────────────┐
├ reading pane ───────────────────────── [Open relationship] [Tasks 2] [×] ┤
└─────────────────────────────────────────────────────────────────────────┘
          ┌ 3 selected · Select all 20 matching · Export · Review for deletion… · Clear ┐
```

- **Toolbar.** Filters, Show as, Group by, Sort, and Columns sit in one row.
  - Columns moves from a `<details>` strip into a menu with the same seven
    checkboxes and the same `columns` URL state.
  - Preview position becomes a two-icon segmented control, keeping radiogroup
    "Preview position" with radios "Below" and "Right". It still appears only
    when the results are at least 960px wide.
  - The count appears once, at the right edge.
- **Sort.** "Newest first" becomes a menu listing the one supported order,
  marked "Other orders aren't available yet". It keeps the accessible name
  "Sort: newest first", so `s` opens it. `r` keeps announcing that reversing
  isn't supported.
- **Context chips.** The query, filters, and groupings appear as removable chips
  with readable labels ("Full text", "Source"). Removing a grouping keeps the
  name "Remove {label} grouping".
- **Selection dock.** The selection bar becomes a kit `BottomDock` that appears
  only while at least one row is selected. It keeps "Select all N matching
  items", "Export selection", meeting-context export, and "Clear selection".
  - It adds **Review for deletion…**, which starts the same flow as `d` (or `D`
    in all-matching mode): `openDeletionReview` switches to Deletions and runs
    the preflight.
  - "Open selection in source" moves to an overflow menu. It is shown disabled
    with its reason as a sentence, which matches today's behavior because no
    handler exists.
- **Reading pane.** "Tasks for this message" becomes a header button labeled
  "Tasks" with the linked-task count, opening the same `TaskLinks` sheet.
  "Close" becomes an icon button that keeps the name "Close reading pane". The
  meta strip uses readable labels.

### Files

Files becomes the single file view.

- Everything's "Show as: Files" opens the Files workspace with the same query,
  filters, and groupings. Files shows the same "Show as" control; choosing
  Table or Timeline opens Everything. Switching is symmetric and visible.
- `explore/FilesPresentation.svelte` is deleted. Its per-row "Open containing
  item" action moves into the Files grid as a row action.
- Saved views with `presentation: 'files'` open in the Files workspace.
- **Toolbar.** Filename search, a **Type** menu, Filters, Show as, Group by, a
  **Sort** menu (Date, Filename, Size), and a **Visual search** toggle.
  - The Type menu replaces the eight raw MIME-family checkboxes with Images,
    PDFs, Audio, Video, Text, Documents, Archives, and Other. URL state
    `fileMIMEFamilies` is unchanged.
  - Column-header sorting still works and stays in sync with the Sort menu
    (`fileSort`).
  - Turning on Visual search expands a second row with the visual query,
    query image, and the unchanged provider disclosure.
- **Columns.** Type shows a readable name ("PDF", "PNG image"); the raw MIME
  type moves to the cell tooltip and the file viewer. Source shows the account
  display name when one is available.
- **Fixes.** The Sort control works (it has no handler today). The count comes
  from the Files query instead of "Count pending". Directory → Media & files
  passes change handlers so its filters and sorting work.

### Saved views

- **Save view…** is a header action in Everything and Files. It opens a dialog
  with Name and Description and saves the current view through the existing
  saved-views API. It keeps the field names "Name" and "Description" and the
  submit button "Save".
- The Saved views page becomes a library. Each view shows its name,
  description, a readable summary of its query, filters, grouping, and layout,
  and the actions "Open {name}", "Edit {name}", and "Delete {name}". The empty
  state points to Save view… in Everything and Files.
- The unused `selection` prop is removed.

### Relationships

- The list search is relabeled "Filter people and domains" (placeholder) to
  separate it from global search; its accessible name "Search people and
  domains" is unchanged.
- The person header groups "Open in Directory" and "Same person…" as one action
  set beside the Messages | Files view switch.
- On narrow screens the list-drawer button reads "People" instead of
  "Contacts".

### Directory

- **Toolbar.** Search, a **Filters** popover, and a **Sort** menu.
  - The popover holds contact state, category, organization, primary channel,
    and last contacted. The URL keys (`directoryContactState`,
    `directoryCategory`, `directoryOrganization`, `directoryPrimaryChannel`,
    `directoryLastContactAfter`, `directoryLastContactBefore`) and API
    parameters are unchanged.
  - "Last contacted" uses kit `DateRangePicker` instead of two free-text date
    fields.
  - Sort keeps the three orders and the `directorySort` URL key.
  - Active filters show as removable chips.
- **Person detail sections** (tablist "Person detail sections"):

  | Section | Contents |
  |---|---|
  | Overview | "Last time we talked" brief, agenda, contact state, activity, meeting activity |
  | Profile | Structured profile (names, contact points, addresses, dates, categories, media), attributes, profile history |
  | Organizations | Unchanged |
  | Connections | Curated person-to-person relationships; renamed from "Relationships" to avoid confusion with the Relationships workspace |
  | Network | Unchanged |
  | Media & files | Person-scoped files, now with working filters and sort |
  | Maintenance | Profile-maintenance tracking, CardDAV publication, merge history and split |

- **Header actions.**
  - **Open relationship** opens the person's Relationships view when the person
    has a linked participant.
  - **Review facts** opens Reviews → Facts with `directoryPersonID` set.
  - An overflow menu holds "Rename person", "View profile history", and "Delete
    person". Rename and delete keep their existing confirmation steps and
    accessible names.
- **Partial dates in editors** stay text fields because a date picker cannot
  express `YYYY` or `YYYY-MM`. They get inline validation messages.

### Reviews

- One view switch under the header: Identity matches, Facts, Imported
  relationships. It keeps the radiogroup "Review type" and the `reviewKind` URL
  key.
- The second header ("Identity matches" with its own segmented control) is
  removed. Each queue's status filter becomes a **Show** menu in its list
  toolbar, keeping the names "Identity review state" and "Imported
  relationship review state" and the URL keys `identityState` and
  `relationshipReviewState`.
- Facts with no person selected shows a person picker that uses the existing
  directory search, instead of only linking to Directory.

### Sources

- **Header.** Title "Sources", description "Accounts and imports in your
  archive, and when they last synced." The action "View source operations" is
  renamed **Sync history** and still opens Operations filtered to source sync.
- **Table.**
  - Source: display name and a readable source type ("Mbox import", "Gmail").
  - Schedule: a sentence from CronField's summary logic, with the cron text in
    a tooltip.
  - Status: a compact chip.
  - Last successful sync: unchanged.
  - Action: "Sync now {name}", or a muted reason:

    | Code | Label |
    |---|---|
    | `source_not_schedulable` | Imported file — nothing to sync |
    | `sync_already_running` | Sync in progress |
    | `scheduler_unavailable` | Scheduler unavailable |
    | `sync_not_configured` | Sync not set up |
    | `sync_unavailable` | Sync unavailable |

- Error messages, item errors, and scheduler errors move to an expandable row
  detail. `sync_start_not_observed` and `stale_last_result` get sentences.

### Operations

- **Lane summary.** The five lane cards become one compact status list: one row
  per operation kind, grouped by lane, showing the status tone, the latest
  run's time, and the row's actions.
  - "Not configured" shows as gray **Off** with a **Set up** link to the related
    setting.
  - "History available" is no longer printed; "History unavailable" still is.
  - Related-status buttons ("Open Sources status" and the others) and actions
    ("Start CardDAV sync", "Build visual index", "Resume visual index") keep
    their names.
- **Toolbar.** Lane, Kind, State, and the date range stay in one row with their
  URL keys. "Refresh operations" becomes kit `RefreshControl` in the header.
- **Runs.** Counters read "20 messages processed · 20 added". An unknown
  trigger shows "—" instead of "Unspecified". Each `OperationPublicErrorCode`
  maps to a sentence; the detail panel keeps the raw code.

### Deletions

- **Header.** Title "Deletions", description "Deletions you've staged. Nothing
  is deleted until you run `msgvault delete-staged`."
- **Review.** The review panel appears when a selection arrives from
  Everything. Without one, an empty state explains: "Select items in
  Everything, then choose Review for deletion…". The summary shows expiry as
  relative time and each unavailable reason as a sentence. Only "Confirm stage
  deletion" is red.
- **Manifests.** A table of ID (monospace), description, items, status chip
  (Pending, In progress, Completed, Failed, Cancelled), and the row actions
  "Inspect {id}" and "Cancel {id}". The detail opens beside the table.

### Settings

- The category list stays as the page's second-level navigation under a normal
  page header. The selected category is stored in a new `settingsCategory`
  field of the `explore` URL state, so reload and Back keep the category.
  `settingsAuthority` deep links keep working.
- **Save model.**
  - Catalog categories show the save bar only when drafts exist, with
    "Discard" and "Save changes" in blue. The status text "N unsaved changes"
    stays.
  - Each category states when saved changes apply: "After the daemon restarts",
    or for Appearance "Right away — no restart needed".
  - Controls that save through their own endpoints (provider credentials,
    enrichment providers, CardDAV account, People sweep) say "Saves
    immediately" beside their buttons.
- **Appearance.** Saving `web.theme`, `web.density`, or
  `web.default_search_mode` updates the open tab through a callback from
  SettingsWorkspace to App. A note explains that the Display menu can override
  the saved default in one tab.
- The plain-HTTP warning stays as a compact kit `Notice`.

### Sign-in and boot screens

The login, connecting, connection-error, and OAuth-callback screens use the
same palette and type.

## Control relocation

Every control not listed here keeps its location, label, and accessible name.

| Control | Today | After |
|---|---|---|
| Workspace tabs | Top bar center, nav "Primary" | Sidebar, nav "Primary" |
| Archive status | Top bar right | Sidebar footer |
| Temporary density | Top bar combobox | Display menu radio group |
| Use daemon theme | Top bar button | Display menu item |
| Search form, mode, Search button | Everything search row | Top bar |
| Preview position | Everything header, text plus segmented control | Everything toolbar, icon segmented control |
| Result count | Everything header and context bar | Toolbar right edge, once |
| Columns | `<details>` strip above the table | Toolbar menu |
| Newest first | Button that only announces | Sort menu with the one supported order |
| Keyboard hints | Everything footer, selection bar badges | Keyboard shortcuts dialog; sidebar footer entry |
| Selection bar | Always visible above results | Bottom dock while a selection exists |
| Stage deletion entry | `d` / `D` only | Also "Review for deletion…" in the dock |
| Open selection in source | Selection bar, reason text | Dock overflow, disabled with sentence reason |
| Tasks for this message | Collapsed disclosure | Reading-pane header button with count |
| Close reading pane | Text button | Icon button, same name |
| Show as: Files | Everything-only files grid | Opens the Files workspace |
| File type filter | Eight raw checkboxes | Type menu with readable names |
| File sort | Column headers only | Sort menu and column headers |
| Save this view | Saved Views page form | "Save view…" in Everything and Files headers |
| Directory filters | Seven inline controls | Filters popover and Sort menu |
| Directory date filters | Two `YYYY-MM-DD` text fields | Date range picker |
| Person detail Overview sections | One long Overview | Overview, Profile, Maintenance sections |
| Person "Relationships" tab | Record tab | Renamed "Connections" |
| Rename, profile history, delete person | Structured profile section | Person header overflow menu |
| Review state filters | Second header segmented control | Show menu in the queue toolbar |
| Sources header action | "View source operations" | "Sync history" |
| Refresh operations | Header button | Header `RefreshControl` |
| Settings category | Transient | `settingsCategory` URL state |

New entry points: Directory → Relationships ("Open relationship"), Directory →
Reviews Facts ("Review facts"), and a person picker in Facts.

## Tests and accessible names

- Accessible names stay the same wherever the control survives in the same
  form. The kit test helper `selectKitTopBarTab` in `web/tests/kit-ui.ts`
  changes to click the sidebar item in nav "Primary".
- Expected test edits, listed in each pull request:
  - The combobox "Temporary density" becomes a menu radio group
    (`density-restoration`, `theme-keyboard`, `docs-fixture-screenshots`,
    `e2e/accessibility`).
  - The grid "Files in current context" and region "Files presentation" are
    replaced by the Files grid (`presentations`).
  - Visible labels change to sentence case, such as "Saved views".
  - The person tab "Relationships" becomes "Connections", and Overview
    sections move (`e2e/directory`, `directory-network`, `directory-review`,
    `e2e/accessibility`).
  - "View source operations" becomes "Sync history" (`operations`).
- Each pull request adds tests for the behavior it changes: the Save view
  dialog, the deletion entry in the dock, Files-view round trips, the Type and
  Sort menus, the Directory filter popover, the Settings category URL, and the
  Appearance save updating the open tab.
- `e2e/accessibility` (axe) runs on every workspace in both themes.
- Each pull request includes before and after screenshots from the real daemon
  with the Enron docs fixture, at desktop and phone widths, in light and dark.
  The docs screenshots (`docs/screenshots/generate-web-fixture-screenshots.sh`)
  are regenerated in the final pull request.

## Delivery

Four stacked pull requests. Each leaves the application shippable.

1. **Foundation and shell.** Palette, type, and status vocabulary; sidebar,
   top bar, global search, Display menu; `PageHeader` applied to every
   workspace; empty-state conventions; keyboard registry additions and footer
   removal; tab titles; sign-in and boot screens.
2. **Everything, Files, and Saved views.** Toolbar, context chips, Sort and
   Columns menus, selection dock, reading-pane header, Files unification and
   fixes, Save view dialog, Saved views library.
3. **People.** Relationships header, Directory filters and person sections,
   cross-links, Reviews structure and fact person picker.
4. **Manage.** Sources, Operations, Deletions, Settings, and the code-label
   maps they need; docs screenshots and the [Web UI guide](../web-ui.md).

## Follow-ups

These need backend changes and are not part of this overhaul.

- **Reviews count.** No API returns a pending review count, so the sidebar shows
  no badge. A count endpoint would enable one.
- **Files fields in saved views.** The saved-view schema has no fields for the
  filename filter, type filter, or file sort, so saving a Files view keeps only
  the shared context.
- **Sort orders.** Everything supports only newest first.
- **Open in source.** The daemon always reports
  `open_in_source:trusted_source_link_unavailable`.
