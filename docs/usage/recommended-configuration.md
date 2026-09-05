---
last_edited: "2026-09-03"
title: Recommended Configuration
description: The config.toml that `msgvault setup providers` writes from the API keys you have, section by section.
---

Msgvault has four retrieval lanes (message text, curated people, visual
attachments, document attachments), a people sweep that keeps profiles
current, a scheduled activity projection, and a media policy for chat
sources. Each shipped as opt-in with its own table, key variable, consent
step, and build command. `msgvault setup providers` chooses recommended
values for all of them from the keys in your environment and turns on the
lanes those keys support, one explicit consent per hosted provider.

```bash
export VOYAGE_API_KEY="..."      # text, people, and visual search
export MISTRAL_API_KEY="..."     # document attachments
export OPENAI_API_KEY="..."      # people sweep (and text search when no Voyage key)

msgvault setup providers --dry-run   # show the plan and each provider disclosure
msgvault setup providers             # answer once per provider, write config.toml
msgvault setup status                # what is on, what is off, and why
```

Hosted lanes never turn on from a key alone. Setup asks before it writes,
records the postures you assert, runs the people-provider check and consent
through the same gates the `person provider` commands enforce, and prints the
exact commands that finish the lanes it cannot complete on its own (the two
provider probes need private synthetic seed files). Re-running setup after
adding a key upgrades only the lanes that are still unset; a configured lane
keeps its model, because switching the embedding policy invalidates the
index and is your call.

Every value below is settable per lane exactly as before. This page only
describes what happens when nothing is set.

## What each key turns on

| Key present | Lanes | Model | Notes |
|---|---|---|---|
| `VOYAGE_API_KEY` | text search, semantic people search, visual attachments (after the probe) | `voyage-context-4` (1024), `voyage-multimodal-3.5` (1024) | Chats embed as conversation windows and meetings as turn-aware chunks; email rides the same generation. |
| `MISTRAL_API_KEY` | document extraction and lexical search; document vectors when a text lane is on | `mistral-ocr-4-0`, EU region | Uploads are manual-only and need the probe manifest plus `documents consent-mistral --yes`. |
| `OPENAI_API_KEY` | people sweep; text search only when no Voyage key | `gpt-5.6-luna` at `medium` reasoning; `text-embedding-3-small` (1536) | The OpenAI text path gives per-message vectors: no conversation-window context and no visual lane, both are Voyage-only endpoints. |
| none | local Ollama at `[chat].server` when reachable | `nomic-embed-text` (768); the `[chat].model` for the sweep | Text stays on your machine. Setup skips a lane the server cannot serve and says why. |

## The file setup writes

With a Voyage key, a Mistral key, and an OpenAI key present, setup writes
the sections below into an otherwise empty `config.toml`. Comments and
sections you already have are preserved.

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
run_after_sync = true            # embed after every successful scheduled sync
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

[attachments.documents]
enabled = true
retention_posture = "standard"   # or "zdr"; --document-retention
training_posture = "default-opt-out" # or "opted-out"; --document-training

[attachments.documents.index.embeddings]
enabled = true                   # document chunks use the text-search profile after `documents vectors consent`

[people.sweep]
enabled = true
provider = "openai"

