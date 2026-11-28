---
last_edited: "2026-10-29"
title: Recommended Configuration
description: Set up optional search and people features from your available provider keys, then check what still needs attention.
---

Run `msgvault setup providers` to configure optional search and people features
using the provider keys you already have. Setup proposes defaults, tells you
what data each provider receives, and asks for your consent before saving
anything. You do not need provider keys for keyword search or normal archive
browsing.

In setup's output, a **lane** is one optional feature: message embeddings,
semantic people search, visual attachments, or people sweeps. Each lane has its
own readiness and consent checks.

```bash
export VOYAGE_API_KEY="..."      # text, people, and visual search
export OPENAI_API_KEY="..."      # text search when no Voyage key

msgvault setup providers --dry-run   # show the plan and each provider disclosure
msgvault setup providers             # answer once per provider, write config.toml
msgvault setup providers --provider openai --model <model> \
  --credential-env OPENAI_API_KEY --retention-posture <assertion> \
  --training-posture <assertion> --allow-sensitive # explicit people inference choice
msgvault setup status                # what is on, what is off, and why
```

If a feature still needs a capability probe, consent, or an index build, setup
prints the follow-up commands for you. Run `setup status` to check those
prerequisites and see the next step. Confirming a provider during setup does not
replace those separate consents, and having a key by itself never starts hosted
processing.

## What starts without provider setup

A fresh archive gives you keyword search, browsing, saved people, and local
contact history without any provider. The daemon refreshes contact activity
every hour. Chat imports collect media by default within the room and file size
limits described below, though each source still needs its own authorization and
sync setup.

Message embeddings, semantic people search, visual search, people sweeps, and
external enrichment start out turned off. Setup turns on the eligible features
described below; it does not configure external enrichment, authorize a source,
choose people to track, or enroll anyone in conversation briefs.

## Defaults selected from your keys

| Key present                             | Lanes                                                                           | Model                                                                               | Notes                                                                                                                                                                                  |
| --------------------------------------- | ------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `VOYAGE_API_KEY`                        | text search, semantic people search, visual attachments (after the probe)       | `voyage-context-4` (1024), `voyage-multimodal-3.5` (1024)                           | Beeper chats use conversation windows; meetings use speaker turns. Other messages share the generation as individual documents.                                                        |
| `OPENAI_API_KEY`                        | text search only when no Voyage key      | `text-embedding-3-small` (1536)               | The OpenAI text path gives per-message vectors and does not enable visual search. Configure a visual provider separately.                                                     |
| no key for the feature being configured | text: Ollama with `nomic-embed-text`, then the shared local server; sweep: loopback Ollama at `[chat].server`                               | `nomic-embed-text` (768); the `[chat].model` for the sweep with `--allow-sensitive` | Text uses Ollama only without Voyage or OpenAI keys. Inference uses it without an OpenAI key, even when Voyage or Mistral is configured. The Ollama model must already be installed. Setup asks before downloading missing files for the shared local server. |

