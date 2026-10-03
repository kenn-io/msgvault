---
title: "Web UI overhaul, PR 4: Manage"
description: "Specification for managing archive sources, background work, and settings in the Web UI."
last_edited: "2026-10-01"
---

# Web UI overhaul, PR 4: Manage

Status: approved 2026-09-30 after review; implemented on the ui-ux-manage
branch (PR pending). [Implementation rulings](#implementation-rulings) records
where the build differs from the text below. This spec refines
the Manage sections of the [Web UI overhaul design](web-ui-overhaul-design.md)
(Sources, Operations, Deletions, Settings, and the sign-in and boot screens)
against the code on `main` at a3f5f04d. The design owns the shared rules
(palette, page structure, toolbars, status vocabulary, code labels). This spec
owns the exact PR 4 behavior, the decisions the design left open, and the test
changes. Where the two disagree, this spec records the deviation under
[Decisions for review](#decisions-for-review).

PR 4 is the last overhaul pull request, so it also brings the
[Web UI guide](../web-ui.md) text up to date. A maintainer regenerates its
screenshots separately (ruling R1).

## Outcome

After PR 4, every Manage page uses the same status vocabulary as the rest of
the app: green for healthy, blue for in progress, amber for attention, red for
failure, and gray for off. No page shows a raw API code as its only
explanation, no button is purple or green, and Settings remembers its category
across reloads and applies appearance changes to the open tab.

No capability is removed. Every control keeps its accessible name unless this
spec names the change.

## What people see today

Observed with the Enron docs fixture at 1440×900 and 420×860, light and dark.

- **Sources** prints `source_not_schedulable` in red as the row's action and
  `stale_last_result` in red under a green "Completed" chip. The source type is
  the raw code ("mbox"). The cron text appears twice, as a summary and as
  visible metadata.
- **Operations** shows five lane cards in which every unconfigured feature has
  a red dot, and every kind repeats "History available". Run triggers read
  "Unspecified" when none was recorded. Counters read "20 processed messages ·
  20 added messages · 0 updated messages · 0 item errors messages". A run
  error shows its code in bold above the server's sentence.
- **Deletions** with no selection shows a disabled "Review selection" button
  with key hints, and "No deletion manifests yet." in an amber warning box.
  After a review it prints every unavailable action as a raw code, including
  export and open-in-source reasons that have nothing to do with deletion, the
  expiry as an ISO timestamp, and the size in bytes. "Stage deletion" is a
  solid red button even before the confirmation.
- **Settings** loses its category on reload. The save bar is always visible,
  with a green "Save settings" button that stays disabled with no changes.
  Appearance says "Changes apply right away", but a saved theme or density
  only reaches the open tab after signing in again. The plain-HTTP warning is a
  hand-built amber box.
- **Operations → Set up targets** do not exist, and the one existing
  document-index settings link (`carddav/navigation.ts`) points at
  `analytics.auto_build_cache`, which is unrelated to document extraction.

## Sources

- **Header.** Title and description are already as designed. The header action
  "View source operations" becomes **Sync history** and still opens Operations
  filtered to source sync.
- **Columns.** Source, Schedule, Status, Last successful sync, Action.
  - **Source:** display name, then a readable type and the identifier
    ("Mbox import · pete.davis@example.com"). One label map covers the source
    types the daemon reports (`internal/api/scheduler_jobs.go`): Gmail, IMAP,
    Microsoft mail, Teams, Discord, Meeting import, SMS backup, iMazing CSV,
    Circleback, Google Calendar, Muesli, Granola, Notion meetings, PST import,
    Apple Mail, Mbox import, plus the types in ruling R3. An empty type shows
    no label, only the identifier. Unknown types fall back to sentence case.
  - **Schedule:** the `scheduleSummary` sentence with the cron text in its
    tooltip only; the visible cron line is removed. "Not scheduled" and "On
    demand · imported through the API" stay.
  - **Status:** one chip: Syncing (blue, with the existing progress line),
    Completed (green), Completed with errors (amber), Failed (red), or Never
    synced (gray). The time of the latest result stays under it.
  - **Action:** "Sync now {name}" (unchanged), or a muted, non-red reason:

    | Code | Shown as |
    |---|---|
    | `source_not_schedulable` | Imported file — nothing to sync |
    | `sync_already_running` | Sync in progress |
    | `scheduler_unavailable` | Scheduler unavailable |
    | `sync_not_configured` | Sync not set up |
    | anything else | Sync unavailable |

    Meeting-import sources keep "On-demand API source". The raw code stays in
    the reason's tooltip.
- **Row detail.** When a source has a sync error message, item errors, or a
  scheduler error, the row gets a disclosure button "Show details for {name}".
  It expands a detail row with the error message, the scheduler error, and the
  item errors (each with its error message). Rows without errors have no
  button.
- **Sentences for two status codes.**
  - `stale_last_result`: "This result may be out of date." (amber, under the
    status chip).
  - `sync_start_not_observed`: "The sync was requested, but it hasn't started
    yet. Refresh to check again." It stays a status message, not an error.

## Operations

- **Header.** The "Refresh operations" button becomes kit `RefreshControl`
  in the header actions, labelled "Refresh operation status", with "Updated N
  ago" from the last successful status load. It reloads the status list only,
  every 5 minutes and on demand. It never touches run history. See
  [decision 1](#decisions-for-review).
- **Run history reload.** A "Reload run history" button at the end of the
  filter row keeps today's refresh behavior for history: it reloads status and
  page one of runs, cancels paging, and replaces the loaded rows.
  - Because run IDs are encrypted per response and the history cursor belongs
    to one snapshot, a refreshed status list can show a newer latest run than
    the loaded history. The history changes only when someone reloads it,
    changes a filter, or loads more.
  - When "Load more operation history" meets a history that changed, the
    existing notice "Operation history changed. Restart from the first page."
    and its "Restart operation history" button stay as they are.
- **Status list.** The five lane cards become one region "Operation lanes"
  holding one list per lane (headings Messages, Facts, Contacts, Documents,
  Attachments), with one row per operation kind:

  | Part | Content |
  |---|---|
  | Name | Kind label, as today |
  | Status | A chip: the active run's state (Running or Queued) when one exists; otherwise **Off** (gray) when not configured; otherwise the latest run's state with the shared tones; otherwise "No runs yet" (gray) when history is available, and no chip when it is not |
  | Time | Latest run's start time; "Last succeeded {time}" when it differs from the latest |
  | Actions | The existing related-status and action buttons, unchanged names; **Set up** for an Off kind with a settings target |

  - "History available" is no longer printed; "History unavailable" stays as
    an amber note on the row.
  - "Active" runs show as the status chip (Running or Queued, blue).
  - A lane with no kinds shows "Status unavailable" (gray).
- **Set up targets.** An Off kind shows one of three things, depending on
  where it is configured:

  | Kind | Off row shows |
  |---|---|
  | `message_embedding` | **Set up**: Settings → Search, focusing `vector.enabled` |
  | `person_embedding` | **Set up**: Settings → Search, focusing `vector.people.enabled` |
  | `visual_embedding` | **Set up**: Settings → Search, focusing `vector.multimodal.enabled` |
  | `person_enrichment` | **Set up**: Settings → Person enrichment (its own editor) |
  | `person_sweep` | **Set up**: Settings → People sweep (its own editor) |
  | `carddav_sync` | The existing "Open CardDAV settings" |
  | `document_extraction` | "Configured in config.toml on the daemon host." and a link, [Document indexing setup](https://msgvault.io/docs/usage/document-indexing/#configure-the-policy) |
  | `document_embedding` | "Configured in config.toml on the daemon host. Also needs semantic search." and a link, [Document search setup](https://msgvault.io/docs/usage/document-indexing/#semantic-and-hybrid-document-search) |
  | `source_sync` | No Set up; "Open Sources status" stays |

  - The daemon decides "configured" (`internal/api/operations.go`).
    Document extraction needs `attachments.documents.enabled` and a usable
    document index scope. Document embedding needs `vector.enabled` and
    `attachments.documents.index.embeddings.enabled`. Neither has a Settings
    control, so no Set up button points at Settings for them. No new settings
    controls are added.
  - The related-status panels use the same rule. "Open visual attachment
    settings" keeps its Search target. The document index and document vector
    panels replace their settings buttons ("Open document index settings",
    "Open document vector settings") with the same host-configuration line
    and guide link as the rows above. The `document_index` and
    `document_vector` values of `settingsAuthority` go away with them; an old
    link carrying one opens Settings on Appearance.
  - Guide links open in a new tab, like the existing Google Contacts setup
    link in CardDAV settings.
- **Toolbar.** Lane, Kind, State, and the date range stay in one row with their
  URL keys and names.
- **Runs table.**
  - Trigger: Manual, Scheduled, or "—" when none was recorded.
  - Counters: zero counters are left out of the table (the detail keeps all).
    Each unit is named once, on the first counter that uses it: "20 messages
    processed · 20 added · 3 people updated". "No counters" stays.
  - A failed or partial run shows the server's error sentence under its state
    chip.
- **Run detail.** The error shows the server's sentence (`error.message`, which
  the daemon fixes per code in `internal/operations/types.go`) as the main
  text, with "Code: {code}" in monospace beneath it. See
  [decision 4](#decisions-for-review). Counter names read in sentence case
  ("Item errors"), with every counter shown.

## Deletions

- **Header.** Title unchanged; the description shows the command as code:
  "Deletions you've staged. Nothing is deleted until you run
  `msgvault delete-staged`."
- **Staging.**
  - **No selection:** an `EmptyState` "Nothing selected for deletion" with
    "Select items in Everything, then choose Review for deletion…". The
    disabled button and key hints are removed.
  - **Selection present:** a "Review selection" panel with the "Review
    selection" button (unchanged name). `d` and `D` keep working on this page.
  - **Arriving from Everything** keeps today's flow: the review runs at once
    and, when staging is available, the confirmation opens. See
    [decision 5](#decisions-for-review).
  - **Review summary:**
    - "2 items · 3 KB", using one shared `formatBytes` that replaces the four
      copies in Files, the file viewer, the Everything table, and the reading
      pane.
    - "{X} can be staged · {Y} will be skipped", plus the exclusion count.
    - "Review expires in 2 hours" as relative time, with the exact time in a
      tooltip.
    - Only the `stage_deletion` reason, as the sentence
      `preflightReasonLabel('stage_deletion', reason)` already produces. The
      export, file-export, and open-in-source reasons are not shown here.
    - The semantic-search note stays.
  - **Buttons:** "Dry run" (outline) and "Stage deletion…" (primary blue, not
    red). Only "Confirm stage deletion" in the confirmation is red.
  - The dry-run and staged results show in a neutral status panel, not an
    amber box. The partial-staging warning stays an amber `alert`.
- **Manifests.**
  - No manifests: an `EmptyState` "No staged deletions" with "Deletions you
    stage appear here, along with their execution status."
  - A table "Deletion manifests" with columns ID (monospace), Description,
    Items, Status, Created, and Actions. Status is a chip: Pending (blue), In
    progress (blue), Completed (green), Failed (red), Cancelled (gray).
    Created is the manifest's `created_at` as a short date and time.
  - Row actions keep their names: "Inspect {id}" (outline) and "Cancel {id}"
    (outline, not red; only for Pending and In progress). "Confirm cancel
    manifest" stays red.
  - The detail opens beside the table wider than 900px and below it at 900px
    and narrower, with a "Close manifest detail" button. Closing it returns
    focus to that manifest's "Inspect {id}" button. Status uses the same chip.
- **Selection bar (deferred from PR 2).** When the Everything preflight
  reports `stage_deletion` unavailable, "Review for deletion…" is disabled and
  shows the reason sentence, the same way "Open selection in source" does.

## Settings

- **Category in the URL.** A new `settingsCategory` field in the `explore` URL
  state holds the selected category id. Reload and Back keep it.
  `settingsAuthority` deep links still choose their category and focus their
  setting, and set `settingsCategory` to match. An unknown category falls back
  to Appearance.
- **Save bar.**
  - Catalog categories show the save bar only while drafts exist: the status
    text ("1 unsaved change", or with ". Enter a number to save."), "Discard",
    and **Save changes** (blue, solid; "Saving…" while saving). The button was
    "Save settings".
  - After a save that needs a restart, the existing "Saved. Restart the daemon
    to apply these changes." notice stays.
- **When changes apply.** Each catalog category keeps its posture line, with
  this copy:

  | Posture | Text |
  |---|---|
  | live (Appearance) | Saved changes apply right away — no restart needed. |
  | restart | Saved changes apply after the daemon restarts. |
  | mixed | Most saved changes apply after the daemon restarts. Rows that differ are marked. |
  | none | Set in config.toml on the daemon host. |

- **Controls with their own save.** Provider credentials, enrichment providers,
  the CardDAV account, and People sweep save through their own buttons. Each
  such group shows one line, "These save immediately when you use their
  buttons — not with Save changes.", beside or above its first button. Their
  existing apply notes stay.
- **Appearance applies to the open tab.**
  - After a successful save that changed `web.theme`, `web.density`, or
    `web.default_search_mode`, SettingsWorkspace passes the saved values to
    App through a callback, and App updates its defaults.
  - **Theme and density** apply to the open tab at once, unless a Display menu
    override is active in this tab; the override still wins. The Appearance
    note says: "Choose “Use daemon theme” and Temporary density “Auto” in the
    Display menu to return this tab to these saved values."
  - **Default search mode** does not change the open view's mode or URL.
    Saving writes the new mode to this browser's remembered mode
    (localStorage `msgvault-search-mode`), so the next tab opened here without
    a mode in its link uses it. The open view keeps its mode even when it had
    none in its URL, which today's re-resolution would change. The setting's
    description becomes "Used when a tab opens without a search mode in its
    link. Your current search keeps its mode." (daemon metadata,
    `internal/api/settings_metadata.go`).
- **Plain-HTTP warning** becomes a compact kit `Notice` (tone warning) with
  the same text. Its role changes from `alert` to kit's `status`.

## Sign-in and boot screens

The login, connecting, connection-error, and OAuth-callback screens already use
palette tokens. PR 4 gives the OAuth-callback screen the same brand line and
layout as connecting, and moves the duplicated login and boot screen styles
into one shared rule. Copy is unchanged.

## Guide and screenshots

- `docs/web-ui.md` describes the shipped navigation, toolbars, and every
  workspace as they are after PR 4, in the guide's existing structure.
- The docs screenshots are not regenerated in PR 4 (ruling R1). A maintainer
  regenerates them with `docs/screenshots/generate-web-fixture-screenshots.sh`,
  following its fixture-lock and provenance rules.

## Decisions for review

1. **Refresh control refreshes status only.** Kit `RefreshControl` calls one
   callback for clicks and for its 5-minute timer. Today's refresh reloads
   status and page one of runs and replaces the loaded rows, so on a timer it
   would discard history someone paged through. Merging new runs into loaded
   history is not possible with the current API: each response encrypts run
   IDs with a fresh nonce (`internal/api/operation_tokens.go`), so the same
   run has a different ID every time, and any run change advances the
   history's membership revision, so the API rejects an older cursor with
   409 `operation_history_conflict`. The refresh control therefore reloads
   the status list only, and history keeps a manual "Reload run history"
   with today's restart behavior. Merging history waits for a backend
   contract (see [Not in PR 4](#not-in-pr-4)).
2. **Gray "Off" uses a chip, not a dot.** Kit `StatusDot` has no gray status.
   Operations uses kit `Chip` (tone `muted`) for Off and for other statuses, so
   every status in the list is a chip with a word, not a colored dot alone.
3. **Set up only where Settings can change it.** People sweep and person
   enrichment have their own editors, so Set up opens their category without
   focusing a row. The document kinds are configured only in config.toml on
   the daemon host, so their rows explain that and link to the setup guide
   instead of opening a Settings page that cannot turn them on.
4. **Error sentences come from the daemon.** The design asks for a sentence per
   `OperationPublicErrorCode`. The daemon already sends a fixed, public-safe
   sentence for every code (`fixedPublicErrorMessages`), so the UI shows that
   sentence and keeps the code beneath it. A second map in the web app would
   drift from the daemon.
5. **Arriving from Everything still opens the confirmation.** The design says
   the review panel appears when a selection arrives. Today the review runs and
   the confirmation opens immediately when staging is available; existing
   browser tests depend on that. This spec keeps that flow and shows the review
   summary behind the confirmation, so closing the confirmation leaves the
   reviewed selection on screen.
6. **Zero counters hidden in the table.** The table hides counters whose value
   is 0; the run detail shows them all.

## Implementation rulings

Review of the implementation settled these points. Each one overrides the
text above where they differ.

- **R1. Screenshots deferred.** PR 4 updates the guide text only. Regenerating
  the docs screenshots needs a macOS run for the darwin images and an
  authorized docs-assets publish, so a maintainer does it.
- **R2. New Settings authorities.** `settingsAuthority` gains
  `semantic_search` (Search, focusing `vector.enabled`) and
  `person_embeddings` (Search, focusing `vector.people.enabled`).
- **R3. More source-type labels.** The label map also covers Beeper, Slack,
  EML import, Maildir import, WhatsApp, Apple Messages, Facebook Messenger, and
  Google Groups. An empty `source_type` shows no type label (the identifier
  only), not "Gmail".
- **R4. Refresh keeps focus.** Kit disables the refresh button while busy, so
  when a refresh started from it completes, focus returns to the button. It
  returns only if focus is still on the page body or inside the refresh
  control; a control the person moved to keeps focus.
- **R5. Standard breakpoint.** The manifest detail sits beside the table above
  the standard 900px breakpoint and stacks below it at 900px and narrower.
- **R6. Notice label.** Kit `Notice` prints a visible "Warning" label above
  the plain-HTTP text. That label is accepted.
- **R7. Appearance note copy.** The note reads "Choose “Use daemon theme” and
  Temporary density “Auto” in the Display menu to return this tab to these
  saved values."
- **R8. Focus after the save bar closes.** When Save or Discard removes the
  save bar, focus moves to the category heading. The same happens when a
  conflict reload (412) leaves no drafts.
- **R9. An active run beats Off.** A kind that is not configured but has a
  running or queued run shows that run's state chip instead of Off.
- **No runs yet needs history.** "No runs yet" shows only when the kind's
  history is available. A kind with unavailable history and no run shows no
  state chip, only its "History unavailable" note.
- **Muted chip contrast.** Kit mixes muted chip text toward its background,
  which falls below 4.5:1 in the light theme. `web/src/styles/tokens.css`
  overrides the muted chip text color with the palette's muted text token, so
  Off, No runs yet, and Cancelled meet 4.5:1 in both themes.

## Not in PR 4

- New operation kinds, settings, or deletion capabilities.
- Sync progress polling changes on Sources.
- The kit `Typeahead` accessibility fix (kenn-io/kit-ui#79).
- Merging refreshed runs into loaded history. It needs a run identity that
  stays the same across responses and a pagination contract that survives new
  or changed runs; the API provides neither today.

## Tests

Changed assertions, by file:

| File | Change |
|---|---|
| `SourcesWorkspace.test.ts`, `AppShell.test.ts:967`, `tests/operations.spec.ts:60` | "Sync history"; reason labels instead of codes; status chip; visible cron text removed; row details |
| `tests/archive-management.spec.ts` | Sources row text; deletion buttons and dialogs |
| `OperationsWorkspace.test.ts`, `tests/operations.spec.ts`, `tests/e2e/operations.spec.ts` | Status list in place of cards; Off; "History available" gone; counters format; trigger "—"; error sentence |
| `tests/e2e/accessibility.spec.ts`, `tests/e2e/keyboard.spec.ts`, `tests/e2e/operations.spec.ts:124` | "Refresh operations" becomes "Refresh operation status" (status only) or "Reload run history" (history); status list; deletions empty state |
| `DeletionsWorkspace.test.ts` | Empty states; reason sentence instead of the raw code (:398); size format; button tones; manifests table |
| `SelectionBar.test.ts` | Review for deletion disabled with a reason |
| `SettingsWorkspace.test.ts`, `App.test.ts`, `tests/session-navigation.spec.ts`, `tests/e2e/security.spec.ts:151` | "Save changes"; save bar hidden without drafts; posture copy; Notice role |
| `explore/state.test.ts` | `settingsCategory` round trip |

New tests cover:

- Sources: each reason label; the status chip for each latest result; the row
  detail toggle with errors and its absence without them.
- Operations: Off and its target for each kind in the targets table,
  including the host-configuration line and guide link for both document
  kinds and the document related-status panels; counters format with mixed
  units and zero values; trigger "—".
- Operations refresh: with a second page of runs loaded and a run's detail
  open, let the refresh timer fire (fake timers) and click the refresh
  control. Each time only the status request is sent; the loaded rows, the
  open detail, and focus stay. "Reload run history" sends the status and
  page-one runs requests and replaces the rows. A "Load more" answered with
  409 `operation_history_conflict` shows the existing conflict notice and
  "Restart operation history". Test fixtures give each response freshly
  encoded run IDs, as the daemon does, so a test cannot pass by matching IDs
  across responses.
- Deletions: empty state with no selection; stage_deletion sentence; only the
  confirmation button is red; manifest status chips and detail close.
- Settings: category survives reload and Back; `settingsAuthority` still
  focuses its setting; save bar appears only with drafts; saving theme and
  density updates the open tab; an active Display override still wins; saving
  the default search mode leaves the open view's mode and URL unchanged and
  changes the mode a new tab opens with.
- Accessibility (axe) on every Manage page in both themes, including the
  Sources row detail, the Operations status list, a Deletions review, and each
  Settings category with a draft.

Before and after screenshots at 1440×900 and 420×860, light and dark, from
the docs fixture wait for the maintainer screenshot run (ruling R1).
