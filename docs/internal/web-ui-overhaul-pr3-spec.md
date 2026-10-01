# Web UI overhaul, PR 3: People

Status: draft for review, 2026-09-30; revised after review the same day. PR 3 has not started. This spec
refines the People sections of the [Web UI overhaul design](web-ui-overhaul-design.md)
(Relationships, Directory, Reviews) against the code on `main` at ef66efc1. The
design owns the shared rules (palette, page structure, toolbars, code labels,
delivery). This spec owns the exact PR 3 behavior, the decisions the design left
open, and the test changes. Where the two disagree, this spec records the
deviation under [Decisions for review](#decisions-for-review).

## Outcome

After PR 3, the three People workspaces follow the pattern Everything and Files
already use: a page header, one toolbar row, removable chips for active
filters, readable labels instead of codes, and one primary action. A person's
Directory page is split into sections that each answer one question, and the
three People workspaces link to each other.

No capability is removed. Every control keeps its accessible name unless this
spec names the change.

## What people see today

Observed with the Enron docs fixture at 1440×900 and 420×860, light theme,
after promoting one participant to a Directory person.

- **Directory toolbar** is eight inline controls that wrap to two rows at
  1440px. The date fields are free text with a truncated `YYYY-MM-DD`
  placeholder. A value that is not shaped like `YYYY-MM-DD`, such as `last
  week`, is silently dropped from the query but stays in the field and the URL
  (`directory/controller.svelte.ts:576-580`). A well-shaped but impossible
  value, such as `2026-02-31`, passes that check and reaches the backend's date
  validation.
- **Directory list rows** show raw codes and ISO timestamps, such as
  "No primary channel · inactive" and "Last contact" followed by an ISO
  timestamp (`DirectoryList.svelte:93-94`). The Primary channel menu lists
  `email`, `phone`, `chat`.
- **"Promote to person"** is a purple `workflow` button, the last purple button
  outside Manage.
- **Person Overview** stacks 14 sections in one scroll: agenda, profile
  maintenance with every eligible field definition, CardDAV publication, the
  structured profile, attributes, summaries, contact state, meetings, the "Last
  time we talked" brief, activity, and merge history. The brief, which is the
  reason to open a person, sits near the bottom.
- **Rename, profile history, and delete** sit inside the Structured profile
  section, far from the person's name.
- **At 420px** the five person tabs wrap to three lines ("Media / & / Files").
- **No cross-links:** Directory cannot open the person's Relationships view or
  their facts. The only way to see a person's facts is to type a URL.
- **Relationships** labels its narrow-screen list button "Contacts", while its
  accessible name is "Show relationship list". The header shows the file count
  twice ("Files 0" in the switch and "0 files" in the counts line).
- **Reviews** repeats the selected review type as a second heading with its own
  segmented control ("Identity matches" and Candidate | Conflict | Accepted |
  Rejected). The view option reads "Fact review" while the design says
  "Facts". Facts with no person selected is a dead end with an "Open Directory"
  button. Review cards show raw states (`candidate`, `pending`) and "Person ID
  42" instead of a name.

## Relationships

- **List search.** The placeholder becomes "Filter people and domains". The
  accessible name stays "Search people and domains".
- **Narrow-screen list button.** The visible label and the drawer title become
  "People". The button's accessible name becomes "People" too, so the visible
  label is part of the name. The drawer keeps its accessible name
  "Relationship search and results".
- **Person header.** The name and avatar sit on the left with "Open in
  Directory" and "Same person…" as one action group on the right. The Messages
  | Files switch moves to its own row under the header, left-aligned, like the
  view row in `PageHeader`. It keeps the radio names "Messages" and "Files N".
- **Counts line.** Drops the file count, which the switch already shows:
  "20 items · Apr 25, 2001 – Jan 2, 2002". Dates use the shared date format.
- Identity chips name the profile by display name, not "profile 123", when the
  name is known.

Out of scope: the meeting panel (shared with Directory, see
[Not in PR 3](#not-in-pr-3)), the "All senders" toggle, and the degraded-state
`msgvault build-cache` hint, which is actionable as written.

## Directory list

### Toolbar

One row: search, **Filters**, **Sort**, and the result count at the right edge.

- **Search** keeps its accessible name "Search directory", its placeholder, the
  250 ms debounce, and the `directoryQuery` key.
- **Filters** is a button that opens a panel under the toolbar, the same
  pattern as Everything's Filters. The panel holds:

  | Control | Accessible name | URL key | Change |
  |---|---|---|---|
  | Contact state select | "Contact state" | `directoryContactState` | None |
  | Category text field | "Category filter" | `directoryCategory` | None |
  | Organization text field | "Organization filter" | `directoryOrganization` | None |
  | Primary channel select | "Primary channel" | `directoryPrimaryChannel` | Options read Email, Phone, Chat |
  | Last contacted after | "Last contacted after" | `directoryLastContactAfter` | Native `<input type="date">` with a clear button |
  | Last contacted before | "Last contacted before" | `directoryLastContactBefore` | Native `<input type="date">` with a clear button |

  The API parameters are unchanged. The Filters button shows the soft style
  while the panel is open or any filter is set, like Everything.
- **Dates.** Each boundary is independent, as today. The native input only
  yields a valid `YYYY-MM-DD` or an empty value, so the silent-drop path goes
  away for typed input. A URL that carries an invalid value (for example
  `2026-02-31`) is dropped when the URL is read, so the field, the chip, and
  the request agree.
- **Sort** is a select whose visible label reads "Sort: Name", "Sort: Most
  recently contacted", or "Sort: Least recently contacted". It keeps the
  accessible name that starts with "Directory order" and the `directorySort`
  key.
- **Chips.** A second line shows each active filter as a removable chip, for
  example "Contact state: Active", "Organization: Example Co", "Last contacted
  after Jan 5, 2024". Removing a chip clears only that filter. A one-sided date
  filter is one chip.
- **Count** reads "N people", or "N+ people" when more pages exist.

### Rows and header

- Rows show "Email · Active" and "Last contact Jan 2, 2002" instead of codes
  and ISO timestamps. One label map covers contact state and channel; unknown
  codes fall back to a sentence-cased form.
- "Promote to person" uses the standard primary button instead of the purple
  `workflow` tone. It stays the only header action and appears only while a
  participant is waiting to be promoted.

## Person detail

### Header

The person's name moves out of Overview into a header above the section tabs,
with actions on the right:

- **Open relationship** opens the person in Relationships through AppShell's
  `openRelationship(participantID)`. It appears only when the person has at
  least one participant ID. See [decision 2](#decisions-for-review) for which
  ID it uses.
- **Review facts** opens Reviews → Facts for this person
  (`workspace: 'directory_review'`, `reviewKind: 'fact'`,
  `directoryPersonID`).
- **More actions** is a menu with "Rename person", "View profile history", and
  "Delete person". It appears only when the structured profile controller is
  available, as the buttons do today. "Rename person" and "Delete person" open
  the existing inline rename form and delete confirmation directly under the
  header, keeping their group names ("Rename person", "Confirm deleting
  person"), buttons, disabled rules, and pending labels. "View profile history"
  opens the existing Profile history dialog.

### Sections

The tablist keeps the name "Person detail sections" and the existing keyboard
pattern. Tab ids keep the `person-<id>-<section>-tab` form.

| Section | Contents, in order | Source today |
|---|---|---|
| Overview | "Last time we talked" brief, agenda, attributes summary, contact state, activity, organization and connection summaries, meeting activity | Overview |
| Profile | Structured profile (names, contact points, addresses, dates, categories, media metadata), attributes | Overview |
| Organizations | Unchanged | Organizations tab |
| Connections | Unchanged content; the tab was "Relationships" | Relationships tab |
| Network | Unchanged | Network tab |
| Media & files | Unchanged content; the tab was "Media & Files" | Media & Files tab |
| Maintenance | Profile maintenance tracking, CardDAV publication, merge history and split | Overview |

- The attributes summary's Edit button switches to Profile and scrolls to the
  attributes section, which it does within Overview today.
- The Connections panel keeps its content; only the tab label and ids change
  (`person-<id>-connections-tab`).
- **Agenda.** When the task integration is disabled or unavailable, the agenda
  shows its status line and hides the "New agenda item" form, instead of
  showing a disabled form under an error-like sentence.
- **Narrow screens.** The tablist scrolls horizontally instead of wrapping, so
  every tab stays one line at 420px.

Media metadata editing stays in Profile with the rest of the structured
profile. It is separate from the Media & files section, which lists archived
attachments.

### Partial dates

Date fields in the person editors stay text fields because a date picker cannot
express `YYYY` or `YYYY-MM`. They get inline validation that mirrors what the
store accepts, which differs between profile dates and interval dates. The
range rules come from `PartialDate.Validate` (`internal/store/partialdate.go`):
year 1–9999, month 1–12, and a day that exists in that month, checked against
leap year 2000 when no year is given.

- **Interval dates** ("Employment start date", "Employment end date",
  "Relationship start date", "Relationship end date") require a year, as
  `validateEmploymentDate` and `ParseRelationshipDate` do. They accept `YYYY`,
  `YYYY-MM`, `YYYY-MM-DD`, and the compact `YYYYMMDD`. Year-less forms such as
  `--04-12` and any other text are invalid. An invalid value shows a message
  under the field, such as "Use a year, year and month, or full date, like
  2019, 2019-04, or 2019-04-12", and disables that editor's save button.
- **Profile dates** (the "Date" field in the structured profile editor) keep
  every form the editor accepts today. Values that `dateParts` recognizes as a
  structured date (`YYYY`, `YYYY-MM`, `YYYY-MM-DD`, `--MM-DD`, `--MM`, and
  `---DD`) must pass the range rules above; `2024-13` or `--02-30` shows a
  message and disables Save. Any other text is still saved as a text date
  (`date_text`), which the store accepts. The field shows the hint "Saved as
  text" for such a value, so people can tell it will not sort or compare as a
  date.

## Reviews

- **View switch.** The "Review type" radiogroup under the page header keeps its
  name, the `reviewKind` key, and its disabled-while-deciding rule. The options
  read "Identity matches", "Facts", and "Imported relationships".
- **One heading.** The second visible heading and description for each review
  type are removed. Each type keeps an `h2` with the same text as a
  visually-hidden heading, so screen readers still announce the section and the
  existing focus targets after a change keep working. Focus on that heading
  must stay visible: while the hidden heading has keyboard focus
  (`:focus-visible`), its queue section shows the standard focus ring, so
  keyboard users see where focus landed without the layout shifting. This
  covers every place that focuses these headings, including the imported
  relationship queue after a context change
  (`RelationshipReviewQueue.svelte:35`).
- **Show menu.** Each queue's state filter moves from a segmented control to a
  select at the start of the queue's toolbar, with visible labels "Show:
  Candidate" and "Show: Pending". Like Directory Sort, the select's accessible
  name starts with the old radiogroup name, for example "Identity review state:
  Show: Candidate" and "Imported relationship review state: Show: Pending".
  The keys `identityState` and `relationshipReviewState` are unchanged.
  Choosing a state still invalidates an open decision, as today.
- **Cards.** States show as status chips with readable labels (Candidate,
  Conflict, Accepted, Rejected, Pending) using the shared status tones: amber
  for Candidate, Conflict, and Pending, green for Accepted, gray for Rejected.
  The imported relationship card shows its state once. Diagnostic fields
  (evidence IDs, basis, service, scope) stay as they are.
- **Facts person picker.** Facts always shows a "Person" picker above the
  ledger. It searches the Directory with `listDirectoryPeople({ q, limit: 20 })`
  through kit `Typeahead`, the same way `PersonRelationshipEditor` finds
  people. Choosing a person commits `directoryPersonID`. With no person
  selected, the picker and a one-line explanation replace the "Open Directory"
  dead end. With a person selected, the ledger header shows the person's
  display name with "Open person profile", instead of "Person ID 42". The name
  comes from the picker selection or, after a reload, from the person endpoint
  Directory already uses.

## Decisions for review

1. **Filters panel, not a popover.** The design says the Directory filters live
   in a popover. This spec uses the disclosure panel Everything's Filters
   button already opens (`ContextBar.svelte`), so both workspaces behave the
   same and native date inputs are not nested inside a floating layer.
2. **Which participant "Open relationship" uses.** `relationshipTarget` is
   `cluster:<participant id>`, and the Relationships controller resolves any
   member ID to its cluster (`relationships/controller.svelte.ts:363, 704`).
   The button passes the lowest of the person's `participant_ids`, so the
   choice is stable. If a Directory person's participants belong to more than
   one relationship cluster, the button opens the cluster that contains the
   lowest ID. Showing one button per cluster needs data the person response
   does not carry.
3. **Visually hidden review headings.** Removing the second heading outright
   would drop the focus targets that review decisions and state changes move
   focus to. Hiding it visually keeps those targets and the heading outline
   with less code. Because kit's `kit-sr-only` stays clipped when focused, the
   section draws the focus ring instead, as described under Reviews.
4. **Rename and delete stay inline.** The header menu opens the existing
   inline form and confirmation instead of new dialogs. This keeps their
   roles, names, and flows, and their tests change only in how they are
   opened.
5. **"Show" uses a select, like Sort.** PR 2 made Sort and Group by kit
   `SelectDropdown` controls with "Sort: …" trigger labels. The review state
   filters follow the same pattern rather than a separate menu component.
6. **"Fact review" becomes "Facts"**, as the design's Reviews section says.
   Tests that name the radio or the region change.

## Not in PR 3

- **Meeting panel.** `MeetingPanel` renders in Relationships and in the person
  Overview with a heavy action-filter form. Restyling it affects both, and it
  is not a People-only component. It moves to a follow-up.
- **Relationship types list** in Connections shows type codes beside their
  labels ("acquaintance · acquaintance"). It stays as is.
- **Identity match and fact ledger diagnostics** (endpoint kinds, evidence and
  candidate IDs, source classes) stay raw, because they identify records for
  diagnosis.
- **Media & files filters and sort** already work; PR 2 wired them.

## Tests

Changed assertions, by file:

| File | Change |
|---|---|
| `DirectoryWorkspace.test.ts` | Filters panel must be opened before the filter fields; date fields become date inputs; Sort visible label; chips |
| `PersonDetail.test.ts` | Seven tabs; "Connections", "Media & files", "Profile", "Maintenance"; Overview order; "Merge history" moves to Maintenance |
| `StructuredProfileSection.test.ts` | Rename, history, and delete open from the header menu |
| `DirectoryReviewCentre.test.ts`, `FactReviewPanel.test.ts` | "Facts"; hidden headings; state selects; picker |
| `RelationshipReviewQueue.test.ts` | State select |
| `RelationshipsWorkspace.test.ts` | "People" button name |
| `controller.svelte.test.ts` (directory) | Invalid URL dates are dropped |
| `tests/e2e/directory.spec.ts` | `.filters` locator; tabs; maintenance region moves to Maintenance |
| `tests/directory-review.spec.ts`, `tests/e2e/accessibility.spec.ts` | "Facts"; state selects instead of radios |
| `tests/directory-network.spec.ts` | Tab count and order |

New tests cover the behavior this PR adds:

- Directory: setting and clearing each date boundary alone; a one-sided date
  chip; removing each chip clears only its filter; an invalid URL date is
  dropped from the field, chip, and request.
- Person header: "Open relationship" commits the relationship target for the
  lowest participant ID and is absent with no participants; "Review facts"
  commits the Facts view for the person; each menu item opens its existing
  flow.
- Interval dates: each year-bearing form is accepted; `--04-12`, free text,
  an out-of-range month, and February 30 are rejected with Save disabled.
- Profile dates: `--MM-DD`, `--MM`, and `---DD` are still accepted;
  `2024-13` and `--02-30` are rejected; free text saves as a text date and
  shows the "Saved as text" hint.
- Reviews: the Show select commits its key; the Facts picker searches,
  selects, and shows the name after a reload; a focused hidden heading gives
  its section a visible focus ring.
- Accessibility (axe) on each People workspace in both themes, including a
  person page and the Facts picker.

The PR includes before and after screenshots at 1440×900 and 420×860 from the
docs fixture.
