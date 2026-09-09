---
last_edited: "2026-09-08"
title: Web UI
description: Browse messages and files, maintain people, and monitor archive work from your browser.
---

# Web UI

The Web UI lets you search across your archive, read messages, browse files,
maintain your contact directory, and see whether sync and indexing work has
finished. It is embedded in the release binary and served by `msgvault serve`;
you do not need a separate web application process.

| Your question | Workspace |
|---|---|
| Where is that message, conversation, or meeting? | Everything |
| Who have I been in contact with? | Relationships, People, and Domains |
| Where is an attachment, image, or video? | Files |
| What do I know about this person? | Directory |
| Which identity matches or profile facts need my decision? | Reviews |
| Can I return to this search later? | Saved Views |
| Did sync, enrichment, or indexing finish? | Operations and Sources |
| What is staged for deletion? | Deletions |
| How do I change the daemon's configuration? | Settings |

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/static/relationships-dark-comfortable-darwin.png" alt="Experimental Relationships workspace in dark theme with ranked people and activity timeline" loading="lazy">
  <figcaption>Relationships ranked view and selected activity timeline.</figcaption>
</figure>

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/static/relationships-light-compact-darwin.png" alt="Experimental Relationships workspace in light theme with compact density" loading="lazy">
  <figcaption>Relationships workspace in light theme with compact density.</figcaption>
</figure>

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/static/analytical-dark-comfortable-darwin.png" alt="Experimental analytical web UI in dark theme with comfortable density" loading="lazy">
  <figcaption>Dark theme with comfortable density.</figcaption>
</figure>

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/static/analytical-light-compact-darwin.png" alt="Experimental analytical web UI in light theme with compact density" loading="lazy">
  <figcaption>Light theme with compact density.</figcaption>
</figure>

The screenshots use a curated public Enron research-data fixture. Authentic
names and message text are intentional; the repository's `docs-fixtures`
branch records provenance, attribution, and the content review. Screenshots
illustrate the workflows; newer controls may differ from these captures.

## Start and discover the URL

```bash
msgvault build-cache
msgvault serve
```

`build-cache` prepares the analytical tables before you open the browser. You
can also run `serve` directly and let the default startup maintenance build a
missing or stale cache in the background.

Foreground startup prints `API server: http://HOST:PORT`. The default
`server.api_port = 0` chooses a free port. Local CLI commands discover that
address through the private daemon runtime record under the configured msgvault
home; browsers should use the printed URL. Configure a fixed port for a stable
remote bookmark:

```toml
[server]
bind_addr = "127.0.0.1"
api_port = 8080
```

The default loopback deployment is trusted. If an API key is active and the
request is not loopback-trusted, `/` still loads the public shell and the UI asks
for the key. A successful login creates an expiring, in-memory browser session.
Daemon restarts, logout, expiry, and API-key activation invalidate sessions.
Existing bearer-key API clients are unchanged.

## Remote access and HTTPS

For remote access, set a strong API key and bind deliberately:

```toml
[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = "replace-with-a-long-random-key"
```

HTTPS at a reverse proxy is recommended. Forwarded scheme and host headers are
accepted only from explicitly trusted proxy addresses or CIDRs:

```toml
[server]
trusted_proxies = ["127.0.0.1", "192.0.2.8/32"]
```

Do not add a whole client network merely to silence proxy warnings. When HTTPS
is known through a trusted proxy, the browser cookie uses `Secure`. Plain HTTP
on an encrypted private network is supported as an explicit tradeoff, but the UI
warns that its session cookie travels without TLS. `HttpOnly` and
`SameSite=Strict` do not encrypt that traffic.

## Explore and search

Everything opens as a compact, sortable table of logical entries: one row per
email, calendar event, meeting note, other durable item, or chat conversation.
Raw chat fragments appear only after drilling into a conversation. Filter,
Group by, Show as, and Search compose into one URL-backed context, so browser
Back and Forward restore the analytical slice and focused item.

Search mode is always explicit:

- **Full text** searches the complete lexical index.
- **Semantic** ranks only content covered by the current embedding generation.
- **Hybrid** combines complete lexical matching with semantic ranking where it
  is available.

The context strip reports semantic coverage. Disabled, building, stale,
incomplete, unavailable, and ready are different states; msgvault never silently
changes the requested mode. Semantic-only results cannot include unembedded
content. Hybrid retains full-text coverage and labels the semantic contribution.

Search execution also has explicit terminal states. **Timed out** means the
selected search backend did not finish within the request budget; it is an
error, not an empty result, and the query and filters remain available to retry.
**Incompatible mode** means the daemon, browser contract, or current index
cannot safely honor the selected search mode. Update or rebuild the named
component, or deliberately select a supported mode. Msgvault does not quietly
substitute full-text search for either state.