These defaults apply during setup, not as runtime failover. Adding a Voyage key
does not replace an existing OpenAI text index, and an unavailable inference
provider does not make a sweep switch providers. Setup does not use a signed-in
Codex account, as the
[Codex app-server transport](../configuration.md#codex-app-server-profiles)
remains release-gated. If the selected API or local model is not the one you
want, configure another supported inference profile explicitly.

## The file setup writes

When both Voyage and OpenAI keys are present, running the explicit OpenAI setup
command above with `<model>` set to `gpt-5.6-luna` and both privacy assertions
set to `provider-declared` produces the configuration below. This example
assumes SQLite, no probe manifests yet, and a people-provider check that
negotiates native JSON output. Both the saved output mode and token-limit field
come from that check. Setup preserves any comments and sections you already have in
the file.

```toml
[vector]
enabled = true
backend = "sqlite-vec"          # "pgvector" when [data].database_url is PostgreSQL

[vector.embeddings]
api_format = "voyage-contextual" # conversation windows and turn-aware meeting chunks
endpoint = "https://api.voyageai.com/v1"
api_key_env = "VOYAGE_API_KEY"
model = "voyage-context-4"
dimension = 1024

[vector.embed.schedule]
run_after_sync = true            # embed after supported scheduled source syncs
cron = "*/15 * * * *"            # and catch up chat sources that do not trigger a post-sync pass

[vector.people]
enabled = true                   # one curated, non-sensitive document per person
retention_posture = "provider-declared"
training_posture = "provider-declared"

[vector.multimodal.schedule]
run_after_sync = true
cron = "*/15 * * * *"
# [vector.multimodal] enabled = true and capabilities_file are written once the
# probe manifest exists at <home>/voyage-capabilities.json.

[people.sweep]
enabled = true
provider = "openai"

[people.sweep.providers.openai]
preset_id = "openai"
protocol = "openai_chat"
endpoint = "https://api.openai.com/v1"
model = "gpt-5.6-luna"
auth = "bearer"
credential = "env"
credential_env = "OPENAI_API_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text", "meeting_text"]
source_since = "2025-01-01"      # January 1 of last year
allow_sensitive = true
request_timeout = "1m0s"
```

To switch text search to the shared local server, run
`msgvault setup providers --local-embeddings`. Keeping the same Nomic message
recipe preserves your existing index, while switching to another model requires
a full rebuild. See [local embedding setup](vector-search.md) for separate
document-vector and person-consent behavior.

### `[vector]` and `[vector.embeddings]`

These sections configure the text lane. Setting `api_format = "voyage-contextual"`
pins `voyage-context-4` and sends each Beeper conversation window and each
meeting as one contextual request, so the model embeds each message with its
neighbors. Other chat sources and email remain individual documents. With the
OpenAI-compatible format `api_format = "openai"`, each message is embedded on
its own. Hosted configurations send message text to that provider, while a
loopback Ollama endpoint processes it locally. Setup states the destination
before asking for confirmation.

The recommended text scope covers all message types and accounts. If you want
only part of your archive to reach the embedding provider, narrow
[`[vector.embed.scope]`](../configuration.md#vectorembedscope) before starting
the daemon. Text embeddings have no separate stored consent. After setup enables
the schedule, the daemon can send eligible text when it loads the new settings.

`run_after_sync` covers Gmail, IMAP, Teams, and Discord syncs. The cron schedule
covers Slack, Beeper, calendar, and meeting sources, which do not trigger a
post-sync embed. See [Vector Search](/docs/usage/vector-search/).

### `[vector.people]`

Semantic people search embeds one curated document per durable person into the
same generation, using searchable, non-sensitive attributes only, so a query
like "a finance contact in Berlin" returns that person. The posture fields
record your assertion about the embedding provider; setup writes
`provider-declared` unless you pass `--retention-posture` and
`--training-posture`. Giving consent is a separate step with
`msgvault person provider consent --semantic-embeddings --yes`. See
[semantic person search](/docs/usage/people/#find-a-person-by-what-you-remember)
for the build and search workflow.

### `[vector.multimodal]`

The visual lane needs a capability manifest from an authenticated probe of
Voyage using private synthetic fixtures. To run the probe, supply four seed
files: a WebP and an MP4, each with a contrasting variant. Enabling the lane
without the manifest makes the daemon refuse every vector lane, so setup writes
only the schedule until the manifest exists:

```bash
msgvault multimodal probe --seeds <private-seed-dir> --out ~/.msgvault/voyage-capabilities.json --yes
msgvault setup providers        # now enables [vector.multimodal] with that manifest
msgvault daemon restart
msgvault multimodal build --yes # consent to exactly that capability profile
```

### `[people.sweep]`

The sweep maintains profile facts from archive evidence for people you track
with `msgvault person track <person-id>`. Deterministic contact state, including
last contacted, cadence, and inferred channel, refreshes hourly for everyone
through `[activity]` and needs no model. When you pass `--provider openai`,
setup onboards the `openai` profile through `person provider add`, and a
synthetic check request is sent during that step. Setup then records consent and
selects that profile. The default
sweep schedule runs daily at `02:15` in the daemon's time zone, and setup
preserves any saved sweep schedule. Real sweeps require `allow_sensitive = true`
because every archive evidence packet is marked sensitive. Hosted setup requires
an explicit `--allow-sensitive=true|false` decision; `false` allows setup and
synthetic checks, while blocking real sweeps. That same flag permits inferring
sensitive attributes, and there is no separate setup flag to allow private
source text while excluding sensitive targets. If you do not have an OpenAI key,
setup offers a loopback Ollama profile on `[chat].model`.

Before you track anyone, review the selected sources and date range. Setup
includes conversation and meeting text from January 1 of the previous year,
keeps the default request and token budgets, and does not add provider prices or
monetary caps. See [sweep budgets](../configuration.md#peoplesweepbudgets) and
[Profile Automation](people-automation.md) to review inferred facts, adjust the
policy, or change a provider.

Conversation briefs require a separate choice for each person:

```bash
msgvault person brief enroll <person-id> --track
msgvault person brief generate <person-id>
```

Enrollment and tracking do different things. Tracking a person alone does not
enable a brief, and enrolling someone requires tracking or the explicit
`--track` flag. The daily sweep refreshes eligible briefs, normally once their
current version is seven days old, while manual generation bypasses that
interval. Briefs use supported chat and text-message evidence, not email,
meetings, or documents. See [Conversation Briefs](people-briefs.md) for evidence
limits and recovery.

### `[activity]`

The `[activity]` job is on by default on the `17 * * * *` schedule. It projects
archived messages into dated per-person contact state using UTC day boundaries
by default. You do not need to configure anything here; see
[Configuration](/docs/configuration/#activity).

### Media policy

Chat sources cap media collection by conversation size. Direct and small-group
media is kept, while media from rooms with more than 20 participants is skipped
with a typed `participant_threshold` marker. To lift the cap on a source, set
`media_max_participants = 0`. The per-file default is 250 MiB for Beeper,
Matrix, Slack, Teams, and iMessage, and 50 MiB for Discord. These limits also
apply to existing configurations that omit the keys, and setup does not need to
write them. Explicit values and per-account overrides keep their configured
policy. See [Media policy](../configuration.md#media-policy).

## What the MCP server answers with these defaults

The daemon-backed MCP server reads saved people and local derived state without
calling any provider:

| Reader question                       | Tool and source                                                                                                                                                                                                  |
| ------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Who is this, and how do I reach them? | `get_person_profile`: curated non-sensitive attributes, current employment, relationships, contact points, preferred channel, and inferred channel. Private Notes and sensitive attributes are excluded.         |
| When did we last talk?                | `get_person_profile`: dated contact state from the activity projection.                                                                                                                                          |
| What did we talk about?               | `get_person_profile`: the current brief under `last_talked.brief`, or `null` when no brief is available. Enrollment and successful generation are required first. Reading the profile does not generate a brief. |
| Who was active recently?              | `list_directory_people`: saved people ordered by latest contact by default, with date filters and pagination. Observed contacts without a saved person remain in `search_people`.                                |

Optional retrieval tools include `semantic_search_messages`,
`find_similar_messages`, `search_visual_attachments`, and `search_media`. Their
queries need the corresponding configured feature, consent, and usable index.
The final MCP summary that setup prints is a partial list derived from
configuration flags, not the server's live tool catalog or an index-readiness
check. See [MCP Server](chat.md) for the full tool contract.

## Existing settings and pending setup

Hosted lanes never turn on from a key alone. Setup asks before writing changes,
records the postures you assert, runs the people-provider check and consent
through the same gates that `person provider` commands enforce, and prints the
exact commands to finish the lanes it cannot complete on its own. The provider
probe needs private synthetic seed files. If you run setup again after adding a
key, it upgrades only the lanes that are still unset. A configured lane keeps
its model, because switching the embedding policy invalidates the index and is
your call.

If visual search is already enabled and `capabilities_file` is unset, setup
validates `<home>/voyage-capabilities.json` and offers to save that path without
replacing an explicit custom path. Status stays pending until the path is saved
and the remaining consent and readiness checks pass.

When setup enables a disabled lane, it preserves saved retention and training
postures and fills in defaults only for unset values. To replace the
corresponding people-search posture, pass `--retention-posture` or
`--training-posture`. Already enabled lanes with known postures remain
unchanged. Setup treats `unknown` as unset rather than as an assertion, and
fills it with the disclosed default or the corresponding explicit flag.

Setup also preserves each explicit `cron` and `run_after_sync` setting for text
and visual embeddings, including `cron = ""` and `run_after_sync = false`. Only
absent keys receive schedule defaults. If embedding credentials are missing,
text search and its dependent people-search lanes stay pending in the status
report.

Configured vector lanes also stay pending if your binary lacks the backend that
the archive database requires, and `setup status` includes the rebuild command.

Consent-gated lanes stay pending until their required consents are active,
including when the consent records cannot be read. Visual consent must match
both the current configuration and the capability-manifest policy; if either
changes, run `msgvault multimodal build --yes` again. Local Ollama setup clears
any old `api_key_env` setting because its selected loopback endpoint does not
require authentication.

For existing text endpoints, setup recognizes the exact OpenAI and Voyage API
hosts over HTTPS and loopback servers. Other hosted endpoints count as custom.
Configure their people-search lanes explicitly, then review the separate consent
commands for the new data those endpoints receive.

Local people-sweep setup stays pending without `--allow-sensitive`, even if you
pass `--yes`. Hosted setup also requires an explicit preset, model, credential
source, and retention and training assertions. The sensitive-content flag
permits sending archive excerpts to the provider and inferring sensitive
personal attributes, and the plan describes this policy in both human and JSON
output.

## Reading the status report

`setup status` reads your local configuration, process environment, stored
credentials, and archive consent records without contacting a provider. An `on`
state means those setup checks passed. To inspect build progress and coverage,
use the index's status command or open
[Settings Overview](/docs/web-ui/#settings-and-restart-behavior).

```text
LANE                                           STATE    PROVIDER  MODEL             CONSENT  SCHEDULE
Search by meaning (messages, chats, meetings)  on       voyage    voyage-context-4  -        cron */15 * * * *, after each scheduled sync
Semantic people search                         pending  voyage    voyage-context-4  missing  -
Visual attachment search                       pending  -         -                 -        -
```

A `pending` state means the lane is configured or the key is present, and an
operator step remains; the `next` line under the table names it. An `unknown`
consent state means the archive could not be read, for example when the database
does not exist yet. Hosted visual and people-sweep lanes also show `pending`
when required credentials are missing.

Text and visual search accept both environment keys and stored provider
credentials bound to the configured provider and endpoint. Unreadable credential
stores and endpoint mismatches leave these lanes pending, matching daemon
startup. People-sweep status also validates the active profile's stored
credential. If the key is missing, unreadable, or incompatible, the lane stays
pending and shows recovery guidance. These checks do not create credential
directories or change permissions; to recover, restore the credential or add and
select a replacement profile. Pass `--json` for scripting.
