# Microsoft Teams Ingestion — Design

Date: 2026-06-18
Status: Implemented on branch feat/teams-ingestion (chats, group chats,
channel posts/replies, meeting chats; transcripts remain deferred to
2026-06-18-teams-transcripts-design.md).

## Goal

Sync and store the user's Microsoft Teams messages — 1:1 chats, group
chats, channel posts/replies, and meeting chats — into msgvault so they
are searchable alongside Gmail/Outlook through the existing TUI, FTS, and
Parquet analytics. Rich text, reactions, @mentions, links, inline images,
and meeting transcripts are preserved. Recordings are referenced by link,
not downloaded.

## Decisions (from brainstorm)

- **Account type:** Work/school (Entra/Azure AD) tenant where the user is
  admin and can register an app and grant admin consent.
- **Data scope:** The user's *own* data only, via **delegated** OAuth.
  No tenant-wide export, so Microsoft's metered Teams export billing is
  not involved.
- **Content scope:** 1:1 & group chats, channel messages (standard +
  private), and meeting chats. **Meeting transcripts are out of scope for
  this build** — moved to a separate spec
  (`2026-06-18-teams-transcripts-design.md`) after the load-bearing pass
  showed delegated transcript content is effectively organizer-only and
  unavailable for expired meetings.
- **Attachments:** Download small inline/pasted images (hosted content)
  into content-addressed storage; for shared SharePoint/OneDrive files
  store filename + link + metadata only (no bytes). Downloading shared
  file bytes is out of scope — tracked in kata `c0gf` (preserve a
  departing user's files before account removal).
- **Acquisition:** Live Microsoft Graph delta sync (Approach A), mirroring
  the existing Gmail live-sync model. Transcripts are sequenced as the
  final phase because they are the most permission-sensitive surface.

## Architecture

```
cmd/msgvault/cmd/sync_teams.go   ← new CLI command(s)
        │
internal/teams/                  ← new package (parallel to internal/whatsapp, internal/gmail)
   ├── client.go      Graph REST client (paging, 429 + Retry-After, $batch)
   ├── sync.go        orchestration: enumerate chats/channels → delta → persist
   ├── messages.go    map Graph chatMessage → store.Message (+ body, recipients, reactions)
   ├── transcripts.go (phase 4) onlineMeetings → transcript text
   └── checkpoint.go  delta-link + cursor persistence
        │
internal/microsoft/oauth.go      ← extended: add Graph delegated scopes + token source
        │
internal/store/...               ← reused as-is (UpsertMessage, EnsureConversationWithType, …)
```

Per sync run: authenticate (delegated token) → list chats + joined
teams/channels → for each conversation run a **delta query** from the
saved delta link → for each `chatMessage` build a `store.Message`,
resolve participants by UPN/email, persist body (HTML + derived text),
reactions, mentions, attachment metadata + inline images → save the new
delta link as the checkpoint.

## Data model mapping

Reuses the existing generic chat schema — **no new core tables**.

| Teams concept | msgvault storage |
|---|---|
| Source (your account) | `sources` row, `source_type = "teams"`, `identifier = your UPN` |
| 1:1 / group chat | `conversations`, `conversation_type = "direct_chat"` / `"group_chat"` |
| Team channel | `conversations`, `conversation_type = "channel"`, `title = "Team / Channel"` |
| Meeting chat | `conversations`, `conversation_type = "group_chat"` (flagged as meeting) |
| `chatMessage` | `messages`, `message_type = "teams"`, `source_message_id = Graph id` |
| Reply (channel thread) | `reply_to_message_id` → root post |
| Sender | `message_recipients` `"from"`; other participants `"to"` |
| @mention | `message_recipients` `"mention"` |
| Rich text (HTML body) | `message_bodies.body_html` + derived `body_text`; FTS indexes text |
| Reactions (👍 ❤️ …) | `reactions` table |
| Inline pasted image | `attachments` (downloaded bytes, content-addressed) |
| Shared SharePoint/OneDrive file | `attachments` row: filename + link + metadata, **no bytes** |
| Raw message JSON | `message_raw`, `raw_format = "teams_json"` (re-parseable) |

Mapping caveats from the load-bearing pass:

- **Identities carry no email inline** (LB-D3). `from`/`mentions` give an
  **AAD object id** + (often null) displayName. Resolution: add an
  `aad_object_id` identifier type to `participant_identifiers`, and resolve
  id → `mail`/`userPrincipalName` via `GET /users/{id}` (best-effort: mail
  can be null, UPN ≠ SMTP always). Branch on `userIdentityType`:
  `aadUser` → resolve; `emailUser` → the id *is* an email (free key);
  `application` (bots/connectors) / `anonymousGuest` / `skypeUser` /
  `azureCommunicationServicesUser` → no email, store id-only.
- **Inline images** need a **separate** `GET .../hostedContents/{id}/$value`
  per image (`contentBytes` is null on the message/list read). Detect them
  by the `hostedContents/{id}/$value` URL pattern (covers `<img>`, custom
  emoji, card images, custom-reaction images), not just `<img src>`.
- **Shared-file attachments** are `chatMessageAttachment` with
  `contentType:"reference"` and a SharePoint `contentUrl`. Filter on
  `contentType == "reference"` so cards/tabs/meeting refs aren't recorded
  as files.

Side effect: once AAD ids are resolved to `mail`, a person's Teams
messages and their Gmail/Outlook mail unify under one `participant`,
enabling cross-platform search of a single human. (The unification depends
on successful id→mail resolution, so it is best-effort for guests/bots.)

## OAuth & permissions (delegated)

Extend `internal/microsoft/oauth.go` (today IMAP-scoped) to also issue a
Graph token. Delegated scopes:

- `Chat.Read` — your 1:1/group/meeting chats and their messages
- `ChannelMessage.Read.All` — messages in channels of teams you belong to
- `Team.ReadBasic.All`, `Channel.ReadBasic.All` — enumerate joined teams/channels
- `User.ReadBasic.All` — resolve Teams AAD user ids to email/display names
- `User.Read` — your identity; `offline_access` — refresh tokens

(Transcript scopes `OnlineMeetings.Read` + `OnlineMeetingTranscript.Read.All`
belong to the separate transcripts spec, not this build.)

One app registered in Entra, admin consent granted once for the `.All`
scopes; client ID/secret placed in config like existing OAuth apps.

> Implementation note (LB, codebase-verified): the existing
> `microsoft.Manager.TokenSource` validates that a token's `Scopes[0]` is
> an IMAP scope and rejects it otherwise. The Graph token path must branch
> around this IMAP-specific validation rather than reuse it unchanged.

### IMAP and Teams are independent

Outlook/IMAP and Teams can each be used alone or together:

- **Separate token files:** IMAP uses `microsoft_<upn>.json` (existing);
  Teams uses `teams_<upn>.json` (new). Each command requests only its own
  scope set, so the consent screen shows only what that feature needs.
- **IMAP-only:** existing Outlook/IMAP `add-account`; `--teams` never
  invoked; no Graph scopes requested or consented.
- **Teams-only:** `add-account --teams` consents to Graph scopes only;
  no IMAP token created.
- **Both:** two independent grants; either can be revoked/re-run without
  affecting the other.

The only shared artifact is the Entra app registration (client ID):

- **(a) One app, incremental consent (default):** the app lists both IMAP
  and Graph permissions as *available*, but each flow requests only its
  subset; a Teams-only user is never prompted for IMAP. One registration
  to administer.
- **(b) Two apps (alternative):** fully separate registrations via the
  existing `[oauth.apps.*]` named-app config. More isolation, more admin
  overhead.

Neither feature requires the other at runtime.

## Incremental sync & checkpointing

> Revised after the load-bearing pass (see findings below). Chats and
> channels use **different** mechanisms — there is no single delta cursor.

**Chats (1:1 / group / meeting).** No delegated per-chat `delta` endpoint
exists (load-bearing A1). Use the list endpoint for both phases:

- **Backfill:** `GET /me/chats/{id}/messages` paginated via
  `@odata.nextLink`. (Residual risk LB-1: docs don't *guarantee* unbounded
  history — validate live.)
- **Incremental:** `GET /me/chats/{id}/messages?$filter=lastModifiedDateTime gt {cursor}&$orderby=lastModifiedDateTime desc`.
  Both `$filter` and `$orderby` **must** target the same property or the
  filter is silently ignored. Cursor = the max `lastModifiedDateTime` seen
  for that chat (a timestamp, **not** an `@odata.deltaLink`).

**Channels.** The list endpoint has **no** date `$filter` (only
`$top`/`$expand`), so the chat approach doesn't transfer:

- **Backfill:** `GET /teams/{team}/channels/{channel}/messages` (roots) +
  `.../messages/{id}/replies` per root (2-level structure). Resumable via
  `@odata.nextLink`.
- **Incremental:** prefer `/messages/delta` (returns `@odata.deltaLink`),
  with a **fallback** to a full re-page + client-side dedupe by
  `(id, lastModifiedDateTime)` if delta proves unavailable under delegated
  consent (residual risk LB-2). Delta tokens can rot (HTTP 400/410) — on
  that error, restart delta from scratch.

**Checkpoint model.** `sync_checkpoints` / `cursor_before` JSON stores a
per-conversation cursor that is **either** a timestamp (chats) **or** a
deltaLink/nextLink (channels) — a small tagged-union, not a uniform delta
link. Honor 429 + `Retry-After` like the Gmail rate limiter. Reuse
`sync_runs` for resumable backfill, matching the fbmessenger/whatsapp
importers.

**Edits/deletes.** Edits bump `lastModifiedDateTime` (caught by the chat
filter / channel delta) → `UpsertMessage` ON CONFLICT updates the row.
Deletes carry `deletedDateTime`; map to the existing soft-delete column.
Delta delete semantics under delegated consent are unverified — treat
delete capture as best-effort.

## CLI & daemon integration

- `msgvault add-account <upn> --teams` — delegated browser OAuth for Graph scopes.
- `msgvault sync-teams <upn>` — full/incremental (auto-detected via stored
  per-conversation cursors); `--after` / `--limit` for scoped first runs,
  mirroring `sync-full`.
- Hooks into `msgvault serve` scheduled syncs so it runs alongside Gmail.
- Parquet cache / TUI / search require **no changes**: Teams messages flow
  through the same `messages`/FTS path and are immediately searchable and
  account-filterable.

## Transcripts — moved to a separate spec

Transcripts are **not** part of this build. The load-bearing pass found the
delegated transcript surface materially more constrained (organizer-only
content, expired-meeting gaps, an indirect `joinWebUrl` resolve step), so
it gets its own design: `2026-06-18-teams-transcripts-design.md`
(kata for that work tracked separately). The validated resolution path and
coverage limits are recorded there.

## Testing

- Table-driven tests with testify (`assert`/`require`), per project
  conventions — no new `t.Errorf`/`t.Fatalf`.
- Unit tests over **recorded Graph JSON fixtures** (synthetic, no real
  PII) for the `chatMessage` → `store.Message` mapping.
- e2e-style test running a fake Graph HTTP server through the full
  sync → store → search path.
- Live Graph validated manually against the tenant during the
  `/load-bearing` pass.

## Load-bearing findings (validated 2026-06-18, Microsoft Learn docs)

**Confirmed:** `/me/chats` lists all chat types under `Chat.Read`;
delegated own-data reads are **not metered** (export APIs de-metered
2025-08-25); channel list + `/replies` give full history under
`ChannelMessage.Read.All`; private channels readable by members; teams
enumerable via `/me/joinedTeams` + `/channels`; transcript fetch path,
VTT format and scopes confirmed; inline images via `hostedContents`;
reactions/mentions/attachments inline with message read; shared files via
`contentType:"reference"`.

**Falsified vs. the original spec (corrected above):**

- **A1** — no delegated per-chat `delta`; chats use list + `lastModifiedDateTime`
  filter (with matching `$orderby`).
- **B2** — channel delta caps at ~8 months; backfill must use list + replies.
- **C1** — no chat→meeting nav and no meeting id on the chat; resolve via
  `joinWebUrl` filter.
- **D3** — identities carry AAD id, not email; need `/users/{id}` resolution.

**Residual risks — RESOLVED via live Graph Explorer probe (2026-06-19,
delegated, tenant "Ontempo NZ"):**

- **LB-1 — VERIFIED.** `GET /me/chats/{id}/messages?$filter=createdDateTime lt 2025-09-01T00:00:00Z&$orderby=createdDateTime desc`
  returned messages older than the ~8-month delta window (HTTP 200). The
  per-chat list serves full history under delegated `Chat.Read`, so chat
  backfill via the list endpoint is sound.
- **LB-2 — VERIFIED.** `GET /teams/{team}/channels/{channel}/messages/delta`
  returned HTTP 200 with an `@odata.nextLink` under delegated
  `ChannelMessage.Read.All` (the terminal `@odata.deltaLink` arrives after
  paging through `nextLink` — standard delta behavior). The delegated delta
  endpoint works; channel incremental sync can use it directly. (The
  list+re-page fallback remains in the design for delta-token rot / 400-410
  recovery.)

Consent learnings for the Entra app: enumerating channels needs
`Channel.ReadBasic.All` (in addition to `Team.ReadBasic.All` for teams);
`ChannelMessage.Read.All` is consented per the message/delta endpoints.

(LB-3, delegated transcript access, moved with transcripts to the separate
spec.)

## Out of scope

- Meeting transcripts — separate spec `2026-06-18-teams-transcripts-design.md`.
- Tenant-wide / compliance export (metered app-only Graph APIs).
- Downloading shared SharePoint/OneDrive file bytes — kata `c0gf`.
- Downloading meeting recording video.