## Cache states

The web tables share one analytical cache across message types. When it is missing,
building, stale, or unavailable, the UI names that state instead of quietly
switching selected modalities to a different read path. Run `msgvault
build-cache` for an explicit rebuild, or leave `analytics.auto_build_cache =
true` for daemon startup to build a stale cache. With `analytics.engine =
"duckdb"`, startup fails if no usable cache can be produced.

## Files and containing context

Files is a searchable table of attachment date, filename, type, size, person or
domain, source, containing item, and content availability. Archived images and
PDFs open in application-controlled viewers. Metadata-only, missing,
unsupported, and previewable content remain distinct. From a file, navigate to
its containing item and then its email or chat conversation.

Filter by filename and file type. In a person's Media & Files view, choose a
media gallery or file table and narrow the relationship to **From them**,
**To them**, or **Group conversations**. These directions describe the
containing messages; they do not identify people pictured in an image.

Turn on **Hosted visual search** to describe image or video content, or supply
a JPEG, PNG, or WebP query image. The UI discloses that the query goes to the
configured provider. It requires the separate
[visual index](/docs/usage/vector-search/#visual-attachment-search); ordinary
filename browsing does not use that provider.

### Images in email

The reader displays archived inline images and leaves remote images unloaded
until you choose **Load images**. That choice applies to the current item and
fetches images through the daemon. Moving to another item resets it.

For unattended, offline preservation of remote images, see
[Archive Remote Email Images](/docs/usage/remote-images/). Reader permission
to load an image and permission to archive remote images during ingest are
separate choices.

## People and domains

People combines identifiers backed by explicit archive identity evidence; it
does not merge records merely because their display names match. Select a
person to inspect contextual activity across email, chat, calendar events, and
meeting notes, plus the files associated with that person. The active search
and filters continue to scope both the timeline and file table.

People in this workspace are observed identity clusters. Source identities
that mean “me,” explicit durable profile promotion, display-name overrides, and
typed profile attributes are separate curated operations; see [People,
Profiles, and Source Identities](/docs/usage/people/).

### Directory and Reviews

Directory holds durable people: the profiles you explicitly curate and keep
across sources. Search by name, email, or organization; filter by contact
state, category, primary channel, or last-contact dates; and sort by most or
least recently contacted.

Its person detail keeps
Overview, Organizations, Relationships, Network, and Media & Files together.
Edit structured profile information, attributes, employment, and typed
relationships here. Curated display names also appear in message views,
analytics, and exports while source identifiers remain available.

The Overview tab's **Last time we talked** card summarizes the person's recent
chat and text messages. Enroll the person, generate a brief, and expand a
sentence to check its sources. You can reject a brief or inspect the dates and status of earlier
versions. Generation requires a consented provider and uses its budget; see
[person briefs](/docs/usage/people-briefs/)
for setup and supported sources.

The Network tab can request one, two, or three hops and optionally include
ended records. It visualizes at most 250 nodes and 500 connections, while an
always-present list groups the same connections by hop for keyboard and screen
reader use. Person and organization names in this view come from durable
profiles. Edges come only from curated typed relationships and employments
(including shared organizations), never messages, participant co-occurrence,
or inferred communication activity.

Reviews brings together identity matches, fact review, and imported
relationships. Inspect the evidence before accepting or rejecting a candidate.
Conflicts between existing profiles require an explicit merge decision.
Merge history and reversal follow the boundaries documented in
[People](/docs/usage/people/).

Domains provides the same activity-and-files analysis for an exact domain
fact. A domain is not treated as an inferred organization identity. Selecting
a grouped person or domain in Everything opens its inspector in the current
context, including chronologically ordered related files.

## Saved Views

Saved Views persist useful analytical contexts in the daemon, so the same
library is available from every authenticated browser connected to this
single-user archive. A view records its query, explicit search mode, filters,
grouping, presentation, sort, visible columns, and inspector preference.
Selection is intentionally not saved.

Each record carries a schema version. An incompatible record remains visible,
but cannot be opened or edited: automatic migration is not attempted. Remove it
after confirmation and save the current context again. Updates and deletion use
the record revision as an optimistic-concurrency guard. If another browser
changes the view first, msgvault reports a conflict and requires you to reload
and review the latest revision instead of overwriting it.

## Sources and sync status

Sources is a status workspace. For each source it shows schedule information,
an active run's processed, added, and error counts, the latest terminal result,
and the last successful sync separately. A failed status request remains an
error rather than becoming an empty source list. Failed runs expose their
run-level and item-level errors, and a terminal result older than 24 hours is
marked `stale_last_result`.

`Sync now` is available only when that source reports the capability. A `202
Accepted` response means the daemon accepted the request, not that work has
finished. While the page is visible, the UI polls source status with bounded
backoff to show the run and live progress; it opens no streaming connection. If
the accepted run never appears, the UI reports `sync_start_not_observed` rather
than claiming success. Conflicting runs and unavailable capabilities retain
their explicit errors or reasons. Full resync, pause/resume, schedule editing,
and source add/remove are outside this workspace's initial scope.

## Operations

Operations answers two different questions: what is configured and ready now,
and what happened during a particular run. Its overview groups work into five
lanes:

| Lane | Work shown |
|---|---|
| Messages | Source sync and message embeddings |
| Facts | People sweeps, person embeddings, and external enrichment |
| Contacts | CardDAV sync |
| Documents | Document extraction and document embeddings |
| Attachments | Visual embeddings |

Filter history by lane, kind of work, state, and start date. Open a run to
inspect its progress, outcome, timestamps, and available diagnostics. Queued,
running, succeeded, partial, failed, and cancelled are distinct states.
Missing history is reported as unavailable instead of looking like no work
has ever run.

Links open source status, CardDAV settings, or detailed document and visual
index status. The latter show coverage and the current prerequisites for
processing. The workspace offers **Start CardDAV sync**, **Build visual
index**, or **Resume visual index** only when the daemon advertises that
action. Source **Sync now** remains in Sources. Document extraction still
requires the explicit CLI upload workflow in
[Document Indexing](/docs/usage/document-indexing/).

While Operations is visible it refreshes status and run history. The filters
and selected run are kept in the URL, so browser Back and Forward restore the
view. If paging history becomes inconsistent after a change, use **Restart
operation history** to load a fresh snapshot.

## Deletions

Everything supports explicit row selection and select-all-matching for the
current query and filters. `d` and `D` open the Deletions workspace, where the
daemon first preflights the selection and reports any unavailable action before
the UI offers a separate staging confirmation. The workspace lists, inspects,
and cancels manifests; it cannot execute deletion against a provider. Use the
explicit `msgvault delete-staged` CLI workflow for that final operation.

## Keyboard controls

Tab keeps its normal browser meaning. Outside inputs and content viewers:

| Key | Action |
|---|---|
| `j` / `k`, arrows | Move row focus |
| `Home` / `End`, `PgUp` / `PgDn` | Navigate large tables |
| `Enter` | Open or drill into the focused row |
| `Esc` | Close the current shell layer or restore prior context |
| `/` | Focus search |
| `Space` | Toggle the focused row |
| `A` / `x` | Select visible rows / clear selection |
| `d` / `D` | Review deletion staging |
| `f`, `g`, `s`, `r` | Filter, group, sort, reverse sort |
| `?` | Searchable shortcut help |
| `Cmd/Ctrl+K` | Command palette |

Destructive keys open a review; they never execute deletion immediately.
Shortcuts are suspended while typing and inside message/file content.

## Settings and restart behavior

Settings edits supported browser, server, search, source, and integration
settings on the daemon host. The daemon supplies the editable fields and their
allowed values. Saving makes targeted edits to `config.toml` while preserving
comments. A stale edit is rejected after another browser or a hand edit
changes the configuration; reload before saving again.

Keys marked restart-required show a pending-restart state until the daemon
restarts. The server API key (`server.api_key`) is read-only in the browser;
change it in `config.toml` on the daemon host. After that key changes and the
daemon restarts, old browser sessions end and the login screen appears.

### Provider policies and credentials

Create or edit named person-enrichment policies for Exa and SixtyFour in
Settings. Provider checks and consent still govern whether enrichment can
run; configuration alone does not authorize a provider. The TUI shows these
policies read-only. See [External Person Enrichment](/docs/usage/people-enrichment/)
for the provider lifecycle.

Provider credentials for embeddings, enrichment, and sweeps are write-only.
You can add, replace, or remove a key. After saving, the UI shows whether a key
is configured and where it comes from, but never its value.

Credentials have a separate revision from `config.toml`. When changing both
an endpoint or model and its credential, save the endpoint/model first, then
the credential. This binds the key to the destination it was entered for.

### CardDAV contacts

CardDAV settings manage contact accounts and discovered address books. Choose
which books participate in sync, lookup, and publishing; publishing also
enables contact sync for that book. The workspace offers incremental and full
sync, recent run history, and conflict review. A conflict shows local and
remote versions before you choose which to keep. See
[People and CardDAV](/docs/usage/people-carddav/) for setup and publishing rules.

## Optional integration states

The optional task integration is server-side and provider-neutral. Msgvault
shows disabled, discovering, authentication required, reachable but
incompatible, partial, stale, unavailable, or ready instead of presenting a
failed lookup as “no links.” Credentials never enter browser types, URLs, or
error messages. The archive remains fully usable while the integration is
absent or unhealthy.