[people.sweep.providers.openai]
protocol = "openai_chat"
endpoint = "https://api.openai.com/v1"
model = "gpt-5.6-luna"
auth = "bearer"
credential = "env"
credential_env = "OPENAI_API_KEY"
output_mode = "native_json_schema"
token_limit_parameter = "max_completion_tokens"
reasoning_effort = "medium"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text", "meeting_text", "document_text"]
source_since = "2025-01-01"      # January 1 of last year
allow_sensitive = true
request_timeout = "1m0s"
```

### `[vector]` and `[vector.embeddings]`

The text lane. `api_format = "voyage-contextual"` pins `voyage-context-4`
and sends each chat conversation window and each meeting as one contextual
request, so a message is embedded with its neighbors. The OpenAI-compatible
format (`api_format = "openai"`) embeds each message on its own. Message
text leaves the machine either way; setup states that before it asks.

`run_after_sync` covers Gmail, IMAP, Teams, and Discord syncs. The cron
covers Slack, Beeper, calendar, and meeting sources, which do not trigger a
post-sync embed. See [Vector Search](/usage/vector-search/).

### `[vector.people]`

Semantic people search embeds one curated document per durable person
(searchable, non-sensitive attributes only) into the same generation, so a
query like "a finance contact in Berlin" returns the person. The postures are
your assertion about the embedding provider; setup records
`provider-declared` unless you pass `--retention-posture` and
`--training-posture`. Consent is a separate step:
`msgvault person provider consent --semantic-embeddings --yes`.

### `[vector.multimodal]`

The visual lane needs a capability manifest from an authenticated probe of
Voyage with private synthetic fixtures, and the probe needs four seed files
you supply (a WebP and an MP4, each with a contrasting variant). Enabling the
lane without the manifest makes the daemon refuse every vector lane, so setup
writes only the schedule until the manifest exists:

```bash
msgvault multimodal probe --seeds <private-seed-dir> --out ~/.msgvault/voyage-capabilities.json --yes
msgvault setup providers        # now enables [vector.multimodal] with that manifest
msgvault daemon restart
msgvault multimodal build --yes # consent to exactly that capability profile
```

### `[attachments.documents]`

Mistral is the only document provider and receives the complete original
bytes of standalone document attachments. Setup records the least-asserting
legal postures (`standard` retention, `default-opt-out` training) unless you
pass `--document-retention zdr` or `--document-training opted-out`; use the
values your account actually has. Uploads stay manual: build the fixture
matrix, probe, consent, then build. See
[Document Attachment Indexing](/usage/document-indexing/).

```bash
msgvault documents probe-mistral --fixtures <private-fixture-dir> > ~/.msgvault/mistral-capabilities.json
msgvault documents consent-mistral --capabilities ~/.msgvault/mistral-capabilities.json --yes
msgvault documents build --capabilities ~/.msgvault/mistral-capabilities.json --yes
msgvault documents vectors consent --yes     # when document vectors are enabled
```

### `[people.sweep]`

The sweep keeps curated attributes current from the archive for people you
track (`msgvault person track <person-id>`). Deterministic contact state
(last contacted, cadence, inferred channel) refreshes hourly for everyone
through `[activity]` and needs no model. Setup onboards the `openai` profile
through `person provider add` (a synthetic check request is sent), records
consent, and selects it; the daily schedule is the `[people.sweep]` default.
`allow_sensitive = true` is required for real sweeps because every evidence
packet is marked sensitive. The Codex app-server adapter is release-gated and
cannot be the default. With no OpenAI key, setup offers a loopback Ollama
profile on `[chat].model`.

### `[activity]`

On by default (`17 * * * *`, UTC). It projects archived messages into dated
per-person contact state. Nothing to configure; see
[Configuration](/configuration/#activity).

### Media policy

Chat sources cap collection by conversation size: media from rooms above 20
participants is skipped with a typed `participant_threshold` marker, direct
and small-group media is kept. Set `media_max_participants = 0` on a source
to lift the cap. See the `[beeper]`, `[slack]`, `[discord]`, and `[teams]` sections of [Configuration](/configuration/#beeper).

## What the MCP server answers with these defaults

The people tools (`search_people`, `get_person_notes`,
`get_person_relationship`, `search_person_files`) read local derived state
and are on regardless of provider keys. The text lane adds
`semantic_search_messages` and `find_similar_messages`; the visual lane adds
`search_visual_attachments`; the document lane adds
`search_document_attachments`. `msgvault setup status` prints the live list.

## Reading the status report

```text
LANE                                      STATE    PROVIDER  MODEL             CONSENT  SCHEDULE
Text search (messages, chats, meetings)   on       voyage    voyage-context-4  -        cron */15 * * * *, after each scheduled sync
Semantic people search                    on       voyage    voyage-context-4  missing  -
Visual attachment search                  pending  -         -                 -        -
Document attachments (...)                on       mistral   mistral-ocr-4-0   missing  -
```

`pending` means the lane is configured or the key is present but an
operator step remains; the `next` line under the table names it. `unknown`
consent means the archive could not be read (for example, the database does
not exist yet). Use `--json` for scripting.
