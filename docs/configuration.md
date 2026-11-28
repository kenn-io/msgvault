---
last_edited: "2026-11-19"
title: Configuration
description: Configuration file reference, environment variables, and file locations.
---

## Config file

msgvault looks for `config.toml` in a platform-specific default location:

| Platform | Path |
|---|---|
| **macOS / Linux** | `~/.msgvault/config.toml` |
| **Windows** | `C:\Users\<you>\.msgvault\config.toml` |

To override the data directory, set the `MSGVAULT_HOME` environment variable or pass the `--home` flag.

For your first archive, add only the sections your source needs. For optional
provider setup, see [recommended configuration](usage/recommended-configuration.md).
The [complete example](#example-configuration) at the end of this page shows
the available sections, though you do not need all of them to start.

### Validation

Invalid TOML, wrong-typed values, and invalid values in sections that CLI
commands read directly cause every command to fail. Invalid values in settings
that only the daemon reads behave differently and do not fail every command.
Those daemon-only settings are `[server]`, `MSGVAULT_API_PORT`, and
`MSGVAULT_CORS_CREDENTIALS` (excluding `daemon_auto_start`,
`daemon_auto_restart`, and the other `MSGVAULT_*` overrides), `[web]`,
`[integrations.*]`, `[carddav]`, `[carddav_connections]`,
`[people.identity_scoring]`, `[people.tiers]`, `[sync]`
(`imap_max_commands_per_minute` and `imap_skip_folders`), the `phone` of
enabled `[[whatsapp_apple]]` entries, `[[imap.drafts]]`, `[[gmail.drafts]]`,
`[[beeper.drafts]]`, and `[[fastmail]]`.

When a command encounters an invalid value in one of those daemon-only settings,
it still runs and prints `Warning: invalid daemon config: ...` to stderr after
finishing. For example, if you set `MSGVAULT_API_PORT=invalid`, `msgvault stats`
prints the warning and still talks to the running daemon, including an older
compatible one, which msgvault never replaces while the config is invalid.

These cases stay strict:

- `msgvault serve`, `msgvault daemon start`, and `msgvault daemon restart` fail
  with the config error before stopping or starting anything.
- If no usable daemon is running, on-demand startup fails with the config error
  instead of launching a daemon.
- Saving configuration is rejected while the file on disk cannot load cleanly.
- The daemon's add-account API returns the reason when it refuses a request, and
  `msgvault add-synctech-sms-drive` fails at startup.
- `msgvault update` also stays strict.

## Remote deletion consent

Starting in v0.20.0, remote deletion is permanently opt-in. You can enable it
durably in your CLI config with `[deletion] remote_enabled = true`, or for a
single command by setting `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both mechanisms
are permanent, with no planned automatic removal of this guardrail.

Consent belongs to the CLI that invokes the command. When you run a command
against a remote daemon, the CLI forwards its effective consent for that
operation. The remote daemon's own `[deletion]` section does not act as server
policy for commands invoked from elsewhere. Staging, listing, inspecting, and
dry-running deletion batches do not require consent.

## Choose optional processing

If you want a guided setup, start with
[recommended configuration](usage/recommended-configuration.md), then come back
here for exact keys and defaults. Each processing feature has its own scope and
consent contract.

The defaults in this reference apply when you omit a key, and they differ from
what `setup providers` writes after you confirm. By default, embeddings and
people sweeps start disabled, while `setup providers` can enable them and add
schedules. Any explicit values you already set stay in effect, and adding
provider keys alone never turns on processing.

- [Profile automation](usage/people-automation.md): tracked people, sweep
  providers, budgets, and fact resolution.
- [Conversation briefs](usage/people-briefs.md): enrolled people and versioned
  summaries through the sweep provider.
- [External enrichment](usage/people-enrichment.md): Exa/SixtyFour policy setup,
  exact consent, request limits, and the persistent suppression key.
- [Vector search](usage/vector-search.md): text, person, and visual indexes.

## People identity scoring

Identity scoring is disabled by default. You start each batch manually and
consent to the exact disclosure printed by `msgvault person scoring status`.
Scoring creates review suggestions and records judgments, and it never accepts
matches or links participants.

```toml
[people.identity_scoring]
enabled = false
model_id = "jev-1.13.0"
minimum_probability = 0.80
credential_env = "MSGVAULT_JEV_API_KEY"
batch_size = 20
retention_declaration = "provider retention policy accepted by the operator"
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable consented identity scoring. Consent is still required. |
| `model_id` | `jev-1.13.0` | Fixed provider model identifier. |
| `minimum_probability` | `0.80` | Probability must be strictly greater than this threshold before local policy can propose acceptance. Accepted values are at least `0.80` and less than `1.00`. |
| `credential_env` | empty | Name of the environment variable holding the provider key. The key value is read from the daemon environment and is never stored in `config.toml`. Required when enabled. |
| `batch_size` | `20` | Default and maximum scoring batch size, from 1 through 100. |
| `retention_declaration` | empty | Operator's exact declaration of the provider retention policy. Required when enabled and included in the consent fingerprint. |

Scoring always uses the fixed endpoint `https://api.typesafe.ai/v1/systemone`
and model `jev-1.13.0`. The consent disclosure binds that endpoint, model,
packet schema, retention declaration, policy version, and question version. If
any of those change, you must consent to the new fingerprint. Running
`msgvault person scoring status` also prints the raw
identity fields and limits covered by the packet schema.

To withdraw consent for a disclosure, run
`msgvault person scoring revoke <fingerprint>`. Revoking consent leaves scoring
enabled in `config.toml` and keeps prior judgments. To turn scoring off in your
config, set `enabled = false`. For the full workflow, see
[identity scoring](usage/people.md#optional-identity-scoring), and for HTTP
endpoints, see [the API reference](api-server.md#identity-match-review-and-scoring).

## People tiers

You can run `msgvault person tiers preview` without adding any configuration.
Add `[people.tiers]` only when you want to adjust which saved people it lists.
Tier 1 captures sustained two-way contact in the short window, while tier 2
captures two-way contact in the long window or meetings.

Contact counts include meetings, sent or received email and chat messages, and
Synctech and Google Voice calls that stayed connected for at least
`min_call_seconds`. Each counted call adds one message in each direction,
regardless of who dialed. Drafts, texts that never left your phone, missed
calls, voicemails, and shorter calls do not count.

If your archive was imported before call and unsent-text filtering shipped, it
continues counting unsent texts and skips calls until you re-import the backup.
For Synctech, run `msgvault import-synctech-sms <path> --owner-phone <number>`,
and for Google Voice, run the Google Voice Takeout import again. Because a Drive
source does not re-download files it already imported, download the Synctech
backup from your Drive folder and run the import locally with the source's owner
phone. Re-importing updates the existing messages in place.

Group chats at or under `[activity] max_direct_counterparts` count for each
member, while messages sent to more people than that limit are excluded.
Newsletter and mailing-list messages are also excluded by default unless you set
`exclude_list_messages = false`. Anyone whose address matches one of your own
addresses in any account is never listed.

```toml
[people.tiers]
short_window_days = 90
long_window_days = 365
exclude_list_messages = true
min_call_seconds = 120

[people.tiers.tier1]
min_outbound_short = 2
min_inbound_short = 2
min_active_days_short = 6
breadth_min_active_days_short = 3
breadth_min_channels_long = 2
breadth_min_meeting_days_long = 2

[people.tiers.tier2]
min_outbound_long = 1
min_inbound_long = 1
min_active_days_long = 3
min_meeting_days_long = 2
```

| Key | Default | Description |
|---|---|---|
| `short_window_days` | `90` | Days counted for tier 1. At least 1 and at most `long_window_days`. |
| `long_window_days` | `365` | Days counted for tier 2. At most 3650. |
| `exclude_list_messages` | `true` | Ignore messages that carry a mailing-list ID. |
| `exclude_address_patterns` | `noreply`, `notifications`, `alerts`, and similar | Lower-case local parts of automated addresses. A trailing `*` matches a prefix. A person whose addresses all match, and who has no phone or chat identity, is never listed. |
| `exclude_domain_patterns` | `resource.calendar.google.com` | Lower-case domains of rooms and resources. A person whose addresses all match is never listed. See [People's room exclusions](/docs/usage/people/). |
| `min_call_seconds` | `120` | How long an answered Synctech or Google Voice call must stay connected to count. Each counted call adds one to the outbound and inbound counts and is its own channel. `0` stops calls from counting. |
| `tier1.min_outbound_short`, `tier1.min_inbound_short` | `2`, `2` | Messages you sent to the person and they sent to you in the short window, plus counted calls. Both are required, so one-way mail never qualifies. |
| `tier1.min_active_days_short` | `6` | Distinct days with contact in the short window. With the two counts above, this alone makes tier 1. |
| `tier1.breadth_min_active_days_short` | `3` | Days needed for the breadth path to tier 1. |
| `tier1.breadth_min_channels_long` | `2` | Message channels (email, chat, calls) in the long window for the breadth path. Meetings count under the next key instead. `0` turns it off; `1` is invalid. |
| `tier1.breadth_min_meeting_days_long` | `2` | Meeting days in the long window for the breadth path. `0` turns it off. |
| `tier2.min_outbound_long`, `tier2.min_inbound_long` | `1`, `1` | Two-way message counts in the long window, plus counted calls. |
| `tier2.min_active_days_long` | `3` | Distinct days with contact in the long window. |
| `tier2.min_meeting_days_long` | `2` | Meeting days that alone make tier 2. `0` turns it off. |

Tier counts come from the activity projection, with windows ending on today's
date in the projection's timezone and the large-audience cutoff using its direct
limit. If you change `[activity] timezone` or `max_direct_counterparts`, counts
keep using the stored projection values and the commands print a warning until
you rebuild the projection with `msgvault activity build`. See
[choose people to maintain](usage/people-automation.md#choose-people-to-maintain).

## People sweep inference

People sweeps use one named protocol profile at a time. Each profile records the
exact endpoint, model, wire protocol, negotiated output mode, privacy posture,
and source scope. Built-in OpenAI, OpenRouter, and Venice presets fill in the
protocol, endpoint, and authentication scheme, while you choose the model and
privacy policy. msgvault never changes the active profile or switches providers
on its own.

```toml
[people.sweep]
enabled = true
provider = "glm"

[people.sweep.providers.glm]
protocol = "openai_chat"
endpoint = "https://api.z.ai/api/paas/v4"
model = "glm-5.3"
auth = "bearer"
credential = "env"
credential_env = "ZAI_API_KEY"
output_mode = "prompt_json"
token_limit_parameter = "max_tokens"
reasoning_effort = "max"
request_timeout = "1m"
retention_posture = "provider-declared"
training_posture = "provider-declared"
allowed_sources = ["conversation_text", "meeting_text"]
source_since = "2026-01-01"
allow_sensitive = true
```

Real sweeps require `allow_sensitive = true` because every packet that carries
seed or context evidence is marked sensitive. A profile with
`allow_sensitive = false` fails on every real sweep and can only run the
synthetic capability check, which sends no archive text. Setting
`allow_sensitive = true` permits sending verbatim archive text to your selected
provider.

You can use four protocols today: `openai_chat`, `openai_responses`,
`anthropic_messages`, and `google_generate_content`. A fifth protocol,
`codex_app_server`, is defined but release-gated and cannot run yet (see
[Codex app-server profiles](#codex-app-server-profiles) below). During
onboarding, msgvault negotiates and saves `native_json_schema`, `json_object`,
or `prompt_json`. OpenAI Chat profiles also save either `max_completion_tokens`
or `max_tokens`, while the other protocols use their own defined token-limit
field.

Other providers use explicit protocol profiles, and OpenRouter, Venice, and
OpenAI also offer built-in presets:

| Example profile | Protocol | Typical profile choice |
|---|---|---|
| GLM 5.3 | `openai_chat` | Z.AI API base, `glm-5.3`, often `max_tokens` |
| Kimi K3 | `openai_chat` or `anthropic_messages` | Choose the exact API surface the account exposes |
| OpenRouter | `openai_chat` | OpenRouter API base and one explicit routed model ID |
| Venice | `openai_chat` | Venice API base and one explicit model ID |
| open-agent-api | `openai_chat` | The gateway's loopback API base and exposed model ID |
| Gemini | `google_generate_content` | Google API base and one Gemini model ID |
| Anthropic | `anthropic_messages` | Anthropic API base and one Claude model ID |
| OpenAI Responses | `openai_responses` | OpenAI API base and one Responses model ID |

Before saving a profile, verify current endpoints, model identifiers, privacy
terms, and subscription rules with your provider. Because OpenRouter and Venice
can route requests to another upstream operator, make sure your retention and
training declarations cover that entire path. If you use a logged-in or
subscription-backed endpoint, including a local gateway, stay within its
provider terms.

People-sweep provider profile credentials are not stored in `config.toml`:

- `credential = "stored"` saves a profile-specific secret in
  `tokens/provider-credentials.json` on every platform and sends it only to the
  endpoint origin you saved it for. If you edit a profile's endpoint to a
  different origin, its stored key stops working; remove the profile and add it
  again with the new endpoint. Keys saved by an older release under
  `tokens/people-providers/` on Linux or macOS move into
  `tokens/provider-credentials.json` the first time msgvault uses the profile.
- `credential = "env"` stores only the environment-variable name. Because
  environment-variable names are host-only settings, configure them through the
  CLI or TOML rather than the Web UI.
- `credential = "none"` is limited to credentialless local or Codex paths.

Changing a credential's value does not change the profile fingerprint, though
changing its source or reference does.

Only `msgvault person provider add` contacts models.dev, and only when you omit
a transport field (`--protocol`, `--endpoint`, `--model`, `--auth`) or pass
`--accept-catalog-prices`. That lookup sends no archive data or provider
credential. Passing `--custom` skips the catalog completely, though the required
synthetic check still contacts the endpoint in your profile. Scheduled and
manual sweeps never contact the catalog. A catalog suggestion also never decides
where a credential is sent: onboarding pairs a credential only with an endpoint
you passed explicitly with `--endpoint` or with the first-party API hosts
compiled into msgvault, so a compromised catalog cannot redirect your key. A
passing check does not grant consent on its own; run
`msgvault person provider consent <name> --yes` as a separate step. Live
credential checks are optional developer or operator verification and are never
CI requirements.

### `[people.sweep]`

Before enabling model-assisted profile maintenance, configure, check, and
consent to a provider. You also need to track a person before the sweep
maintains their facts. [Conversation briefs](usage/people-briefs.md) share this
provider and schedule, with separate enrollment and interval controls.

| Key                      | Default      | Description                                                                                                                                                                                        |
| ------------------------ | ------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `enabled`                | `false`      | Run the scheduled people sweep with the selected provider.                                                                                                                                         |
| `provider`               | `default`    | Name of a table under `[people.sweep.providers]`. The initial profile has an OpenAI endpoint but no model; it is not a usable, consented provider. Setup can explicitly select `openai`, `openrouter`, or `venice`, or configure local `ollama`. |
| `schedule`               | `15 2 * * *` | Daily at 02:15 in the daemon's time zone. An omitted or empty value receives this default; use `enabled = false` to disable the sweep.                                                             |
| `work_batch_size`        | `25`         | Tracked people considered in one worker batch.                                                                                                                                                     |
| `historical_message_cap` | `2000`       | Maximum archived messages considered when finding context for each profile field.                                                                                                                  |
| `context_per_target`     | `8`          | Maximum context items selected for each profile field.                                                                                                                                             |
| `extraction_max_output_tokens` | `4096` | Output token cap of one extraction request, from 1 through 32768. Reasoning models spend part of it thinking before they answer, so a response that hits the cap is cut off and fails. `max_output_tokens_per_person` must be at least this value, and it divides that budget into requests. |
| `evidence_max_bytes`     | `131072`     | Byte limit for an evidence packet.                                                                                                                                                                 |
| `evidence_max_items`     | `200`        | Item limit for an evidence packet.                                                                                                                                                                 |
| `backstop_interval`      | `24h`        | Interval before checking tracked people for changes missed by incremental work.                                                                                                                    |

Hosted people inference requires an explicit
`setup providers --provider <openai|openrouter|venice> --model <model>` choice,
a credential source, and explicit retention, training, and sensitive-content
decisions. Setting `OPENAI_API_KEY` alone configures only eligible embedding
lanes. When you have no OpenAI key or explicit provider choice, setup can offer
the configured loopback Ollama chat model if you pass `--allow-sensitive`. If a
sweep is already enabled, setup preserves it and `--provider` fails with
instructions to use `person provider add` and `person provider use`. See
[setup flags](cli-reference.md#setup-providers).

### `[people.sweep.budgets]`

These request and token limits apply to both sweeps and briefs. Setup leaves
these defaults in place and does not fetch provider prices or set a monetary
limit.

| Scope      | Request key and default       | Input-token key and default            | Output-token key and default           |
| ---------- | ----------------------------- | -------------------------------------- | -------------------------------------- |
| One person | `max_requests_per_person = 4` | `max_input_tokens_per_person = 200000` | `max_output_tokens_per_person = 16000` |
| One run    | `max_requests_per_run = 100`  | `max_input_tokens_per_run = 1000000`   | `max_output_tokens_per_run = 160000`   |
| One day    | `max_requests_per_day = 500`  | `max_input_tokens_per_day = 5000000`   | `max_output_tokens_per_day = 800000`   |

Both `max_estimated_cost_microusd_per_run` and
`max_estimated_cost_microusd_per_day` default to `0`, which disables cost
limits. To enable either limit, set positive values for
`input_cost_microusd_per_million_tokens` and
`output_cost_microusd_per_million_tokens`, which also default to `0`. All four
keys take integer millionths of a US dollar. These caps are local estimates
based on the prices you enter, not provider billing limits.

### `[people.sweep.brief]`

Use `[people.sweep.brief]` to control how often enrolled people get a "Last time
we talked" brief and how much text each generation uses. Briefs use your
selected sweep provider and share its budgets. Your provider profile must allow
sensitive content and include `conversation_text` in `allowed_sources`.

Only supported chat and text-message sources supply brief evidence. Email,
meeting transcripts, documents, and your own replies are excluded. For supported
sources and enrollment steps, see the [brief guide](usage/people-briefs.md).

Brief generation is enabled here by default, though you still enroll each person
individually. These settings apply globally without per-person overrides.
Restart the daemon after changing them.

```toml
[people.sweep.brief]
enabled = true
min_interval = "168h"
pre_call_window = "72h"
max_items = 40
max_bytes = 65536
overlap_items = 8
max_output_tokens = 2048
max_rendered_runes = 560
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `true` | Generate briefs for enrolled people when the sweep runs. `false` stops generation for everyone without removing enrollments. |
| `min_interval` | `168h` | Minimum age of the current version before a scheduled run regenerates it. A rejected current version counts as no brief and is replaced on the next eligible run. `msgvault person brief generate` bypasses this. Must be positive. |
| `pre_call_window` | `72h` | Regenerate this far ahead of a due contact cadence, so the brief is current before you reach out. Must be positive. |
| `max_items` | `40` | Maximum archive items admitted to one brief window. Must be positive. |
| `max_bytes` | `65536` | Maximum packet size for one brief window. Must be positive. |
| `overlap_items` | `8` | Items already covered by the previous brief's window that may be re-admitted so a continued thread is recognizable. Must not be negative or exceed `max_items`. |
| `max_output_tokens` | `2048` | Output cap for the brief call. Must be positive and must not exceed `[people.sweep.budgets] max_output_tokens_per_person`, because a brief is one more call against the same per-person ceiling. |
| `max_rendered_runes` | `560` | Maximum length of the rendered paragraph, in Unicode runes. Msgvault enforces it after the model answers by dropping structured items from the tail: uncertainties first, then follow-ups, then highlights, never the last-interaction sentence and never mid-sentence. Every dropped item is counted in the version's `dropped_item_count`, and the stored structure and evidence pointers are the trimmed ones. Must be at least 240, which is the interaction summary's own maximum length. |

If you set an invalid value, configuration validation fails and names the
offending key instead of clamping it.

### Codex app-server profiles

The `codex_app_server` protocol is not usable in this release. Its transport
stays unavailable until the executable isolation gate releases a verified build,
and every Codex operation fails closed with
`codex app-server isolation is not released`. The profile shape is documented
here so your configuration is ready when the gate ships. Codex sign-in and model
routes return HTTP 503 before changing credentials or consent. Both the
terminal-only [`person provider enroll-codex`](cli-reference.md#person-provider-enroll-codex)
command (which creates a new profile through the daemon) and host-side
`person provider login` (which reauthenticates the selected existing Codex
profile) remain gated.

The Linux launcher disables Codex's local execution environment and shell tools.
While the app server can manage its staged OAuth credential, its command and
filesystem interfaces cannot access it, and any command-execution request or
event aborts inference. Enabling a release still requires a real authenticated
structured-inference check through this launcher.

`codex_app_server` is also the one protocol `person provider add` cannot create.
Generic onboarding negotiates HTTP capabilities against an endpoint, whereas
`codex_app_server` has no endpoint and runs through an attested local Codex
executable instead. The snippet below shows the reference shape for that gated
implementation, not a working setup procedure:

```toml
[people.sweep.providers.codex]
protocol = "codex_app_server"
model = "gpt-5.3-codex"
auth = "none"
credential = "none"
reasoning_effort = "medium"
retention_posture = "zero_retention"
training_posture = "no_training"
allowed_sources = ["conversation_text"]
source_since = "2026-01-01"
allow_sensitive = true
```

Omit `endpoint`, and set both `auth` and `credential` to `"none"`, because the
transport is the local Codex app server authenticated by its own ChatGPT login.
Sensitivity policy works the same across protocols: real Codex sweeps send the
same seed and context packets, so `allow_sensitive = true` is required here too.
Login, model discovery, checks, and consent cannot make this profile usable
while the release gate is closed. Use one of the available HTTP protocols for
[profile automation](usage/people-automation.md) today.

### Windows paths

TOML treats backslashes inside double-quoted strings as escape characters, so
native Windows paths like `"C:\Users\you\..."` cause a parse error.

Use forward slashes or single-quoted strings instead:

```toml
# Forward slashes (recommended)
client_secrets = "C:/Users/you/Downloads/client_secret.json"

# Single-quoted string (backslashes are literal)
client_secrets = 'C:\Users\you\Downloads\client_secret.json'
```

## Cron schedules

Every `schedule` and `cron` key in `config.toml` uses the same five-field cron
format. msgvault parses schedules with
[go-cron](https://github.com/netresearch/go-cron) and keeps its rules as they
are, so check go-cron's documentation for edge cases. The Web UI schedule editor
follows the same rules.

| Field | Values |
|---|---|
| Minute | 0-59 |
| Hour | 0-23 |
| Day of month | 1-31 |
| Month | 1-12 or `jan`-`dec` |
| Day of week | 0-7 or `sun`-`sat`; 0 and 7 are both Sunday |

Each field accepts `*` or `?`, a number or name, a range (`9-17`), a comma list
(`0,30`), or a step (`*/15`, `5/20`, `0-30/10`).

- Ranges where the start comes after the end wrap around. For example, `22-2` in
  the hour field covers 22:00 to 02:59, and `fri-mon` covers Friday to Monday.
- A step must be smaller than the range it steps through. go-cron rejects
  `*/60 * * * *` and `0 */24 * * *`; write `0 * * * *` and `0 0 * * *` instead.
- When you restrict both day fields, a day that matches either one runs. For
  example, `0 4 1 * mon` runs on the 1st of the month and on every Monday.
- Descriptors such as `@hourly`, seconds and year fields, and the `L`, `W`, `#`,
  and `H` extensions are rejected.
- msgvault adds one check of its own: a field made only of commas, such as `,`,
  is rejected because go-cron accepts it but never runs it.

Schedules run in the daemon's local time zone. To run a schedule in another
zone, prefix it with `CRON_TZ=` or `TZ=`, an IANA zone name, and a space, such
as `CRON_TZ=Europe/Berlin 0 3 * * *`. You can quote the zone name, as in
`TZ="UTC" 0 3 * * *`. An empty name, such as `CRON_TZ= 0 3 * * *`, is rejected.

Daylight saving time changes how schedules in the affected hour run:

- When clocks spring forward and skip an hour, a run scheduled in that hour
  starts after the jump at the same minute. In New York, `30 2 * * *` runs at
  03:30 on the spring-forward day.
- When clocks fall back and repeat an hour, `msgvault serve` runs a schedule in
  that hour once, on the first pass. `sync-muesli --watch` runs it on both
  passes.

## Sections

You can run `msgvault setup providers` to write recommended `[vector]` and
`[people.sweep]` values from the API keys in your environment, and
`msgvault setup status` to see every lane's provider, model, consent state, and
next step. The values it picks are listed in
[Recommended Configuration](/docs/usage/recommended-configuration/).

### `[data]`

| Key | Default | Description |
|---|---|---|
| `data_dir` | `~/.msgvault` | Base directory for all data |
| `export_dir` | `{data_dir}/exports` | Directory for attachment ZIPs, downloads, and files opened from the TUI |
| `database_url` | `{data_dir}/msgvault.db` | SQLite database path or PostgreSQL DSN |
| `loose_attachments` | `false` | Keep attachments as loose files and reject pack/repack commands instead of creating immutable packs |

msgvault stores attachments in `{data_dir}/attachments/` and OAuth tokens in
`{data_dir}/tokens/`. You cannot configure those two subdirectories
independently.

Setting `loose_attachments = true` stops msgvault from creating new pack files,
but it does not convert existing packs. To turn existing packs into loose files,
stop the daemon and run `msgvault unpack-attachments` once. Backup restore also
restores attachments as loose files while this setting is enabled.

### `[oauth]`

| Key | Default | Description |
|---|---|---|
| `client_secrets` | — | Path to Google OAuth `client_secret.json` for browser OAuth flows |
| `client_secrets_command` | — | Command argv that prints the client JSON; mutually exclusive with `client_secrets` |
| `service_account_key` | — | Path to a Google service account key JSON for Workspace domain-wide delegation |

#### `[oauth.apps.<name>]`

Use named OAuth apps when a Google Workspace organization requires its own
OAuth credentials. Each entry can define a separate browser OAuth
`client_secret.json`, a service account key, or both. Pass `--oauth-app <name>`
to `add-account` to bind an account to a named app.

| Key | Default | Description |
|---|---|---|
| `client_secrets` | — | Path to the org's `client_secret.json` |
| `client_secrets_command` | — | Command argv that prints this app's client JSON; mutually exclusive with `client_secrets` |
| `service_account_key` | — | Path to the org's Google service account key JSON |

See [OAuth Setup: Google Workspace Accounts](/docs/guides/oauth-setup/#google-workspace-accounts) for when and why you need named apps.

Discord's `--oauth-app` flag is only a protected bot-token binding label.
msgvault does not resolve it from `[oauth.apps]`, and you do not need an entry
here to use it. `export-messages` uses the same daemon and database
configuration as other archive commands, loads no provider credentials, and
makes no provider API calls. It reads a single database snapshot and can run
alongside imports and syncs. The older `export-discord` compatibility command
has the same read-only provider behavior.

When you configure `service_account_key`, `msgvault add-account <email>`
validates the delegated Gmail profile and registers the account without saving
a per-user refresh token. On Unix-like systems, the service account key file
must be readable only by its owner, for example with
`chmod 600 /path/to/service-account.json`.

#### Command-backed Google credentials and tokens

To keep Google client credentials and tokens in your own secret store instead
of local files, configure command-backed storage. Gmail, Calendar, Drive, and
Google Contacts share this configuration.

```toml
[oauth]
client_secrets_command = ["pass", "show", "msgvault/google-client"]

[oauth.tokens]
read_command = ["/path/to/token-store", "read"]
write_command = ["/path/to/token-store", "write"]
delete_command = ["/path/to/token-store", "delete"]
list_command = ["/path/to/token-store", "list"]
```

`client_secrets_command` prints the complete Google `client_secret.json` to
stdout, and you can also set it under `[oauth.apps.<name>]`. Each application
must use either `client_secrets` or `client_secrets_command`. Named
applications do not inherit the default application's client credentials.
Service account keys remain files.

Token commands replace Google token files for every application, including
separate Google Contacts authorizations. Configure all four commands together,
or omit all four to keep file storage; other providers keep their existing
storage. msgvault validates this configuration without running the commands.

| Command | Input | Successful output or effect |
|---|---|---|
| `read_command` | Account and namespace environment variables | Complete msgvault token JSON on stdout |
| `write_command` | Token JSON on stdin; account and namespace environment variables | Save the exact bytes received |
| `delete_command` | Account and namespace environment variables | Remove the token; succeed if already absent |
| `list_command` | Namespace environment variable | JSON array of every stored account spelling in this namespace, such as `["reader@example.com"]`; use `[]` when empty |

If `read_command` returns malformed JSON or omits token fields, msgvault leaves
the stored record unchanged. Fix `read_command` or the stored token JSON, then
retry.

Each token command receives two environment variables:

| Variable | Meaning |
|---|---|
| `MSGVAULT_TOKEN_NAMESPACE` | Token namespace: empty for Gmail, Calendar, and Drive tokens; `carddav-google/<hash>` for a separate Google Contacts authorization, where `<hash>` is the lowercase hex SHA-256 of the OAuth app name (empty for the default app) |
| `MSGVAULT_ACCOUNT` | Account spelling used for this token; empty for listing |

Key each stored record by namespace and account. Because these values do not
depend on the data directory or host, moving the data directory or reading the
store from another machine finds the same records. Keep namespaces separate: a
Google Contacts token can share an account name with its Gmail token in a
different namespace, so your wrapper must not collapse distinct account
spellings or namespaces.

Only `read_command` may exit with status **3** to report a missing record. Any
other nonzero exit fails the operation, and empty or malformed JSON on a zero
exit is also an error. If `write_command` fails after a token refresh, the sync
or request fails instead of continuing with an unsaved token. `list_command`
must include credentials that do not have registered source rows so Gmail alias
checks can find them; read or listing failures stop those checks.

Specify each command as an executable-and-argument array. msgvault does not
invoke a shell or expand tildes, environment variables, redirects, or pipes. It
resolves executable names through `PATH` and relative executable paths against
the process working directory. Use absolute paths for scripts that must run
from the daemon. A
command that cannot start, such as a missing or non-executable binary, is
reported as a configuration error; for `client_secrets_command`, msgvault prompts
you to fix the OAuth app settings. Commands inherit the user's environment and
privileges and must run unattended for scheduled syncs.

Every command invocation has a 30-second deadline and a 1 MiB stdout limit, and
stops if the calling operation is cancelled. When msgvault's stderr is a
terminal, command stderr appears there; otherwise, including inside the daemon,
msgvault discards it. msgvault never puts command output or arguments in error
messages, and ignores stdout from `write_command` and `delete_command`. Pass
secrets over stdin rather than in command arguments.

Google Contacts reuses an access token until it expires, so the daemon runs
these commands roughly once per token lifetime rather than on every contacts
request. Saving or authorizing a Google Contacts connection, or having Google
reject the token, triggers a fresh read.

Keep token JSON unchanged, including `scopes` and `client_id`. A write followed
by a read must return the same bytes. msgvault coordinates local
processes with private lock files and compares snapshots before replacing a
token, and it never writes temporary secret files in command mode.

Those lock files coordinate only processes on the same machine. Because
msgvault reads the token to compare it and then calls `write_command` in a
separate step, the backing store cannot make that sequence atomic. If msgvault
runs on more than one machine against the same store, a refresh on one machine
can overwrite a sign-in saved by another at the same moment. Run msgvault
against a
given store from only one machine at a time, and keep other programs from
writing to the store while msgvault runs.

Configure the command backend on each machine separately. Changing the config
leaves existing plaintext token files in place rather than migrating them
automatically. To migrate a file credential, copy its JSON into your store
under its namespace and account and verify a sync before deleting the old file.
`export-token` reads from the active backend, and the receiving server's
token-upload endpoint writes to its own configured backend, so configure the
receiving backend before uploading. Client credentials stay configured
separately, and setup does not put command output into a NAS bundle.

### `[carddav]` and `[carddav_connections.<name>]` {#carddav}

`[carddav]` defines the `default` connection. Add named tables under
`[carddav_connections.<name>]` for additional accounts; each uses the keys
below and keeps its own credential binding, discovery state, retry state, and
schedule. Connection names must be 1 to 64 lowercase ASCII letters, digits,
underscores, or hyphens, starting with a letter. The name `default` is reserved
for `[carddav]`.

Set up accounts through the [CardDAV account workflow](usage/people-carddav.md)
so the daemon verifies discovery before saving these settings. Two connections
cannot share the same `base_url` and `username`, even if one is disabled. For
Google, the account email is case-insensitive and the OAuth app does not create
a separate CardDAV account. Both config edits and account saves reject
duplicates. If you remove a config table, msgvault keeps that account's archive
data; see
[recovering an orphaned connection](usage/people-carddav.md#recover-a-connection-removed-from-config).

| Key | Default | Description |
|-----|---------|-------------|
| `provider` | `""` | Empty for a password-based server, `google` for Google Contacts, or `microsoft` for Microsoft 365 and Outlook.com contacts |
| `oauth_app` | `""` | Named Google OAuth app; empty selects `[oauth]` |
| `base_url` | `""` | CardDAV discovery URL; Google and Microsoft setup supply their canonical URL, and iCloud setup saves its discovered contacts partition |
| `username` | `""` | Server username, or Google or Microsoft account email |
| `schedule` | `""` | Cron schedule; empty disables scheduled sync |
| `enabled` | `false` | Enable the configured connection |
| `trusted_origin` | `""` | Exact HTTPS origin approved for private access, including its port; a trailing `/` is accepted. Applies only when it matches the account URL's origin. |
| `trusted_addresses` | `[]` | Private IP addresses to dial for `trusted_origin`, without DNS. Accepts `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10`, and `fc00::/7`; rejects duplicates, IPv6 zones, loopback, and link-local addresses. |

For example, to configure a second connection:

```toml
[carddav_connections.work]
base_url = "https://contacts.example.com/dav/"
username = "you@example.com"
enabled = true
schedule = "0 */6 * * *"
```

Run `add-carddav --connection work` or click **Add connection** in **Settings**
to save its credential and discover address books. Passwords are not allowed in
these TOML tables. The default connection stores its binding in
`tokens/carddav.json`, and named connections store theirs in
`tokens/carddav-connections/<name>/carddav.json`. Google authorizations are
shared by account email and OAuth app, separate from these connection bindings.
The scheduler names the default job `carddav` and named jobs `carddav:<name>`.

When you configure a private server, set `trusted_origin` and
`trusted_addresses` together. See
[private-server setup](usage/people-carddav.md#private-servers) for an example,
restart requirements, and what happens when the origin does not match.

Passwords and Google tokens stay in the configured token directory outside
`config.toml`. For browser and terminal authorization steps, see
[Google Contacts setup](usage/people-carddav.md#google-contacts).

### `[microsoft]`

This section configures Microsoft 365 / Outlook.com OAuth and Microsoft Teams
Graph sync. It is required only if you use `add-o365`, `add-teams`, or
`sync-teams`.

| Key | Default | Description |
|---|---|---|
| `client_id` | — | Default Azure AD Application (client) ID |
| `redirect_uri` | `http://localhost:8089/callback/microsoft` | OAuth redirect URI registered in the Azure AD app |
| `tenant_id` | `common` | Azure AD tenant ID; `common` allows both personal and org accounts |

To archive a mailbox in another organization, add a separate app under
`[microsoft.apps.<name>]` with its own `client_id`, `tenant_id`, and
`redirect_uri`. If you omit `tenant_id` or `redirect_uri`, msgvault applies the
table defaults above rather than inheriting from `[microsoft]`. Restart the
daemon after editing Microsoft app settings so scheduled syncs pick up the
changes. Bind a mail source with
`msgvault add-o365 user@org-b.example --oauth-app orgb`. Teams, contacts, and
the Web add-source template always use `[microsoft]`.

For app registration steps, see
[OAuth Setup: Microsoft 365](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail).
Teams uses the default `client_id` with Microsoft Graph scopes and saves tokens
to `tokens/teams_<email>.json`, while Outlook/Hotmail IMAP OAuth with the
default app saves tokens to `tokens/microsoft_<email>.json`.

### `[[fastmail]]`

Add `[[fastmail]]` entries for an optional, source-scoped Fastmail JMAP
identity inventory. This does not replace IMAP credentials: add and sync your
mailbox as usual, then use the API token only to discover masked and send-as
addresses belonging to that source.

| Key | Default | Description |
|---|---|---|
| `source_id` | — | Positive numeric archive source ID; mutually exclusive with `account` |
| `account` | — | Unambiguous source identifier or display name; mutually exclusive with `source_id` |
| `api_token` | (required) | Fastmail API token used for the JMAP identity inventory |
| `auto_confirm_identities` | `false` | Refresh and apply strong provider identity evidence after successful mailbox syncs |

You must set exactly one of `source_id` or `account`. Use `source_id` when two
sources share an identifier or display name. With `auto_confirm_identities`
disabled, run `msgvault identity discover --source-id <id> --provider` to
preview the inventory, and add `--apply` only after you review it. See
[People, Profiles, and Source Identities](/docs/usage/people/#fastmail-alias-inventory).

### `[discord]`

Use `[discord]` for provider-wide Discord import settings and optional
message-container filters. Register guilds and store their bot credentials
first with `msgvault add-discord`; never put tokens or binding labels in
`config.toml`.

| Key | Default | Description |
|---|---|---|
| `max_media_bytes` | `52428800` (50 MiB) | Maximum size of one Discord attachment downloaded during sync or backfill |
| `max_media_mb` | — | Same cap in MiB; when set it takes precedence over `max_media_bytes` |
| `media` | `true` | Download attachment bytes at all |
| `media_scope` | `all` | Which conversations collect media: `all`, `direct` (direct and group chats only), or `none` |
| `media_max_participants` | `20` | Skip media from conversations with more participants than this; `0` disables the cap. See [Media policy](#media-policy) |
| `edit_rescan_window` | `168h` (seven days) | Trailing per-channel/thread window refreshed for edits, deletions, and reaction summaries |

To filter channels or override media settings for a specific guild, add a block
keyed by its exact guild ID. That block also accepts the per-account media
overrides (`media` and `max_media_mb`) that other chat providers place under
`accounts_config`:

```toml
[discord.guilds."123456789012345678"]
include = ["456789012345678901"]
exclude = ["567890123456789012"]
# media = false
# max_media_mb = 25
```

An empty `include` list matches every accessible text or announcement channel,
thread, and forum post. Top-level channels match directly, and child threads or
posts inherit their parent's state unless you list their own ID explicitly. An
explicit child `include` can override an excluded parent, and an explicit child
`exclude` can override an included parent. If the same ID appears in both lists,
`exclude` wins. See
[Discord](/docs/usage/discord/#configure-media-repairs-and-channel-filters).

### Conversation archive tiers (staged)

Conversation policy uses the provider's archived roster and applies the same
participant-count rules as media policy. Four tiers are defined:

| Tier | Intended behavior |
| --- | --- |
| `full` | Archive text and use it in search, embeddings and people features. |
| `cold` | Archive text; explicit lexical search only, without embeddings or people contributions. |
| `stub` | Keep your own messages, direct replies and explicit mentions of you, plus room metadata. |
| `skip` | Stop importing room history. |

This release adds only configuration and inspection. Every room's effective
tier stays `full`. Automatic classification and cold exclusions are not enabled
yet. `stub` and `skip` are rejected until retention and purge support
ships, and saving a `cold` override does not yet hide messages or remove
vectors.

Automatic classification is off by default. To enable it for Beeper, Slack
(including Slackdump imports), Discord, or Teams, set that provider's
`max_participants` to the largest room size you want kept `full`, such as `20`.
Omitting the key or setting `0` keeps every room `full` unless an override says
otherwise. This key is separate from `media_max_participants` and
`max_media_mb`, which continue to control attachments.

```toml
[beeper]
max_participants = 20

[conversation_policy]
activity_window_days = 180

[[conversation_policy.overrides]]
source_type = "beeper"
source_identifier = "example-account"
conversation_id = "example-room"
tier = "cold"
```

The policy selects `full` when a room's known roster is at or below the limit,
or when you have recent activity there: your own post, an explicit mention of
you, or a direct reply to one of your messages. The activity cutoff is
inclusive, and future timestamps do not count as activity. Larger rooms and
rooms with an unknown size select `cold` without that activity.
`activity_window_days` defaults to 180 days (including when set to `0`);
negative values and values above 106751 days are rejected. Other providers keep
their existing behavior.

You can inspect archived rooms and save overrides through the daemon:

```sh
msgvault policy list
msgvault policy set 1 example-room cold
msgvault policy reset 1 example-room
```

`msgvault policy list` prints JSON with source IDs, provider room IDs, roster
status, effective tiers, and selected overrides. Pass those source and provider
IDs to `set` or `reset`. A saved CLI override takes precedence over config, and
`reset` removes it so config or automatic selection applies again. Config
overrides match the exact source type, source identifier, and provider room ID,
and duplicate keys are rejected. All saved values are marked staged in this
release, and these commands neither purge messages nor contact any provider.

### Media policy

`[beeper]`, `[inline]`, `[matrix]`, `[slack]`, `[discord]`, `[teams]`, and
`[imessage]` share a common set of attachment-policy keys that decide which chat
media to download during sync and backfill. Message text is always archived. For
attachment policy, `[imessage]` accepts `media_max_participants` and
`max_media_mb`, along with `enabled`, `schedule`, `db_path`, and `me`; see the
[iMessage settings](#imessage).

[Chatwoot](#chatwoot) supports `media` and `max_media_mb` per configured
account, and `media = false` records an `account_policy` skip marker.

| Key | Default | Description |
|---|---|---|
| `media` | `true` | Download attachment bytes. `false` archives messages without their media and records a `policy_scope` skip marker |
| `media_scope` | `all` | `all` collects from every conversation; `direct` collects only from direct and group chats (not channels, rooms, or guild channels); `none` collects nothing |
| `media_max_participants` | `20` | Skip media from conversations with more participants than this. Omitting the key applies the default; an explicit `0` removes the cap |
| `max_media_mb` | `250` (Discord `50`) | Per-attachment size cap in MiB. Sized for long voice notes, screen recordings, and phone video from direct chats now that the participant cap keeps large-room volume out |
| `accounts_config` | — | Per-account overrides of `media` and `max_media_mb`, keyed by Beeper accountID, Inline canonical account identifier, Matrix user ID, Slack team ID, or Teams account email. Discord uses `[discord.guilds."<id>"]` instead |

The participant cap exists because most attachment bytes in a real chat archive
come from large rooms whose forwarded videos nobody wants kept. Direct chats
and small groups keep their photos, voice notes, and files. When msgvault skips an
attachment under policy, it records a typed marker (`participant_threshold`,
`policy_scope`, `account_policy`, or `size_cap`) so `backfill-*-media` commands
can tell a deliberate skip apart from a failed download and do not retry it
unless you change the policy.

```toml
[beeper]
media_scope = "all"
media_max_participants = 20
max_media_mb = 250

# Keep everything from one account regardless of room size or size cap.
[beeper.accounts_config.signal]
media = true
max_media_mb = 500

# Never download from another account.
[beeper.accounts_config.telegram]
media = false
```

Policy changes affect future downloads only. Media already downloaded under an
earlier policy stays on disk until you run `msgvault purge-excluded-media`,
which removes attachment bytes that the current policy no longer collects.

Participant limits use provider membership rather than the number of senders
seen in archived messages. During a normal Beeper sync, msgvault refreshes
missing or unreadable membership for quiet archived conversations even when
they have no new activity. Those repairs run in bounded batches after message
sync; failed repairs retry after a day without holding up new messages, and a
full sync retries sooner. Discord records its guild member estimate whenever a
participant limit is configured, even if media downloading is disabled. If the
provider still cannot report membership, `purge-excluded-media` keeps that
room's media and prints the unresolved count. Once access is restored, sync
again and re-run the purge preview.

### `[log]`

Structured file logging is disabled by default. Turn it on when you want
persistent, machine-readable logs for troubleshooting. Every CLI run writes a
unique `run_id` on each log line so you can follow a single invocation across
shared daily log files.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Turn on persistent file logging. Setting `dir` also enables it implicitly. |
| `dir` | `<data_dir>/logs` | Directory for log files |
| `level` | `info` | Log level: `debug`, `info`, `warn`, `error` |
| `sql_trace` | `false` | Log every SQL query at info level (verbose, for debugging) |
| `sql_slow_ms` | `100` | Threshold in ms above which SQL queries are logged at warn level. `0` uses the built-in default (100 ms). |

msgvault writes daily logs as newline-delimited JSON in
`msgvault-YYYY-MM-DD.log` (UTC date). When a daily file exceeds 50 MiB, it
rotates to `.log.1`, `.log.2`, and so on, keeping up to 5 rotated files.

With SQL logging enabled, slow and error entries include query arguments and
streaming query durations, which helps you diagnose expensive reads without
enabling full trace output.

To view and tail logs from the selected local or remote daemon, run
`msgvault logs`. See [CLI Reference: logs](/docs/cli-reference/#logs).

### `[sync]`

| Key | Default | Description |
|---|---|---|
| `rate_limit_qps` | `5` | Scales Gmail's local quota-unit refill rate: `5` allows 250 units/second; `3` allows 150. Gmail values above `5` are capped at `5`. Also sets Microsoft Teams Graph requests/second, without that cap, so lowering it slows Teams imports too. Reduce it if Gmail reports quota errors; Google's [project quotas](https://developers.google.com/workspace/gmail/api/reference/quota) can be lower than this local budget. |
| `imap_max_commands_per_minute` | `0` | Evenly spaces each IMAP client's explicit sync read commands and authentication attempts. `0` disables pacing; negative values are invalid. Applies to manual and scheduled syncs. Retries wait for the full delay requested by the server, with at most three retries per command. See [IMAP command limits](usage/imap.md#handle-server-command-limits) for retry behavior and scope. |
| `archive_remote_images` | `false` | Download remote email images during Gmail/IMAP sync and EML, EMLX, MBOX, and PST imports |
| `imap_skip_folders` | `{}` | Per-IMAP-account folders excluded from scheduled syncs and manual syncs without folder flags |
| `trusted_imap_sent_mailboxes` | `{}` | Per-IMAP-account Sent-folder names (keyed by the ACCOUNT identifier from `msgvault list-accounts`) that enable edited-copy snapshot refresh for servers without advertised special-use roles |

To skip folders on every sync, key `imap_skip_folders` by the exact `ACCOUNT`
identifier printed by `msgvault list-accounts`:

```toml
[sync]
imap_skip_folders = { "imaps://user@example.com@imap.example.com:993" = ["Trash", "Junk"] }
```

Folder names match exactly and case-insensitively. Use the full path for nested
folders, without wildcards, and restart the daemon after editing the file.
Passing `--folder` or `--skip-folder` replaces the saved filter for that single
run. Sync prints a warning when an account key matches no stored IMAP source or
a folder name matches no selectable mailbox; unmatched folders stay configured
and are excluded if they appear later. Blank saved folder names prevent the
daemon from starting. See
[IMAP folder filtering](usage/imap.md#what-filtering-changes) for how filtering
affects archive membership, deletion tracking, cursor advancement, and `gc`.

Remote image archiving is **off by default**. Enabling it contacts
sender-controlled servers, which can activate tracking pixels or reveal the
archive server's IP address. Restart the daemon after changing this setting. It
applies only to new ingestion; existing mail needs an explicit backfill.

For the opt-in workflow, supported formats, download limits, and how remote
images affect attachment counts, see
[remote email images](usage/remote-images.md).

Use `trusted_imap_sent_mailboxes` to name an IMAP account's Sent folder on
servers, such as some Exchange or Outlook accounts, whose (possibly localized)
Sent folder does not advertise an RFC 6154 `\Sent` special-use role. By
default, a surviving copy in an unadvertised folder never replaces an archived
snapshot: sync adopts the surviving location but keeps the previously archived
body, raw MIME, recipients, and attachments, because any sender can forge an
RFC822 `Message-ID`. Placement that the server advertises with an unambiguous
`\Sent` or `\Drafts` role is always trusted automatically regardless of this
setting. Listing a mailbox here tells msgvault that it is that account's Sent
folder and holds only mail authored by that account, which re-enables
refreshing the archived snapshot from an edited copy found there.

Key this map by the exact source identifier from the `ACCOUNT` column of
`msgvault list-accounts` (for example,
`imaps://user@example.com@imap.example.com:993`), not by email address or
display name. Trust never crosses accounts: a same-named mailbox in another
synced account stays untrusted, and an account with no entry has no explicit
trust.

```toml
[sync]
trusted_imap_sent_mailboxes = { "imaps://user@example.com@imap.example.com:993" = ["Gesendete Elemente"] }
```

Keep in mind that this is an explicit trust assumption, not proof: if a filter
or IMAP rule files received mail into a listed mailbox, that mail can replace
an archived snapshot under the same Message-ID. Advertised unambiguous `\Sent`
and `\Drafts` folders are trusted automatically, while `INBOX` and any mailbox
carrying `\All`, `\Junk`, or `\Trash` roles are never trusted, even if you list
them here. If the server advertises a listed folder as `\Drafts`, it keeps its
Drafts meaning and cannot be treated as the account's Sent folder.

### `[server]`

Configure the Web UI and API server started by `msgvault serve`. Remote CLI
access uses this same HTTP server, and the local background daemon runs it for
archive-access CLI commands. The owner key also authenticates
[native MCP endpoints](usage/chat.md#native-daemon-endpoints), and `api_key`
doubles as the inbound bearer token when `msgvault mcp --http` starts a
separate Streamable HTTP listener (whose address comes from `--http`). For API
endpoint details, see [Web UI & API Server](/docs/api-server/); for MCP client
setup, see [MCP Server](/docs/usage/chat/#streamablehttp-transport), or fetch
`/openapi.json` from a running server for the generated OpenAPI contract.

| Key | Default | Description |
|---|---|---|
| `api_port` | `0` (auto-select) | Port the server listens on; `0` picks an open port at startup and clients discover it automatically. Set a fixed port for remote/NAS deployments. |
| `bind_addr` | `127.0.0.1` | Bind address, or `iface:NAME` to bind an address on a named interface |
| `api_key` | — | API key for daemon/API authentication and bearer authentication on `msgvault mcp --http`; also authenticates native `/mcp` |
| `api_key_file` | — | Owner-only file holding the API key |
| `api_key_env` | — | Name of an environment variable holding the API key |
| `agent_access` | `false` | Enable restricted agent grants; requires an effective API key. Read at daemon startup only; a `config.toml` edit takes effect only after a restart. |
| `allow_insecure` | `false` | Allow non-loopback binding without `api_key` |
| `cors_origins` | `[]` | Allowed CORS origins |
| `cors_credentials` | `false` | Allow credentials in CORS requests |
| `cors_max_age` | `0` | CORS preflight cache duration in seconds |
| `trusted_proxies` | `[]` | IP addresses or CIDRs allowed to supply forwarded HTTPS/host headers |
| `daemon_idle_timeout` | `20m` | Idle timeout for automatically started daemons; set to `"0s"` to disable |
| `daemon_auto_restart` | `newer` | Local daemon restart policy when the CLI finds a different daemon binary version or an older API schema: `newer`, `never`, or `always` |
| `muesli_recording_max_bytes` | `4294967296` (4 GiB) | Server limit for streamed Muesli audio uploads; `0` uses the default. Independent of the recorder's `recording_max_bytes` |
| `daemon_auto_start` | `true` | Let CLI, TUI, and MCP commands start a local background daemon when none is running; set `false` when a supervisor runs `msgvault serve` |
| `remote_clients` | `[]` | Read-only API keys for remote CLIs; requires an effective server API key. See below. |

Each `[[server.remote_clients]]` entry gives one remote CLI its own read-only key. See [read-only remote clients](guides/remote-deployment.md#read-only-remote-clients) for what those keys can do and their limits.

| Key | Default | Description |
|---|---|---|
| `client_id` | — | Unique name, used in server logs |
| `api_key_file` | — | Owner-only file holding the client's key; relative paths resolve like the server's `api_key_file`. Must differ from the server key and other clients' keys. |
| `collections_write` | `false` | Also allow creating, editing, and deleting collections |

`msgvault serve` reads these files after resolving the server key from
`api_key`, a file, an environment variable, or startup generation. Startup
fails if a file is missing or empty, or if any key repeats another. Local CLI
commands that start a background daemon run the same checks. Restart the daemon
whenever you add, remove, or rotate a key.

`daemon_idle_timeout` applies only to automatically started daemons. Daemons
started with `msgvault daemon start` or `msgvault daemon restart` stay running
until you stop them, and foreground `msgvault serve` runs until interrupted. An
automatically started daemon does not idle out while a
[Meta webhook receiver](#meta-webhooks) is listening. To override the timeout
for automatic starts, set `MSGVAULT_DAEMON_IDLE_TIMEOUT`.

You can also configure the server with flags and environment variables instead
of `config.toml`. `serve --bind` and `serve --port` take priority over
environment variables, which take priority over TOML, followed by defaults.
`iface:NAME` resolves at startup, preferring a usable IPv4 address and falling
back to IPv6; an unknown, down, or unaddressed interface fails before opening a
listener. Startup logs record the bound address and where it came from (flag,
environment, config file, or default).

Config edits validate saved settings without resolving network interfaces or
reading server credentials. Those resources must be available when the server
starts. An unavailable interface or key does not block unrelated edits.

Server credentials check `api_key`, then `api_key_file`, then `api_key_env`. If
the selected file or environment variable is missing, empty, or unsafe,
msgvault fails without trying another source. Key files must be regular
files owned by the process user, at most 64 KiB, and readable only by that user
(`0400` or `0600` on Unix). Symlinks are rejected, and Windows files need an
owner-only ACL. msgvault trims surrounding whitespace when reading a file and
leaves mounted permissions unchanged. Relative secret paths in TOML or
environment variables resolve next to `config.toml` (even before the default
file exists), or next to the file passed to `--config`. Saving configuration
preserves the original path strings so relative paths keep working if you move
the configuration directory.

Container secret mounts must follow the same rules:

- In Docker Swarm, set the secret's `uid` to the process user and its `mode` to
  `0400` (see [Swarm secret options](https://docs.docker.com/reference/cli/docker/service/create/#create-a-service-with-secrets---secret)).
  The default root-owned `0444` mount is rejected.
- In Docker Compose file mounts, set the host file's ownership and mode before
  mounting it.
- Kubernetes Secret volumes use symlinks and are rejected directly. Use a
  Secret-backed environment variable or provide a regular owner-only file
  instead.

These file restrictions apply to `api_key_file`, MCP's `--http-token-file`, and
`credentials set --from-file`. `credentials set --stdin` reads a stream and
can import a readable mounted secret through shell input redirection.

If a secure non-loopback server has no configured credential source, it creates
`<data_dir>/tokens/server-api-key` with an unpredictable key and owner-only
permissions. Persist `data_dir` to keep that key across restarts. Later starts
reuse that file, including loopback-only starts, which means the Web UI then
requires login on `127.0.0.1` as well using the key from that file. An invalid
existing key causes startup to fail instead of being overwritten. Local CLI
clients discover the persisted key automatically, even when they start the
daemon themselves. Setting `allow_insecure = true` skips generating this
default key and keeps unauthenticated mode, though any explicitly configured
key is still enforced. Startup logs name the credential file without printing
its contents.

Run local CLI commands as the same OS user as the daemon with the same
credential sources. Running as a different user, including root through `sudo`
or `docker exec`, fails the key file's ownership check. Inside a container, run
`docker exec --user <daemon-uid> ...`. If `api_key_env` names an environment
variable set only for a supervised daemon, pass that variable to the CLI
process too, because the CLI does not inherit the daemon's environment.

With `daemon_auto_restart = "newer"`, a local CLI replaces an older compatible
daemon with its own binary. A source-build CLI also replaces the release daemon
it was built from or any earlier release, while a daemon from a different
source build of the same release keeps running and the command prints the
restart command. Set `"never"` when an external supervisor manages the daemon,
or `"always"` to restart whenever the recorded daemon version differs. A CLI
client never auto-restarts a remote server. When automatic replacement is
suppressed for a daemon that would otherwise be replaced, local commands print
both versions and `msgvault daemon restart` to stderr.

A local daemon whose API schema is older than the CLI's cannot serve it. Under
`"newer"` and `"always"`, the CLI replaces that daemon, including one from
another source build of the same release. With `"never"`, or with
`daemon_auto_start = false`, the CLI stops with an error that tells you to
upgrade the daemon. Run `msgvault daemon restart`, or restart it through your
supervisor.

Set `daemon_auto_start = false` when a supervisor such as launchd, systemd, or
Docker runs `msgvault serve`. Local archive commands then connect to the
running daemon, wait for one that is starting up, or fail with an error instead
of launching their own. They also never replace a running daemon, regardless of
`daemon_auto_restart`, because the supervisor owns restarts. Running
`msgvault daemon start` or `msgvault daemon restart` still starts a daemon on
request. `msgvault update` restarts only a daemon that msgvault started in the
background, unless you pass `--no-restart`. Commands routed to `[remote].url`
are unaffected.

Browser sessions work alongside API-key authentication, while CLI and
programmatic clients continue sending the configured key. For remote browser
access, terminate TLS at a reverse proxy and list only that proxy, not
arbitrary clients, in `trusted_proxies`. See [Web UI](/docs/web-ui/) for the
full security model and the plain-HTTP warning.

For MCP Streamable HTTP, send the effective `[server]` key, or the key from
`--http-token-file` or `--http-token-env`, as `Authorization: Bearer <key>` on
every `/mcp` request. This inbound credential is separate from
`[remote].api_key`, which authenticates `msgvault mcp` when it connects to a
remote daemon.

### `[web]`

Set defaults for the daemon-served browser application. You can also change
these values in **Settings**, while `config.toml` stays authoritative.

| Key | Default | Description |
|---|---|---|
| `default_search_mode` | `full_text` | Initial mode: `full_text`, `semantic`, or `hybrid` |
| `theme` | `system` | Color theme: `system`, `light`, or `dark` |
| `density` | `compact` | Table density: `compact` or `comfortable` |

Settings edited in the browser are validated and written with optimistic
concurrency. Click **Save changes** for general settings; provider API keys,
providers, contact accounts, and People sweep profiles each save through their
own buttons.

Changes to `[web]` values, person-enrichment provider keys, and contact
accounts take effect right away. Other settings and embedding provider keys
take effect after you restart the daemon, and the UI shows a restart banner
when the running daemon still holds an old value. Changing `server.api_key`
also requires a restart and invalidates existing browser sessions.

### `[integrations.tasks]`

Configure an optional, provider-neutral integration for linking messages to
tasks. Person agendas use the separate `[integrations.kata]` connection below.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable discovery and capability checks |
| `endpoint` | — | Explicit loopback HTTP, Unix socket, or HTTPS endpoint; empty requests secure local discovery |
| `api_key` | — | Server-side credential; the browser sees only a masked hint of it, never the key |
| `default_project` | `msgvault` | Fixed project used for create/link/search operations |

Remote plaintext HTTP endpoints are rejected. An endpoint works only if it
supports the required idempotency and compare-and-swap capabilities, and the UI
distinguishes disabled, authentication required, incompatible, partial, stale,
unavailable, and ready states.

### `[integrations.kata]`

Connect to Kata for live person agendas and for filing
[Kata issues from archive evidence](usage/kata-issues.md). Tasks remain in
Kata, and msgvault fetches their current state when you open a person's agenda.
This integration targets Kata v0.18.0 and requires Kata API schema version
0.21.0 or later.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable Kata person agendas and evidence issues |
| `endpoint` | — | Required when enabled: an explicit HTTPS URL, loopback HTTP URL, or Unix socket URL |
| `trust_private_network` | `false` | Also allow HTTP to DNS names and private, link-local, unspecified or CGNAT addresses on a network you trust; public literal IPs stay refused |
| `api_key` | — | Bearer credential sent by the daemon to Kata; Settings returns only its configured state and a masked hint |
| `default_project` | `msgvault` | Existing active Kata project used for person agendas and evidence issues |

Create the project in Kata, then configure its endpoint and credential on the
machine running the msgvault daemon:

```toml
[integrations.kata]
enabled = true
endpoint = "https://kata.example.com"
api_key = "replace-with-your-kata-api-key"
default_project = "msgvault"
```

Restart the msgvault daemon after saving. You can also edit these fields in
**Settings** and restart afterward. If you change the endpoint to a different
origin in **Settings**, msgvault clears the saved key unless you supply a
replacement key in the same save. For containers on a trusted private network,
set `endpoint = "http://kata:7777"` and `trust_private_network = true`; keep in
mind that this sends traffic, including your API key, in plaintext over that
network. Without `trust_private_network = true`, non-loopback HTTP is rejected.
Kata does not use local endpoint discovery, and `[integrations.tasks]` does not
configure person agendas.

Each Kata task can belong to one person. The scalar metadata value
`msgvault.person` stores the person's canonical vCard UID rather than their
numeric msgvault person ID, and `msgvault.list` names the list (defaulting to
`agenda`). Reads and unlink operations also recognize the person's UID aliases
after a merge.

Agendas show open tasks only, returning up to 100 items. If a response is
marked `truncated`, open Kata to see the rest. The transport also caps each
response at 1 MiB and returns an error above that limit. You can create, link,
move between lists, and unlink tasks in msgvault; to edit task content or
priority, complete tasks, or reopen them, use Kata. See
[person agenda commands](cli-reference.md#person-agenda).

### `[analytics]`

Control daemon-side aggregate query behavior. The Web UI, TUI, MCP server, and
aggregate list commands all use these settings through the local daemon or a
configured remote server.

| Key | Default | Description |
|---|---|---|
| `engine` | `auto` | Aggregate engine: `auto` starts with live SQL and switches to DuckDB after cache maintenance succeeds; `sql` always uses live SQL; `duckdb` requires a usable Parquet cache |
| `auto_build_cache` | `true` | Refresh a stale or missing Parquet cache automatically at startup, after scheduled or manual syncs, and when a query finds it due; `false` skips automatic builds. An explicit `query --fresh` or sync `--build-cache` can still request one |
| `min_rebuild_interval` | `0s` | Minimum age of a usable cache before a sync, query, or daemon restart may queue an automatic rebuild. Queries serve the committed snapshot during the interval |
| `builder_memory_limit` | Adaptive | DuckDB buffer-manager budget for cache builds; an explicit value such as `16GiB` or `512MiB` replaces the adaptive default. Total process memory can exceed it |
| `builder_threads` | min(CPUs, 2) | Maximum DuckDB threads for cache builds; SQLite scanner builds use one worker. Zero keeps the default |
| `builder_temp_limit` | `32GB` | Maximum spill-to-disk size for cache builds |
| `query_memory_limit` | `512MB` | DuckDB memory limit for daemon aggregate queries; raise it on a large archive |
| `query_threads` | min(CPUs, 4) | DuckDB threads for daemon aggregate queries; zero keeps the default |
| `query_temp_limit` | `2GB` | Maximum spill-to-disk size for daemon aggregate queries; a query that spills past it fails with a DuckDB out-of-memory error |

Each thread setting caps both DuckDB's query worker pool and its separate
asynchronous I/O pool from database creation. It is a per-pool limit rather
than a cap on all threads in the process.

If a Web UI query runs out of memory or temporary disk space, the error names
the query-limit keys above. Try narrowing your filters first. Before raising
these limits in `config.toml`, check available memory and free disk space on
the host running msgvault, then restart the daemon and retry the query. These
limits cap resource use and do not pre-reserve memory or disk space. Cache
builds use the separate `builder_*` limits.

By default, the builder budgets one quarter of currently available memory,
clamped between `256MiB` and `16GiB` (releases before 0.23.0 used a fixed
`2GB` default). On Linux, it also accounts for remaining memory under visible
cgroup v2 ancestor limits. If host memory cannot be detected, it falls back to
`2GB`; unsupported or unreadable Linux container limits cap the default at
`2GB` while keeping any lower headroom already detected. The `256MiB` floor is
an attempt budget and can exceed available memory on an exhausted host. Setting
`builder_memory_limit` explicitly skips memory detection.

Start with the builder defaults, including at most two threads. More threads can
increase peak memory use. DuckDB's memory budget covers only its
buffer manager, while native allocations and Go memory add to total process
memory. A `24GB` budget therefore does not guarantee that a build stays under
24 GB of resident memory, so leave headroom for the daemon, syncs, other
processes, and those allocations. On a constrained host, lower
`builder_memory_limit` and set `builder_threads = 1` before raising the memory
budget. See
[DuckDB's memory guidance](https://duckdb.org/docs/stable/guides/performance/oom).

If a cache build runs out of memory or spill space, its error reports the
effective `builder_memory_limit`, `builder_threads`, and `builder_temp_limit`.
Check available memory and disk space before raising the relevant key in
`config.toml`, then run `msgvault build-cache` again. A large archive may need
an explicit budget above the adaptive cap. Failed builds leave the last
published cache intact.

Builders spill temporary work under the cache staging directory beside the
analytics cache. Check free space on that filesystem before raising
`builder_temp_limit`. The default allows up to `32GB` of spill on top of the
existing cache and the new generation being staged. When DuckDB's SQLite
scanner is unavailable, the CSV fallback also writes a temporary copy of the
source tables beside the database (or in the system temp directory if that
fails). A larger disk budget can help a large archive finish with a smaller
memory budget, and it does not reserve space upfront.

The relationship cache stores one fact per message along with its direct sender
or recipient edges. Group-chat builds join the full member roster once per
logical conversation entry, while earlier messages keep their direct edges and
owner presence for relationship scores. Large groups therefore keep every
member searchable in People without multiplying every message by the full
roster, while direct chats and meetings keep per-message participant
attribution. `[activity].max_direct_counterparts` still controls the activity
projection; changing it does not affect relationship membership.

The daemon brings up HTTP health and API routing before running analytics cache
maintenance. With `engine = "duckdb"`, analytics stay unavailable until a
usable cache is ready, and `msgvault serve` fails instead of silently falling
back if no usable cache can be built or opened. If an automatic refresh fails
after a sync, the daemon keeps serving the last usable cache and tries again 15
minutes later even if subsequent syncs write nothing. With
`auto_build_cache = false`, run `msgvault build-cache`, `query --fresh`, or
sync `--build-cache` to maintain the cache explicitly. Per-command analytics
flags deprecated in 0.17.0 (`msgvault tui --force-sql`,
`msgvault mcp --force-sql`, `msgvault tui --no-cache-build`, and
`--no-sqlite-scanner`) were replaced by `[analytics]`; set `engine = "sql"` to
force live SQL.

`min_rebuild_interval` throttles automatic refreshes triggered by syncs,
queries, and daemon startup. A restart or sync within that window keeps serving
the existing publication and schedules a rebuild check once the interval ends.
On a busy archive, Parquet analytics can therefore lag SQLite by roughly the
configured interval plus cache build time, and deleted messages can stay
visible in analytics results until the next publication (both during the
interval and while a build is in progress). Even a zero interval allows stale
rows while a build runs. To wait for analytics that include deletions committed
before your request, run `query --fresh`.

Explicit refresh requests and recovery of a missing, interrupted, incompatible,
or otherwise unusable cache are never delayed by `min_rebuild_interval`.

Automatic rebuilds run in the background outside the scheduled sync that
requested them, so syncs stay on schedule while a build runs. A sync finishing
mid-build does not abort it: the build publishes one consistent read snapshot
with that snapshot's counters and message boundary, and the next build appends
later messages and repairs journaled child-row changes. Participant links can
refresh relationship data while keeping existing message shards, even when new
messages arrive in the same sync. Changes to baked message facts, account
identities, deletions, and failed syncs still trigger a full rebuild, and
archives without the child-row repair journal fall back to a full rebuild after
an overlapping sync. The published snapshot stays usable across daemon
restarts, and automatic follow-up builds still respect `min_rebuild_interval`.
Because cache build memory and temporary disk usage grow with archive size,
setting a minimum interval helps avoid repeated full-scale work on frequently
synced archives. Restart the daemon for `[analytics]` changes to take effect.

These settings control aggregate views (Senders, Domains, Labels, and Time) on
SQLite and are ignored when `[data].database_url` points at PostgreSQL, because
a PostgreSQL backend always uses live SQL for those views and `build-cache`
refuses to run against it. In `auto` mode on SQLite, **Messages** serves live
archive rows while the cache rebuilds and after a failed build, showing a
recovery notice if a build fails. Grouping, person filters, semantic search,
per-row exact-match counts, bulk actions, **Files**, **People/domains**, and
the reader's **Open person** action still require the analytical cache. Those
workspaces remain unavailable on PostgreSQL; see
[PostgreSQL Backend](/docs/architecture/postgresql/) for current scope.

### `[backup]`

Set defaults for `msgvault backup`. For the capture, verify, and restore
workflow, see [Backup](/docs/usage/backup/).

| Key | Default | Description |
|---|---|---|
| `repo` | — | Default backup repository directory used when a backup subcommand omits `--repo` |
| `zstd_level` | `0` | Compression level for backup pack files. `0` uses msgvault's built-in default; otherwise use `1` through `19` |

### `[remote]`

When you set `[remote].url`, archive-access CLI commands talk to that remote
server by default instead of the local background daemon. To target the local
daemon for a single command, pass `--local`.

The remote server must run the same or a newer API schema of the same major
version as the CLI. Otherwise the CLI stops before sending archive requests and
tells you to upgrade the daemon. The same rule applies to the daemon named by
`--agent-url`.

| Key | Default | Description |
|---|---|---|
| `url` | — | Remote API base URL (e.g. `http://nas-ip:8080`) |
| `api_key` | — | API key used by remote commands |
| `api_key_file` | — | Owner-only file holding the remote API key |
| `api_key_env` | — | Name of an environment variable holding the remote API key |
| `allow_insecure` | `false` | Allow HTTP remote connections |

Commands that use `[remote]` include `search` (FTS mode), `query`,
`show-message`, `stats`, `list-accounts`, `list-senders`, `list-domains`,
`list-labels`, `identity` subcommands, `collection` subcommands, `export-eml`,
`export-attachment`, `export-attachments`, and `tui`. With a
[read-only remote client](guides/remote-deployment.md#read-only-remote-clients)
key, `list-senders`, `list-domains`, `list-labels`, `query`, vector search,
`tui`, and `mcp` are not available.

These settings also route `mcp` to a remote daemon. Secret precedence and file
rules match [server credentials](#server). Passing `--local` ignores the remote
destination and its secret sources, and msgvault never reads secrets for an
unused destination. `setup` and `export-token` never copy runtime keys from
files or environment variables into saved `api_key` fields.

### `[remotes.<name>]`

Use named remotes when one config needs to reach multiple servers, such as a
home archive in `[remote]` and a separate work archive. Each table accepts the
same keys as `[remote]`, and `url` is required. Pass `--remote <name>` to
target that server for one command:

```toml
[remotes.work]
url = "https://archive.example.com"
api_key_file = "work.key"
```

```sh
msgvault --remote work search "quarterly plan"
```

When you omit `--remote`, `[remote]` stays the default. Passing `--remote`
replaces the entire `[remote]` destination for that run, including
`MSGVAULT_REMOTE_URL` and `MSGVAULT_REMOTE_API_KEY`. A relative `api_key_file`
resolves next to the config file. `export-token --remote <name>` uploads to the
named server without saving anything to `[remote]`. You cannot combine
`--remote` with `--local` or agent-delegated mode, and an unknown remote name
fails the command.

#### Credential helper

A hosted archive can issue short-lived bearer tokens instead of a static API
key. Set `credential_provider` to a trusted local helper (as an executable and
arguments) and describe the approved access in a `delegation` table. The
hosting service's setup tool usually writes both blocks for you:

```toml
[remotes.hosted]
url = "https://archive.example.org/messages"
credential_provider = ["example-credential-helper", "--server", "work"]

[remotes.hosted.delegation]
archive_id = "archive-a"
client_id = "22222222-2222-4222-8222-222222222222"
delegation_id = "33333333-3333-4333-8333-333333333333"
source_ids = [17, 23]
```

| Key | Description |
|---|---|
| `credential_provider` | Helper command and arguments, run without a shell. It excludes `api_key`, `api_key_file`, and `api_key_env`. |
| `delegation.archive_id` | The hosted archive's UID |
| `delegation.client_id` | The approved client installation |
| `delegation.delegation_id` | The approval record |
| `delegation.source_ids` | The approved sources |

`url` must use HTTPS. Each command asks the helper for a bearer token covering
the read operations called by `list-accounts`, `stats`, `search`,
`show-message`, `show-thread`, and `export-messages`, and sends that token only
to `url`. msgvault keeps the bearer in memory and renews it before expiry. If
the helper fails or refuses, the command fails without falling back to an API
key. `search` supports `--mode fts` only, and `export-token` requires `--to`
and `--api-key` for a server where you hold the owner key. msgvault writes a
credential-free request journal in `remote-credentials/` under the msgvault
home so an interrupted request can resume. See the
[credential provider guide](guides/credential-providers.md#read-a-remote-through-a-helper)
for how the helper works.

### `[[accounts]]`

Use `[[accounts]]` entries to configure sync for individual accounts. You can
schedule automatic background syncs for Gmail, IMAP, Microsoft Teams, and
Discord sources. For IMAP or Teams, use the account
display name or email when available instead of a raw provider identifier. For
Discord, always use the exact guild ID because guild display names can change
or collide.

| Key | Default | Description |
|---|---|---|
| `email` | (required) | Account identifier/display name, or exact Discord guild ID, to sync |
| `schedule` | — | Cron expression for sync schedule (e.g., `0 * * * *`) |
| `enabled` | `false` | Whether scheduled sync is active for this account |
| `gmail_skip_spam_trash` | `false` | Skip acquiring messages in Gmail Spam or Trash during full, incremental, and History recovery syncs |

To exclude Spam and Trash for a Gmail account, set the key on its entry. As
with schedules, `email` matches the account's identifier, display name, or
alias, ignoring case. The setting also applies when the entry has no schedule.
Add the key to the existing entry if one already exists.

```toml
[[accounts]]
email = "account@example.com"
gmail_skip_spam_trash = true
```

Messages already in the archive stay there. When Gmail History reports a move
to Trash, incremental sync updates the message's label. A permanent deletion
marks it as deleted from the source. Previously skipped messages can be
acquired when Gmail History reports that they have left Spam or Trash. To
backfill messages still in those folders, disable the setting and run an
unrestricted `sync-full`.

For example, to schedule a registered Discord guild on its own cadence:

```toml
[[accounts]]
email = "123456789012345678"
schedule = "*/30 * * * *"
enabled = true
```

### SyncTech SMS sources

Configure scheduled SMS Backup & Restore sources with `[[synctech_sms.sources]]`
entries. Running `msgvault add-synctech-sms-drive` creates these entries for
you, and you can also edit them directly.

| Key | Default | Description |
|---|---|---|
| `name` | (required) | Source name used by `sync-synctech-sms <name>` and scheduler logs |
| `enabled` | `false` | Whether the source is active |
| `backend` | `local` | `local` for a path on disk, or `drive` for Google Drive |
| `path` | — | Local XML/ZIP file or directory when `backend = "local"` |
| `folder_id` | — | Google Drive folder ID when `backend = "drive"` |
| `google_account` | — | Google account used for Drive access |
| `owner_phone` | (required) | Owner phone number in E.164 format |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `include_sms` | `true` | Import SMS records |
| `include_mms` | `true` | Import MMS records |
| `include_calls` | `true` | Import call logs |
| `include_attachments` | `false` | Import MMS attachments |
| `stable_after` | `10m` | How long Drive files must remain unchanged before import |
| `oauth_app` | — | Named Google OAuth app to use |

### Google Calendar sources

Add top-level `[[gcal]]` entries to schedule Google Calendar syncs. Authorize
each account first with `msgvault add-calendar`, then add one entry per OAuth
account. `msgvault serve` runs each entry on its cron schedule. The first run
performs a full sync and registers calendars, and later runs are incremental.

```toml
[[gcal]]
name = "primary"                 # optional; defaults to email
email = "you@gmail.com"          # OAuth account = token key
oauth_app = ""                   # optional named OAuth app
calendars = []                   # optional calendarId filter; empty = owner+writer
schedule = "0 */6 * * *"         # 5-field cron, no seconds
enabled = true
write_calendars = []             # explicit IDs; empty denies every event write
invite_calendars = []            # subset allowed to change guests or notify them
# calendar_aliases = { team = "team@example.com" }
```

| Key | Default | Description |
|---|---|---|
| `name` | email | Source name used by `sync-calendar <name>` and scheduler logs |
| `email` | (required) | Google account that owns the token (the token key) |
| `oauth_app` | — | Named Google OAuth app to use |
| `calendars` | — | Specific calendar IDs to sync; empty syncs owned/writable calendars |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Enable the source for scheduled sync and live control; scheduling also requires `schedule` |
| `write_calendars` | `[]` | Exact live calendar IDs allowed for event writes; empty denies all writes |
| `invite_calendars` | `[]` | Exact IDs allowed to change guests, respond, or notify them; writes still require `write_calendars` |
| `calendar_aliases` | `{}` | Names mapped to exact calendar IDs for live control; aliases do not expand permissions |

Event control also requires [write consent](usage/calendar.md#control-events).
`calendars` controls only which calendars sync and does not grant write
permission. `email` selects the OAuth token, while `write_calendars` lists the
calendars that own the events. For example, `person@example.com` can create
events on `team@example.com` when Google currently reports `owner` or `writer`
for that calendar. `primary` resolves to the live primary calendar ID before
the daemon checks policy. Put the actual calendar ID in both permission lists,
because neither list accepts wildcards or alias names.

### `[matrix]`

Archive joined rooms from native [Matrix](/docs/usage/matrix/) accounts. This
single block applies to every account you register with `msgvault add-matrix`;
never put credentials in `config.toml`.

```toml
[matrix]
enabled = true                    # gate for the daemon schedule
schedule = "*/30 * * * *"         # 5-field cron; empty = manual sync only
rooms = []                        # exact room-ID include filter (empty = all)
exclude_rooms = []                # exact room IDs to skip; wins over rooms
media = true                      # download attachment bytes
media_scope = "all"               # all, direct, or none
media_max_participants = 20       # skip media from larger rooms; 0 = no cap
max_media_mb = 250                # per-file download cap (MiB)

# [matrix.accounts_config."@archive:example.org"]
# media = false
# max_media_mb = 100
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Whether the daemon schedules Matrix sync |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `rooms` | all joined rooms | Exact Matrix room IDs to sync |
| `exclude_rooms` | — | Exact Matrix room IDs to skip; exclusions win over inclusions |
| `media` | `true` | Download attachment bytes during sync |
| `media_scope` | `all` | `all`, `direct` for rooms listed in `m.direct`, or `none`; `direct` excludes group rooms absent from `m.direct`. See [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip media from rooms above this many joined members; `0` = no cap |
| `max_media_mb` | `250` | Per-file download cap in MiB |
| `accounts_config` | — | Per-user-ID `media` and `max_media_mb` overrides |

### `[beeper]`

Archive chats from a locally running [Beeper Desktop](/docs/usage/beeper/).
Because the Beeper Desktop API is loopback-only, there is one instance per
machine and the daemon must run beside it, so `[beeper]` is a single block
rather than a list. Authorize first with `msgvault add-beeper`.

```toml
[beeper]
# url = "http://localhost:23373"  # Beeper Desktop API (default)
enabled = true                    # gate for the daemon schedule
schedule = "*/30 * * * *"         # 5-field cron; empty = manual sync only
accounts = []                     # accountID include filter (empty = all)
exclude_accounts = []             # skip networks archived natively, e.g. ["whatsapp"]
rate_limit_qps = 20               # request rate against the local API
media = true                      # download attachment bytes
media_scope = "all"               # all, direct, or none
media_max_participants = 20       # skip media from larger rooms; 0 = no cap
max_media_mb = 250                # per-attachment download cap (MiB)

# [beeper.accounts_config.signal]  # per-account override, keyed by accountID
# media = true
# max_media_mb = 500
```

| Key | Default | Description |
|---|---|---|
| `url` | `http://localhost:23373` | Beeper Desktop API base URL |
| `enabled` | `false` | Whether the daemon schedules Beeper sync |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `accounts` | all | Beeper accountIDs to sync (include filter) |
| `exclude_accounts` | — | Beeper accountIDs to skip (wins over `accounts`) |
| `rate_limit_qps` | `20` | Request rate limit against the local API |
| `media` | `true` | Download attachment bytes (failed downloads retry via `backfill-beeper-media`) |
| `media_scope` | `all` | `all`, `direct`, or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip media from conversations above this many participants; `0` = no cap |
| `max_media_mb` | `250` | Per-attachment download cap in MiB (over-cap media is recorded as a `size_cap` skip and retried only after the cap changes) |
| `accounts_config` | — | Per-accountID `media` and `max_media_mb` overrides |

#### Mirror stored attachments to Docbank

msgvault can mirror stored attachment bytes from every source to a Docbank
folder. Enable the integration and its separate attachment upload consent; the
daemon then drains the backlog at upload speed in passes of about 50 seconds:

```toml
[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"
api_key_env = "DOCBANK_API_KEY"
attachment_upload_consent = true
attachment_folder = "/msgvault"
document_profile = "documents"
```

| Setting | Default | Effect |
|---------|---------|--------|
| `attachment_upload_consent` | `false` | Allow attachment bytes from all sources, including future imports, to leave msgvault. Without consent, status reports mirroring as disabled |
| `document_profile` | Empty | Docbank profile used to process mirrored attachments and search their text |
| `attachment_folder` | `/msgvault` | Clean absolute virtual directory path; missing directories are created in Docbank |
| `attachment_search_profile` | `""` | Name an existing Docbank processing profile for semantic/hybrid attachment retrieval. Does not queue processing or embeddings |

Point `document_profile` at a Docbank profile that has a text-extraction
rendition stage. Processing covers all mirrored files except declared images,
audio, and video, as long as a live message references them. Attachment search
also returns images whose text was already processed in Docbank. To grant
consent in Docbank across documents for that profile:

1. Run `docbank processing plan <path-or-id> --profile <name>` on one mirrored
   file and review the plan.
2. Run `docbank processing build <path-or-id> --profile <name> --plan-fingerprint <fp> --consent`.

msgvault always sends `consent:false`, keeping processing consent inside
Docbank.

The mirror also requests processing for files uploaded before you enabled
`document_profile` or before an upgrade, and changing the profile in Docbank
triggers processing again. Run `msgvault docbank attachments status` to see
extracted, processing, and waiting counts, per-code counts for files without
text, and worker errors. Run `msgvault docbank attachments backfill` to retry
failed uploads and restart discovery. Backfill re-requests processing for files
that failed due to missing consent, stale authority, a missing PDF password, an
unreadable password file, a quarantined job, or a malformed Docbank response.
Backfill clears only settled recoverable failures. A job that fails after
backfill needs another backfill. Other outcomes wait for a profile change.

The mirror includes files retained after source deletion or hidden by
deduplication, and excludes files whose attachment rows were removed. Hashless
Slack or Discord attachments use the content hash from their local storage
path. The mirror shares the same URL and credential rules as the audio route
below. Its consent covers file transport only, while Docbank owns document
processing consent and profiles. `all_sources_upload_consent` controls the
audio and transcript route independently. When both routes are enabled, audio
files are both mirrored as files and sent to the audio route.

Run `msgvault docbank attachments status` (with `--json` for structured output)
to inspect consent, folder, delivered, sending, and failed file counts, failure
reasons, and remaining distinct eligible files. Run
`msgvault docbank attachments backfill` to retry failed files and discover
existing attachments. Both commands talk to the selected daemon, and backfill
requires attachment upload consent and the attachment mirror job.

The daemon discovers newly stored attachments and restored messages
automatically, queueing each new file as `sending` without contacting Docbank
so discovery finishes even while Docbank is unreachable. Uploads then drain the
queue, retrying folder errors automatically. Temporary I/O or upload errors
wait at least five minutes before retrying and stay under `sending` in status
output with their failure reason, while attempts interrupted before recording a
result show `retrying`. Missing or corrupt bytes fail only that file, and
restoring a message or updating its attachment makes unavailable files eligible
again. HTTP 400, 413, 415, or 422 responses and invalid receipts need a
backfill after you fix the underlying cause.

Purging messages from msgvault leaves mirrored copies and delivery receipts in
place. To start fresh against a rebuilt Docbank at the same URL, change
`attachment_folder`. Restart the daemon after changing consent, the
destination, or `attachment_search_profile`.

Each unique file uploads once per destination under
`<first 12 hex of hash>-<sanitized basename>`, and Docbank handles filename
collisions and replays. Attachment and message details display the resulting
Docbank file.

Each content hash has one durable delivery per archive, Docbank URL, and
folder. Duplicate bytes across many messages share the same file. Completed
deliveries make no further upload requests. Changing the destination starts a
new mirror. Disabling the mirror or changing its destination keeps existing
delivery records and remote files, while attachment and message details show
receipts only for the active destination. If you re-enable the earlier
destination, its receipts reappear without re-uploading delivered files.

Docbank's digest-checked upload endpoint takes one file part and no message
metadata. The upload filename prefixes the first filename's basename with the
first 12 hex characters of the content hash and a hyphen; control characters
become underscores and invalid basenames become `attachment`. msgvault keeps
exact filename, message, source, sender, and date provenance in the archive.
Attachment and message details in the CLI, HTTP API, and MCP include `docbank`
receipts with `endpoint`, `folder`, `node_id`, `version_id`, and `blob_hash` so
callers can open the file in Docbank, while list and search views omit them.
URL-only attachments are excluded from discovery. Details recover a hashless
Slack or Discord attachment's hash from its trusted CAS storage path, and the
mirror reads these aliases from loose files or retained pack entries while
verifying their SHA-256. It uploads attachment content and excludes thumbnails
from `thumbnail_hash`. Missing or corrupt archive bytes fail only that file,
and Docbank processes mirrored documents to supply their searchable text.

#### Search document text through Docbank

To search document text, configure a Docbank rendition profile (such as a
Docling profile) and set its name in `[integrations.docbank] document_profile`.
Your Docbank daemon must support the chosen processor, and a Mistral profile
requires Docbank v0.17.0 or later. For Mistral, create the five seed files and
run `docbank processing probe --profile <name> --seeds <dir>` to write its
capability manifest, as described in
[Docbank's Mistral OCR configuration](https://github.com/kenn-io/docbank/blob/main/docs/configuration.md#document-ocr-with-mistral).
This replaces `msgvault documents probe-mistral`.

Follow the [attachment mirror setup](https://msgvault.io/docs/configuration/#mirror-stored-attachments-to-docbank)
to review the processing plan and grant consent in Docbank. msgvault requests
that profile for mirrored files, while Docbank owns extraction, text, and
embeddings. Check progress with `msgvault docbank attachments status`.

Search extracted text with `msgvault media search "project proposal"`,
`GET /api/v1/media/search`, MCP `search_media`, or the Web **Everything**
panel. To scope CLI results to one person, add `--person <id>` and
`--direction from_person`.

Remove `[attachments.documents]` from older configuration files; the loader
warns and ignores that section and points you to `document_profile`. Upgrading
drops the local document text, chunk, and consent tables. Backup snapshots
taken before the upgrade keep them and can still restore, and
`msgvault backup list` marks them in the `DOCUMENT TEXT` column. Docbank
processes the mirrored files again, which incurs OCR charges if you use
Mistral.

#### Send stored audio to Docbank

The daemon can send stored WAV and MP3 audio from any captured source,
including messaging and email imports, to a separate Docbank media service that
exposes Docbank's media HTTP routes. For capture and processing rules, see
[Send audio to Docbank](/docs/usage/beeper/#send-audio-to-docbank).

```toml
[integrations.docbank]
enabled = true
url = "http://127.0.0.1:8080"     # your Docbank daemon; the port is an example
api_key_env = "DOCBANK_API_KEY"   # daemon environment variable with the key
all_sources_upload_consent = true # allow audio from every captured source to leave msgvault
# voice_transcript_search = true  # include generated Beeper voice transcripts in keyword search
# asr_profile = "asr"             # optional Docbank profile for audio without source text
# recording_search_profile = "supplied-transcript" # Docbank profile whose transcript embeddings semantic search uses
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Schedule the stored-media job in `msgvault serve` |
| `url` | — | Docbank base URL: HTTPS, or HTTP on a loopback address. User info, query strings and fragments are rejected |
| `trust_private_network` | `false` | Also allow HTTP to DNS names and private, link-local, unspecified or CGNAT addresses on a network you trust; public literal IPs stay refused |
| `api_key_env` | — | Name of the daemon environment variable that holds the Docbank API key. It is read for each request and sent as `X-Api-Key` |
| `api_key` | — | Inline Docbank API key; takes priority over file and environment sources |
| `api_key_file` | — | Owner-only Docbank key file, read for each request; takes priority over `api_key_env` |
| `all_sources_upload_consent` | `false` | Allow stored audio and explicit source transcripts from every captured source, including future providers, to be sent to `url`. Without it, uploads stop. If `voice_transcript_search` remains enabled, the job can still read transcripts for previously retained recordings. |
| `voice_transcript_search` | `false` | Include generated Beeper voice memo transcripts in keyword search. Restart the daemon after changing this setting. Disabling it removes saved transcripts from search. |
| `asr_profile` | — | Optional Docbank processing profile for stored audio without usable source text. An empty value retains audio without requesting processing. Msgvault rejects `supplied-transcript`, which Docbank reserves for supplied transcript input. |
| `recording_search_profile` | — | Docbank processing profile whose transcript embeddings `media search --mode semantic` or `--mode hybrid` searches. Use `supplied-transcript` for supplied transcripts embedded by Docbank's `supplied_media_embedding_profile`, or your `asr_profile` for generated transcripts. Does not queue processing or embeddings. Without it, hybrid uses keyword search and semantic fails. Restart the daemon after changing it. |
| `reference_consent` | `false` | Send Loom/Cap links from every message source and native Teams recording pointers to `url` as references. This consent is separate from audio upload consent. |
| `reference_origins` | `[]` | Exact origins for self-hosted Cap, such as `https://cap.example.test`. Include only scheme, host and optional port. Register each origin in Docbank too. |

For containers on a trusted private network, set `url = "http://docbank:8485"`
and `trust_private_network = true` in `[integrations.docbank]`. Keep in mind
that this sends traffic, including your API key, in plaintext on that network.
The Docbank and Kata opt-ins are independent and each defaults to `false`.

With `enabled = true` and `reference_consent = true`, the daemon routes Loom
and Cap links from normalized message text and HTML across every message
source, including existing messages and links behind display text, along with
native Teams recording pointers from typed attachment records. For HTML
messages, it reads actual link destinations and surrounding text, falling back
to the plain body when HTML is absent. Beeper imports and repairs keep HTML
outside quoted replies so links behind display text and updated link targets
stay discoverable. References are submitted with acquisition disabled; Docbank
records `access_required` or `unsupported`, and coverage stays `unprocessed`
until media is imported. Delivery log events include the delivery state along
with Docbank's `source_id` and `occurrence_id` for
`docbank media import-artifact`.

Each run attempts up to 20 deliveries, and HTTP requests time out after 30
seconds. Slow Docbank responses delay backfill, and the scheduler skips
overlapping runs.

If you remove a recording link or delete its message, future submissions stop
once the worker sees the change, though a request already prepared can still
finish. The local `withdrawn` state does not retract an occurrence or delete
media already retained in Docbank; manage those records in Docbank separately.

When a submission is definitively rejected, that reference stays blocked until
you fix the cause and restart the daemon with the enabled, consented route.
Waiting for the next scheduled minute does not unblock it. Transient failures
and uncertain sends keep their scheduled retry and receipt recovery.

The older Beeper-only `upload_consent` key no longer enables uploads. If you
used it before, set `all_sources_upload_consent = true` to resume sending
audio, including Beeper recordings.

Restart the daemon after changing any of these settings, including
`reference_consent` or `reference_origins`. Pointing to a new `url` starts a
separate delivery record while keeping earlier rows. Disabling the route stops
the job and keeps its rows. A failed setup, such as an invalid `url`, does the
same and logs a warning. `all_sources_upload_consent` covers transport only;
the Docbank daemon's processing consent still decides whether a configured
profile can run. The route inspects stored CAS bytes, so MIME claims do not
expand Docbank's WAV and MP3 capability. Capture gaps and unsupported formats
remain typed local states.

msgvault's Docbank audio route delivers files up to 1 GiB even if a source's
capture limit is higher; larger files remain in your msgvault archive.

Agents can quote transcripts of delivered files in
[Kata issues](usage/kata-issues.md#cite-a-docbank-transcript).

### `[inline]`

Archive [all accessible Inline chats](usage/inline.md) by default, including
hidden and archived chats and accessible child threads. Run `add-inline` to
verify your account and add it to `[[inline.accounts]]`. You can optionally
limit capture to specific chat IDs. One `[inline]` block controls scheduling
and media policy for all registered Inline accounts, while credentials live
outside `config.toml`. Restart the daemon after changing scheduled accounts or
chat selections.

```toml
[inline]
enabled = true
schedule = "0 */6 * * *"          # optional; omitted = manual sync only
media = true
media_scope = "all"
media_max_participants = 20       # unknown group counts also skip; 0 = no cap
max_media_mb = 250

[[inline.accounts]]
identifier = "api.inline.chat:user:42" # verified by add-inline
transport = "mcp"                # mcp (default) or cli
chat_ids = []                     # empty or omitted = all accessible chats
# chat_ids = ["123", "456"]     # optional exact filter; no implied children
# endpoint = "https://mcp.inline.chat/mcp/v2"
# cli_path = "inline"            # CLI transport only; daemon-host executable

[inline.accounts_config."api.inline.chat:user:42"]
media = true
max_media_mb = 500
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Whether the daemon schedules Inline sync |
| `schedule` | — | Cron expression used by `msgvault serve`; no schedule means manual sync only |
| `media` | `true` | Download attachment bytes; deferred media retains metadata |
| `media_scope` | `all` | `all`, `direct`, or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip media above this participant count or when the effective group count is unknown; explicit `0` removes the cap |
| `max_media_mb` | `250` | Per-attachment download cap in MiB |
| `accounts_config` | — | Per-canonical-account-identifier `media` and `max_media_mb` overrides |

Each `[[inline.accounts]]` entry has:

| Key | Default | Description |
|---|---|---|
| `identifier` | required | Canonical authenticated account identifier written by `add-inline` |
| `transport` | `mcp` | OAuth MCP or the explicitly selected `cli` transport; there is no automatic fallback |
| `chat_ids` | all accessible chats | Optional numeric chat IDs as strings for an exact filter; empty or omitted discovers all chats, while filtered child chats must be selected separately |
| `endpoint` | `https://mcp.inline.chat/mcp/v2` | Only this production URL is accepted; omit the key to use the default |
| `cli_path` | `inline` | CLI executable path on the daemon host, used only for `transport = "cli"` |

An explicit `chat_ids` filter pauses future capture outside those chats without
removing already archived records. Running `add-inline` again with chat IDs
merges them with your existing filter, while running it without IDs restores
all-chat mode. Switching transports keeps the same canonical account,
conversation, and message identities. All-chat mode requires a complete catalog
from the deployed Inline backend and MCP server or CLI; deploy the backend
first, because an older backend can ignore the subthread request and silently
omit child chats. Unsupported CLI commands and malformed MCP catalogs fail
explicitly. Review [connection and capture limits](usage/inline.md) before
setting media caps or configuring a remote daemon.

### `[[facebook_comments]]` {#facebook-comments}

Configure a Facebook Page for comment ingestion. For token setup, coverage, and
recovery, see [Facebook Page comments](usage/facebook-comments.md). msgvault
reads tokens only from the named daemon environment variable.

```toml
[[facebook_comments]]
identifier = "example-page"
app_id = "9"
page_id = "100"
token_env = "EXAMPLE_FACEBOOK_PAGE_TOKEN"
enabled = true
schedule = "*/30 * * * *"
```

| Key | Default | Description |
|---|---|---|
| `identifier` | required | Local profile label, unique ignoring case; renaming it preserves archive identity |
| `app_id` | required | Decimal Meta app ID; paired with `page_id` for source and actor identity |
| `page_id` | required | Decimal selected Page ID; verified against the supplied Page token before registration |
| `token_env` | required | Explicit environment variable name on the daemon host; no token values or fallback variables |
| `enabled` | `false` | Enable scheduled sync; manual commands remain available |
| `schedule` | empty | Five-field cron expression; empty means manual sync only |
| `actions` | empty | Comment changes allowed on Meta: `"hide"` (also allows unhide), `"delete"`, and `"reply"`. Empty means read only. See [Hide or delete comments](usage/facebook-comments.md#hide-or-delete-comments) and [Reply to comments](usage/facebook-comments.md#reply-to-comments) |

msgvault trims `identifier` and requires it to be nonempty and case-insensitively
unique. Both `app_id` and `page_id` must be positive decimal IDs with no leading
zeros. Duplicate app/Page pairs, invalid environment variable names, unknown
keys, unknown actions, and invalid schedules are rejected. The canonical source
identifier is `app:<app_id>:page:<page_id>`, and scheduled jobs run as
`facebook_comments:<source-identifier>`, separate from email account dispatch.

### `[[facebook_messages]]` and `[[instagram_messages]]` {#meta-inboxes}

Configure a Page's Messenger inbox or a linked Instagram account's inbox.
See [Facebook and Instagram messages](usage/meta-messages.md).
`[[facebook_messages]]` follows the same keys and rules as
[`[[facebook_comments]]`](#facebook-comments), and `[[instagram_messages]]`
follows [`[[instagram_comments]]`](#instagram-comments), except that neither
supports `actions` or `mentions`. Scheduled jobs are named
`facebook_messages:<source-identifier>` and
`instagram_messages:<source-identifier>`.

### `[meta_webhooks]` {#meta-webhooks}

Run a listener for signed Meta webhook deliveries; see
[Meta webhooks](usage/meta-webhooks.md) for setup. Set `listen_addr` to a
`host:port` address, or leave it empty to keep the receiver off. Each
`[[meta_webhooks.apps]]` entry requires a decimal `app_id` plus the daemon
environment variable names holding the app secret (`app_secret_env`) and verify
token (`verify_token_env`). App IDs must be unique, setting `listen_addr`
requires at least one app, and unknown keys are rejected.

### `[[instagram_comments]]` {#instagram-comments}

Configure a professional Instagram account linked to a specific Facebook Page.
For setup and coverage, see [Instagram comments](usage/instagram-comments.md).
Each profile requires `identifier`, `app_id`, `page_id`,
`instagram_account_id`, and `token_env`. `enabled` defaults to `false`, an
empty `schedule` means manual sync only, and `actions` works the same way as in
`[[facebook_comments]]`. To also archive comments on the account's ads, set
`ad_account_id` (decimal, without `act_`) and `ad_token_env` together; see
[Include ad media](usage/instagram-comments.md#include-ad-media).

msgvault trims `identifier` and requires it to be nonempty and case-insensitively
unique; renaming it preserves archive identity. All three IDs must be positive
decimal IDs with no leading zeros. Duplicate app/Page/Instagram tuples, unknown
keys, invalid environment variable names, and invalid five-field cron
expressions are rejected. Tokens come only from the named daemon environment
variable. The canonical source identifier is
`app:<app_id>:page:<page_id>:instagram:<instagram_account_id>`, and its generic
scheduler job is `instagram_comments:<source-identifier>`.

To also archive media where other accounts tag this account, set
`mentions = true` (default `false`). Mentions sync into a separate
`instagram_mentions` source with the same identifier, and when the profile has
a schedule, the scheduler job is `instagram_mentions:<source-identifier>`. See
[Archive media that tags the account](usage/instagram-comments.md#archive-media-that-tags-the-account).

### `[feeds]` {#feeds}

Schedule fetches for the RSS, Atom, and JSON feeds you follow with
`msgvault source feeds add`. The feed list itself lives in the archive rather
than `config.toml`; see [Feeds](usage/feeds.md).

```toml
[feeds]
enabled = true                    # gate for the daemon schedule
schedule = "*/30 * * * *"         # 5-field cron; empty = manual sync only
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Whether the daemon schedules feed sync |
| `schedule` | — | Cron expression used by `msgvault serve` |

### `[[chatwoot]]` {#chatwoot}

Configure one account per Chatwoot instance. Each registered inbox becomes its
own archive source. For token and inbox registration, see
[Chatwoot setup](guides/chatwoot.md).

```toml
[[chatwoot]]
identifier = "support"
url = "https://chatwoot.example.com"
account_id = 9
api_key_env = "MSGVAULT_CHATWOOT_TOKEN"
enabled = true
schedule = "*/30 * * * *"
inboxes = []
exclude_inboxes = []
self_agent_ids = []
include_private = false
media = true
max_media_mb = 250
recording_max_bytes = 4294967296   # 4 GiB per call recording
reconcile_interval_hours = 24
```

| Key | Default | Description |
|---|---|---|
| `identifier` | required | Unique local profile label for commands; does not determine archive identity |
| `url` | required | Absolute instance URL, including a deployment subpath if needed; HTTPS is required off loopback, while HTTP is allowed for localhost and loopback IPs; credentials, query, and fragment are rejected |
| `account_id` | required | Positive numeric Chatwoot account ID |
| `api_key_env` | `MSGVAULT_CHATWOOT_TOKEN` | Environment variable containing the user token on the daemon host |
| `enabled` | `false` | Enable scheduled sync; manual commands remain available |
| `schedule` | — | Five-field cron expression; empty means manual sync only |
| `inboxes` | all accessible | Positive inbox IDs to register and sync |
| `exclude_inboxes` | — | Positive inbox IDs to exclude; wins over `inboxes` |
| `self_agent_ids` | — | Positive user IDs representing the archive owner; outgoing replies alone do not establish ownership |
| `include_private` | `false` | Include private notes; turning it on later and running `sync-chatwoot --full` backfills the notes already skipped. Sync refuses to run with it off once private notes are archived |
| `media` | `true` | Download attachment and call-recording bytes; retain metadata and source transcripts when disabled |
| `max_media_mb` | `250` | Cap in MiB for media other than call recordings; zero selects the default; negative/overflowing values are rejected |
| `recording_max_bytes` | `4294967296` (4 GiB) | Per-file cap in bytes for audio on voice-call messages and linked call meetings; zero selects the default; negative values and `MaxInt64` are rejected. Does not enable media downloads or Docbank uploads |
| `reconcile_interval_hours` | `24` | Interval for listing every conversation and rereading histories with commit-time updates since the previous reconcile began; zero selects the default; negative/overflowing values are rejected |

msgvault rejects duplicate profile labels (case-insensitively) and duplicate
canonical instance/account pairs. Normalization of scheme, host, default port,
and trailing slashes keeps instance identities consistent. Add another
`[[chatwoot]]` block for each additional account or instance.

### `[slack]`

Archive [Slack workspaces](/docs/usage/slack/). Authorize each workspace first
with `msgvault add-slack`; tokens stay in per-workspace files, and this single
block controls every registered workspace.

```toml
[slack]
enabled = true                    # gate for the daemon schedule
schedule = "*/30 * * * *"         # 5-field cron; empty = manual sync only
channels = []                     # channel-name include filter (empty = all available)
exclude_channels = []             # channel names to skip, e.g. ["noise"]
private_channels = true           # sync private channels
dms = true                        # sync one-to-one direct messages
group_dms = true                  # sync group direct messages
media = true                      # download shared-file bytes
media_scope = "all"               # all, direct, or none
media_max_participants = 20       # skip files from larger channels; 0 = no cap
max_media_mb = 250                # per-file download cap (MiB)

# [slack.accounts_config.T0123456]  # per-workspace override, keyed by team ID
# media = false
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Whether the daemon schedules Slack sync |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `channels` | all | Channel names to sync (include filter; never applies to DMs or group DMs) |
| `exclude_channels` | — | Channel names to skip (wins over `channels`) |
| `private_channels` | `true` | Sync private channels; `false` pauses them without removing archived messages or affecting DMs |
| `dms` | `true` | Sync one-to-one DMs; `false` pauses them without removing archived messages |
| `group_dms` | `true` | Sync group DMs; `false` pauses them without removing archived messages |
| `media` | `true` | Download shared-file bytes (failed downloads retry via `backfill-slack-media`) |
| `media_scope` | `all` | `all`, `direct` (DMs and group DMs only), or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip files from conversations above this many members; `0` = no cap |
| `max_media_mb` | `250` | Per-file download cap in MiB (over-cap files are recorded as a `size_cap` skip and retried only after the cap changes) |
| `accounts_config` | — | Per-team-ID `media` and `max_media_mb` overrides |

To archive public channels only, set `private_channels`, `dms`, and `group_dms`
to `false`. These keys filter what sync archives, while your Slack token
determines what msgvault can read. For a token scoped to public channels, see
[Slack permissions](/docs/usage/slack/#prerequisites). A restricted token lists
all public channels (including ones you have not joined), whereas broader
tokens list your memberships.

### `[teams]`

Configure media policy for [Microsoft Teams](/docs/usage/teams/) chats and
channels. Teams sync schedules live under `[[accounts]]`; `[teams]` controls
only which attachments msgvault downloads.

```toml
[teams]
media = true
media_scope = "all"
media_max_participants = 20
max_media_mb = 250

[teams.accounts_config."user@example.com"]
media = true
max_media_mb = 500
```

| Key | Default | Description |
|---|---|---|
| `media` | `true` | Download attachment and inline hosted-content bytes (failed downloads retry via `backfill-teams-media`) |
| `media_scope` | `all` | `all`, `direct` (chats only, not channels), or `none`; see [Media policy](#media-policy) |
| `media_max_participants` | `20` | Skip media from chats and channels above this many members; `0` = no cap |
| `max_media_mb` | `250` | Per-attachment download cap in MiB |
| `accounts_config` | — | Per-account overrides of `media` and `max_media_mb`, keyed by the Teams account email |

### Apple WhatsApp sources

Keep WhatsApp for Mac synced with `msgvault serve`. Add one entry per owner
phone number; `path` defaults to the native WhatsApp `ChatStorage.sqlite`.
Grant **Full Disk Access** to the process running the daemon. These imports
copy message text and metadata only, not media.

```toml
[[whatsapp_apple]]
name = "personal"
enabled = true
phone = "+15550000000"
display_name = "Me"
schedule = "*/10 * * * *"
# path = "~/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"
```

`phone` must start with `+`. `path` expands `~` and resolves relative paths
against the configuration directory when you load config with `--config`. The
**Sources** page shows run history and offers **Sync now** for scheduled
stores.

### `[imessage]`

Schedule imports with `msgvault serve` and set media limits for
[`import-imessage`](/docs/usage/text-messages/). Passing `--max-media-mb`
overrides `max_media_mb` for one run, and
[`purge-excluded-media`](cli-reference.md#purge-excluded-media) uses the same
keys to remove stored iMessage attachments above those limits.

```toml
[imessage]
enabled = true
schedule = "*/10 * * * *"
# db_path = "~/Library/Messages/chat.db"
# me = "+15550000000"
media_max_participants = 20
max_media_mb = 250
```

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Enable daemon imports when a schedule is set |
| `schedule` | — | Five-field cron schedule; Sources also offers **Sync now** |
| `db_path` | `~/Library/Messages/chat.db` | Source database; accepts `~` and paths relative to the configuration directory when loaded with `--config` |
| `me` | — | Your phone or email for sender and recipient attribution |
| `media_max_participants` | `20` | Skip media from chats with more participants than this, counting you; `0` = no cap |
| `max_media_mb` | `250` | Per-attachment size cap in MiB |

Grant **Full Disk Access** to the process running the daemon. Each run re-reads
all of `chat.db` and writes only what changed, including late arrivals and
edits. Choose a schedule interval that allows for the time it takes to read
your database.

### Omi sources

Add one `[[omi]]` entry per hosted or self-hosted Omi account and register each
with `msgvault add-omi`; see [Omi setup](usage/meetings.md#omi). Use HTTPS for
non-loopback backends; plain HTTP is allowed only for loopback hosts such as
`localhost`, `127.0.0.1`, or `[::1]`.

```toml
[[omi]]
identifier = "omi-personal"
account_email = "you@example.com"
api_key = "omi_dev_..."
# base_url = "http://localhost:8000" # self-hosted backend root
schedule = "0 */6 * * *"
enabled = true
```

| Field | Default | Description |
|---|---|---|
| `identifier` | `default` for a single entry | Stable source label; required and unique when multiple entries exist |
| `account_email` | required | Primary account identity; independent of the source label |
| `api_key` | required for add/sync | Developer API key with `conversations:read` on this backend |
| `base_url` | `https://api.omi.me` | HTTPS backend root, or HTTP loopback root, with optional proxy prefix; omit `/v1/dev`; credentials, query, and fragment are rejected |
| `schedule` | empty | Five-field cron expression; empty disables daemon scheduling |
| `enabled` | `false` | Enables scheduled sync when a schedule is present |

You can run manual CLI syncs on any entry regardless of `enabled`. Store Omi
credentials in the daemon's configuration, including when you connect from a
remote CLI.

Each Omi sync pass runs for up to five minutes and can resume where it left
off. Scheduled runs continue unfinished creation-date windows, and if a
provider cooldown lasts longer than a pass, sync waits for the next scheduled
trigger. Manual commands print instructions for continuing. See
[Omi sync behavior](usage/meetings.md#how-omi-sync-runs).

### Granola sources

Add one top-level `[[granola]]` entry per Granola account. Register each
account first with `msgvault add-granola`. `identifier` sets a stable source
label, `account_email` sets the primary identity used for organizer
attribution, and `msgvault serve` runs the entry on its cron schedule. See
[Meeting Transcripts](/docs/usage/meetings/).

```toml
[[granola]]
identifier = "work"              # stable label; defaults to "default" for a single entry
account_email = "you@example.com" # required primary account identity
api_key = "grn_..."              # from the desktop app's settings (Business plan)
schedule = "0 */6 * * *"         # 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by `sync-granola <identifier>` and scheduler logs |
| `account_email` | (required) | Normalized primary account identity used for `is_from_me` |
| `api_key` | (required) | Granola API key (`grn_…`) |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

Config loading keeps `identifier` as written and rejects entries without an
effective email, prompting you to add `account_email`. Running
`msgvault add-granola` confirms the primary identity even if aliases already
exist. Manage aliases with `msgvault identity add <identifier> <email>`, then
run `msgvault sync-granola <identifier> --full` after identity changes to
repair existing meeting attribution. A scheduled source must stay registered in
the archive; if you remove it, the scheduler does not silently recreate it.

### Plaud sources

Add one top-level `[[plaud]]` entry per Plaud cloud account. Browser OAuth
stores credentials outside `config.toml`. Turn on Cloud Sync and transcription
in Plaud before syncing. For setup and preservation rules, see the
[meeting guide](usage/meetings.md#plaud).

```toml
[[plaud]]
identifier = "work"
account_email = "you@example.com"
schedule = "30 */6 * * *"
limit = 50
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` for one unnamed entry | Stable command, source, and token label; must be unique and contain no path separators, control characters, or surrounding whitespace |
| `account_email` | Required | Explicit account email; normalized to lowercase and checked against live Plaud identity |
| `endpoint` | `https://mcp.plaud.ai/mcp` | MCP resource endpoint override; changing it requires new authorization |
| `schedule` | — | Five-field cron expression used by `msgvault serve` |
| `limit` | `0` (all) | Most recordings a scheduled run checks; works like `sync-plaud --limit` |
| `enabled` | `false` | Whether a scheduled entry runs in the daemon |

Authorize on the daemon host with `msgvault add-plaud <identifier>`. The OAuth
callback listens on `localhost:8091/callback/plaud`, and tokens are saved to
`tokens/plaud_<identifier>.json`. An existing source keeps its confirmed owner
even if configuration changes. Use a new identifier for another account. A
scheduled entry must stay registered; removing the source stops sync from
recreating it automatically.

### Twenty sources

Add top-level `[[twenty]]` entries on the daemon host to archive call
recordings from Twenty workspaces using read-only API keys. Register each
source with `msgvault add-twenty` before syncing or scheduling it. For required
API permissions, see
[Meeting Transcripts](/docs/usage/meetings/#twenty-call-recordings).

```toml
[[twenty]]
identifier = "work"
account_email = "you@example.com"
base_url = "https://api.twenty.com"
api_key = "YOUR_READ_ONLY_API_KEY"
schedule = "15 */6 * * *"
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Stable label for commands and the `twenty:<identifier>` scheduler job |
| `account_email` | (required) | Actual primary email for account identity and organizer attribution |
| `base_url` | (required) | API root: `https://api.twenty.com` for Cloud, or the self-hosted instance origin |
| `api_key` | (required at registration/sync) | API key with read access to recordings, calendar events, and participants |
| `schedule` | — | Five-field cron expression used by `msgvault serve` |
| `enabled` | `false` | Opt into daemon scheduling; manual sync remains available |

Origins require HTTPS except loopback HTTP and cannot include credentials,
query strings, fragments, or path prefixes. Redirects are rejected. Identifiers
must be case-insensitively unique, and each entry requires `account_email`. Enabled
entries without a `schedule` are not scheduled, and removing a registered
source stops scheduled sync from silently recreating it.

### Pocket sources

Add top-level `[[pocket]]` entries to sync Pocket accounts.

```toml
[[pocket]]
identifier = "personal"
account_email = "you@example.com"
api_key_env = "POCKET_API_KEY"
enabled = true
schedule = "15 */6 * * *"
```

| Field | Default | Purpose |
|---|---|---|
| `identifier` | `default` for one unnamed entry | Stable source label; multiple entries require unique labels |
| `account_email` | Required | Explicit primary email; must match the live Pocket account |
| `api_key_env` | `POCKET_API_KEY` | Environment variable holding a personal API key on the daemon host |
| `enabled` | `false` | Enable scheduled sync |
| `schedule` | Unset | Cron expression; an enabled source without a schedule is not synced automatically |

Set the API key in the daemon's environment and restart the daemon after
changing it; a variable set only in a remote CLI shell never reaches the
daemon. msgvault sends the key only to Pocket's fixed REST and MCP endpoints
and never stores it in source configuration or raw meeting evidence.

Run `msgvault add-pocket personal` to verify access and bind the source to the
authenticated email and stable Pocket user ID. Replacing the key for the same
account keeps working, while a different account requires a new source label
even if the email matches. Removing an account does not revoke its Pocket key.
For coverage and compatibility limits, see the
[Pocket workflow](usage/meetings.md#pocket).

### Circleback sources

Add top-level `[[circleback]]` entries for Circleback meeting sync. Authorize
through browser OAuth with `msgvault add-circleback`; no secret lives in
`config.toml`. See [Meeting Transcripts](/docs/usage/meetings/).

```toml
[[circleback]]
identifier = "work"              # stable label/token key; defaults to "default" for one entry
account_email = "you@example.com" # required primary account identity
schedule = "30 */6 * * *"        # 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by `sync-circleback <identifier>`, the token filename, and scheduler logs |
| `account_email` | (required) | Normalized primary account identity used for `is_from_me` |
| `endpoint` | production | MCP endpoint override (testing only) |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

Config loading and alias repair work the same way as Granola: keep the label,
set `account_email`, manage aliases with `msgvault identity`, and run
`msgvault sync-circleback <identifier> --full` after identity changes.
Circleback OAuth always confirms the primary identity without an opt-out flag.

### Notion AI Meeting Notes sources

Add one top-level `[[notion_meetings]]` entry per Notion identity. The
meeting token needs **AI Meeting Notes** and **Read Content** access. A
personal access token (PAT) can read its own user's meetings but cannot list or
look up other users. To resolve Notion attendee IDs, also configure a separate
internal integration in the same workspace with **Read user information
including email addresses**.

```toml
[[notion_meetings]]
identifier = "notion-personal"      # stable source label; defaults to "default" for one entry
account_email = "you@example.com"   # required primary account identity
token = "ntn_..."                   # meeting token; keep this file private
users_token = "ntn_..."             # optional workspace users integration token
schedule = "15 */6 * * *"           # optional 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by commands and scheduler logs |
| `account_email` | (required) | Normalized primary identity for relationships; it is not assumed to be the meeting organizer |
| `token` | (required) | Meeting token; PAT or integration with Meeting Notes and Read Content access |
| `users_token` | — | Optional workspace integration token with Read user information including email addresses |
| `schedule` | — | Cron expression used by `msgvault serve` |
| `enabled` | `false` | Whether the source is daemon-scheduled |

Both manual and scheduled syncs use the optional `users_token` from config, and
msgvault keeps token values out of diagnostics and archived evidence. For
lookup timeouts, failures, and retries, see
[Notion attendee emails](https://msgvault.io/docs/usage/meetings/#notion-attendee-emails).

Run `msgvault add-notion-meetings <identifier>` to verify access and register
the source before turning on a schedule. Removing the source stops the
scheduler from recreating it. See [Meeting Transcripts](/docs/usage/meetings/)
for the 50-result discovery limit, attendee visibility, transcript retries, and
stored data.

### Twilio sources

Add one `[[twilio]]` entry per Twilio account or subaccount. For what sync
stores, see the [Twilio meeting guide](usage/meetings.md#twilio).

```toml
[[twilio]]
identifier = "work"
account_email = "you@example.com"
account_sid = "AC00000000000000000000000000000001"
api_key_sid = "SK00000000000000000000000000000001"
api_key_secret = "your-key-secret"
region = "us1"
enabled = true
schedule = "15 */6 * * *"
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Stable source label; required with several entries |
| `account_email` | Required | Your primary identity; not treated as a caller |
| `account_sid` | Required | Account or subaccount SID |
| `api_key_sid`, `api_key_secret` | — | API key credentials, set together |
| `auth_token` | — | Account auth token instead of an API key |
| `region` | `us1` | `us1`, `ie1` or `au1`; credentials must belong to that region |
| `intelligence_service_sid` | — | Only read Conversation Intelligence transcripts from this service; without it, transcripts from every service are read |
| `enabled` | `false` | Allow daemon scheduling |
| `schedule` | — | Five-field cron expression |
| `media` | `true` | Download recordings; `false` archives calls and transcripts only. After turning it back on, run `sync-twilio --full` to fetch the skipped recordings |
| `max_media_mb` | `1024` (1 GiB) | Per-recording size cap in MiB; `0` uses the default. Oversized recordings are skipped; raise the cap and run `sync-twilio --full` to retry |

### Bland sources

Add one `[[bland]]` entry per Bland org API key. For what sync stores, see the
[Bland meeting guide](usage/meetings.md#bland).

```toml
[[bland]]
identifier = "work"
account_email = "you@example.com"
api_key = "your-org-api-key"
enabled = true
schedule = "15 */6 * * *"
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Stable source label; required with several entries |
| `account_email` | Required | Your primary identity; not treated as a caller |
| `api_key` | Required | Org API key |
| `encrypted_key` | — | Key for your own Twilio account connected to Bland. With it, Bland returns only inbound numbers associated with that account SID |
| `enabled` | `false` | Allow daemon scheduling |
| `schedule` | — | Five-field cron expression |
| `media` | `true` | Download recordings; `false` archives calls and transcripts only. After turning it back on, run `sync-bland --full` to fetch the skipped recordings |
| `max_media_mb` | `1024` (1 GiB) | Per-recording size cap in MiB, shared with Twilio's default; `0` uses the default. Oversized recordings are skipped; raise the cap and run `sync-bland --full` to retry |

### Muesli sources

Add one top-level `[[muesli]]` entry per Muesli database on the Mac where
Muesli records. When `[remote]` is configured, the native client reads the
local database and Contacts and sends selected meetings with your existing
remote API credentials. Otherwise, the daemon reads the files on its own host.

```toml
[[muesli]]
identifier = "mac"                  # stable source label; defaults to "default" for one entry
account_email = "you@example.com"   # required; you, the person who records
db_path = "~/Library/Application Support/Muesli/muesli.db"  # optional; this is the default
recordings = false                 # opt in to retained M4A/WAV audio
recording_max_bytes = 4294967296    # optional; 4 GiB per file by default
phone_country_code = "1"            # optional; convert national-format Contacts phones
schedule = "*/30 * * * *"           # optional 5-field cron, no seconds
enabled = true
```

| Key | Default | Description |
|---|---|---|
| `identifier` | `default` (single entry) | Source name used by `sync-muesli <identifier>` and scheduler logs |
| `account_email` | (required) | Normalized primary identity; attributed as the organizer of every meeting |
| `db_path` | `~/Library/Application Support/Muesli/muesli.db` | Muesli database path; `~` expands, and a relative path resolves against the config directory when `--config` is used |
| `recordings` | `false` | Archive retained M4A or WAV audio as meeting attachments; works in same-host and native remote mode |
| `recording_max_bytes` | `4294967296` (4 GiB) | Per-file recording cap; `0` uses the default. Increase it and the remote daemon's cap to retry oversized files |
| `contacts` | `true` | Resolve attendees through Apple Contacts; the process reading the files needs Full Disk Access |
| `contacts_path` | `~/Library/Application Support/AddressBook` | Apple Contacts data folder; expands like `db_path` |
| `phone_country_code` | — | Country calling code (1–3 digits, such as `"1"` or `"44"`) used for Contacts phone numbers typed without one; unset means only international numbers are used |
| `schedule` | — | 5-field cron used by the local daemon or recorder-side `sync-muesli --watch` |
| `enabled` | `false` | Whether automatic rescanning includes this source |

Run `msgvault add-muesli <identifier>` to check the database and register the
source before enabling a schedule. On a remote recorder, keep
`msgvault sync-muesli --watch` running so enabled schedules execute. After
identity changes, run `msgvault sync-muesli <identifier> --full` to repair
existing meeting attribution. Removing the source stops the scheduler from
recreating it. See [Meeting Transcripts](/docs/usage/meetings/#muesli) for what
sync stores.

### `[vector]`

Configure semantic and hybrid search. SQLite vector search requires a build
with `sqlite_vec` support (the default with `make build`), while PostgreSQL
vector search requires a build with the `pgvector` tag and a PostgreSQL
`[data].database_url`. For prerequisites, initial embedding, and the full
workflow, see [Vector Search](/docs/usage/vector-search/).

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Turn on vector and hybrid search. When `false`, `mode=vector` and `mode=hybrid` return `vector_not_enabled`. |
| `backend` | `sqlite-vec` | Backend marker. Supported values are `sqlite-vec` and `pgvector`; the concrete backend is selected from `[data].database_url`. |
| `db_path` | `<data_dir>/vectors.db` | SQLite vector database path. Ignored by the PostgreSQL pgvector backend. |
| `skip_extension_create` | `false` | PostgreSQL only. Skip `CREATE EXTENSION IF NOT EXISTS vector` when pgvector is already installed by an administrator. |

#### `[vector.embeddings]`

Text embeddings use either a configured endpoint or the shared local
`kennembed` server. To install the local server (after confirming any missing
downloads), run `msgvault setup providers --local-embeddings`. Local requests
ignore saved and environment API keys. Local and self-hosted endpoints keep
text on your machine or network, whereas hosted endpoints receive the text
being embedded.

| Key | Default | Description |
|---|---|---|
| `api_format` | `openai` | Request contract: `openai` (OpenAI-compatible `/embeddings`, one vector per message chunk) or `voyage-contextual` (Voyage `/contextualizedembeddings`; pins `model = "voyage-context-4"` and embeds chat conversation windows and turn-aware meeting chunks as contextual documents). |
| `local_only` | `false` | Use the shared local server, even while Ollama is running. Setup installs its model and runtime; subsequent commands reuse them. |
| `endpoint` | (required unless `local_only = true`) | HTTP(S) base URL for an OpenAI-compatible embeddings API. msgvault appends `/embeddings` (for example, set `http://localhost:11434/v1`, not `.../embeddings`). |
| `model` | (required) | Model name or serving alias to pass in each request (e.g., `nomic-embed-text`). Use a new alias when the served weights or vector recipe change; msgvault does not verify the checkpoint behind an alias. |
| `dimension` | (required) | Expected vector width. Responses with a different width are rejected. The OpenAI-compatible client does not send a `dimensions` field or slice returned vectors; configure the server to return the desired width. |
| `document_prefix` | `""` | Model-specific instruction prepended to every document chunk after chunking (for example, `"search_document: "` for `nomic-embed-text`). The prefix does not reduce `max_input_chars`; maximum 4096 UTF-8 bytes. |
| `query_prefix` | `""` | Model-specific instruction prepended to every vector-search query (for example, `"search_query: "` for `nomic-embed-text`); maximum 4096 UTF-8 bytes. |
| `api_key_env` | — | Name of an environment variable containing the API key. Omit for anonymous endpoints. |
| `batch_size` | `32`, or `8` for `local_only` | Embedding inputs per HTTP call. Long messages can contribute multiple chunk inputs. |
| `timeout` | `30s`, or `5m` for `local_only` | Per-request timeout. |
| `max_retries` | `3` | Retries per batch on transient failures. |
| `max_input_chars` | `32768` | Character cap per embedding chunk, counted in characters rather than tokens. Too high and chunks are rejected or silently truncated; too low and long messages split into more chunks, adding embedding overhead. For example, start around `6000` for a 2k-token model such as Ollama's `nomic-embed-text`, then check representative content. See [Matching `max_input_chars` to your embedder's context window](usage/vector-search.md#matching-max_input_chars-to-your-embedders-context-window). |
| `eta_window` | `10` | Number of completed batches used for foreground throughput and ETA smoothing; daemon status retains at most 20. Zero or negative values use 10. |

For a native 768-dimensional EmbeddingGemma 2 setup, see the
[EmbeddingGemma 2 text example](usage/vector-search.md#optional-embeddinggemma-2-text-endpoint).

##### Stored provider credentials

Instead of setting `api_key_env`, you can save a provider API key in
**Settings** in the Web UI or TUI. Stored keys live in
`tokens/provider-credentials.json` under your data directory with owner-only
permissions and are never written to `config.toml`. After you save a key,
**Settings** displays only a masked hint (its first three and last three
characters) and where it comes from (the credential store or the environment),
never the key itself.

A stored key takes precedence over `api_key_env` and is bound to the exact
endpoint origin (scheme, host, and port) you saved it for. If you later change
the endpoint to a different origin, msgvault removes the stored key
automatically and you must enter it again, so a key is never sent to a host it
was not entered for.

Like other `[vector]` settings, changing a stored key for vector or multimodal
(visual) embeddings requires a daemon restart. Person enrichment and sweep keys
take effect on the next run.

A stored person-enrichment suppression key also takes precedence over a custom
`suppression_key_env` (before 0.22.0, the custom environment variable won).
When upgrading an install that has both, make sure the stored key matches the
environment variable's value; if existing suppression records were created with a
different key, enrichment stops with `ErrSuppressionKeyMismatch`.

The host owner can also install keys without the Web UI:

```sh
msgvault credentials set vector.embeddings --from-file /run/secrets/embedding-key
msgvault credentials set vector.multimodal --stdin < /run/secrets/visual-key
msgvault credentials set people.enrichment/research --endpoint https://api.example.com/search --stdin
msgvault credentials set people.enrichment/suppression --stdin < /run/secrets/suppression-key
msgvault credentials list --json
msgvault credentials import-env
```

`msgvault credentials list` prints IDs and bound origins without secret values.
File input follows the [server secret-file rules](#server), and stdin input is
trimmed and capped at 64 KiB. Suppression keys must be at least 32 bytes after
trimming, or `set` and `import-env` reject them. `--endpoint` defaults to the
configured provider endpoint (suppression keys have no endpoint). `import-env`
copies present configured vector, multimodal, named enrichment, and suppression
environment variables once without overwriting existing stored keys. None of
these commands start the daemon or change provider consent. Run them on the
daemon host, passing `--local` to target a local home if remote access is
configured.

Sweep providers manage their own named profile credentials through
`msgvault person provider add --api-key-stdin` or `--credential-env`; current
sweep runs ignore the legacy `people.sweep` store ID.

The index generation fingerprint includes the model, dimension, document and
query prefixes, preprocessing settings, `max_input_chars`, embedding policy,
and scope. Changing any of those triggers a stale-index error on the next
vector or hybrid query. If you rebuild after changing settings while the daemon
is running, run `msgvault daemon restart` so the daemon picks up the new index.
For an existing account-scoped generation built with CLI flags, set a matching
`[vector.embed.scope].accounts` and restart the daemon, or run
`msgvault embeddings build --full-rebuild`.

#### `[vector.preprocess]`

Control how msgvault normalizes message text before embedding.

| Key | Default | Description |
|---|---|---|
| `strip_quotes` | `true` | Drop quoted reply blocks (`> ...` lines, reply preambles) before embedding. |
| `strip_signatures` | `true` | Drop trailing signature blocks (content after `-- `). |
| `strip_html` | `true` | Convert HTML-only bodies to text and remove HTML markup before embedding. |
| `strip_base64` | `true` | Remove base64/data blobs before HTML stripping so encoded data does not crowd out prose. |
| `strip_url_tracking` | `true` | Remove common tracking parameters such as `utm_*`, `fbclid`, and `gclid` from URLs. |
| `collapse_whitespace` | `true` | Normalize repeated horizontal whitespace and blank lines. |

#### `[vector.search]`

Tune hybrid ranking parameters applied at query time.

| Key | Default | Description |
|---|---|---|
| `rrf_k` | `60` | Reciprocal Rank Fusion constant. Higher values flatten score differences between signals. |
| `k_per_signal` | `100` | Candidate pool size drawn from each signal (BM25 or vector) before fusion. |
| `subject_boost` | `2.0` | Multiplier applied when a query term matches a message's subject line. |
| `max_page_size_hybrid` | `50` | Hard cap on `page_size` for vector/hybrid responses. Set to `0` to disable clamping. |
| `sqlite_accelerator` | `auto` | Use a ready SQLite approximate index. Set to `exact` to keep exhaustive vector search. PostgreSQL ignores this setting. |
| `ann_nprobe` | `8` | SQLite index partitions searched per query. Higher values trade latency for recall. |
| `ann_oversample` | `8` | Approximate candidates requested per result before exact reranking. Range: 1–128. |
| `ann_threads` | CPU count, max `128` | Native worker threads used by `msgvault embeddings optimize`. Range: 1–128. |

Tuning accelerator keys does not change the embedding generation fingerprint,
because they affect only how stored vectors are searched or optimized, not how
text is sent to the embedding provider.

#### `[vector.embed.scope]`

Optionally limit newly built embedding generations to a subset of your archive.
Leaving this section empty embeds the full archive. Setting `message_types`
embeds only matching `messages.message_type` values:

```toml
[vector.embed.scope]
message_types = ["teams"]
```

Scoped generations are intentionally partial. Any vector or hybrid query
against a message-type-scoped index must include a matching `message_type`
filter, such as
`msgvault search "release planning" --mode hybrid --message-type teams`. An
unscoped vector or hybrid query returns `index_scope_mismatch` instead of
treating the partial index as covering the full archive.

| Key | Default | Description |
|---|---|---|
| `message_types` | `[]` (all types) | Embed only messages of these types. |
| `accounts` | `[]` (all accounts) | Embed only these accounts' messages, by canonical account identifier (display names are rejected here — they are not stable identities for a privacy boundary). Resolved to source IDs at startup; an unknown identifier fails vector initialization (or the CLI command). The daemon's scheduled embeds honor this scope, so it also acts as a privacy boundary: unlisted accounts' text is never sent to the embedding endpoint. |

`accounts` and `message_types` combine so both filters apply. Passing
`--account` or `--collection` to `msgvault embeddings build` or `resume`
overrides `accounts` for that single run. Both scope keys are part of the
generation fingerprint, so changing either requires
`msgvault embeddings build --full-rebuild`; because the fingerprint stores
archive-local source IDs, re-adding an account under a new source ID also
requires a rebuild. Unlike message-type scopes, account-scoped indexes do not
reject unscoped searches: out-of-scope accounts have no vector matches and rank
on BM25 alone in hybrid mode.

#### `[vector.embed.schedule]`

Optionally schedule background embedding inside `msgvault serve`. Leaving this
section empty disables scheduled embedding, though you can still run
`msgvault embeddings build` manually.

| Key              | Default | Description                                                                                                                                  |
| ---------------- | ------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `cron`           | —       | 5-field cron expression. Empty string disables the standalone cron.                                                                          |
| `run_after_sync` | `false` | Run an embed pass after successful scheduled Gmail, IMAP, Teams, and Discord syncs. Other sources use the standalone cron or a manual build. |

When `msgvault setup providers` enables a text lane, it writes
`run_after_sync = true` and `cron = "*/15 * * * *"`, while preserving either
key if you already set it explicitly (including `false` and `""`). Text lanes
that are already enabled keep their schedules. See
[Recommended Configuration](usage/recommended-configuration.md).

Automatic message and person indexing runs in five-minute passes. If a
scheduled sync is waiting and indexing has held the work slot for one minute,
the scheduler asks indexing to yield; indexing finishes its current provider
request, saves the response, and resumes behind the waiting work. At five
minutes, the scheduler cancels any request still in flight. Post-sync indexing
starts after the sync releases the work slot.

msgvault saves each completed provider response to the vector store before
sending the next request, so resumed passes and daemon restarts do not repeat
completed work; only unfinished requests run again. See
[scheduler status](api-server.md#get-apiv1schedulerstatus) for queue times.

#### `[vector.people]`

Configure semantic people search, which builds one curated document per durable
person from searchable non-sensitive attributes and embeds it into the
text-search generation. This requires `[vector] enabled = true` and a separate
consent with `msgvault person provider consent --semantic-embeddings --yes`.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Embed curated person documents and serve `msgvault person search`. |
| `retention_posture` | — | Your assertion about the embedding provider's retention; must be explicit (not `unknown`). |
| `training_posture` | — | Your assertion about the embedding provider's training use; must be explicit. |

#### `[vector.multimodal]`

Configure the independently consented visual attachment lane. Voyage requires a probe manifest before uploads. Kit supports Vertex and services that implement its embedding API. Both accept JPEG, PNG, WebP, and MP4 video up to 120 seconds and 20 MiB per file. Vertex limits each image dimension to 16,384 pixels and does not extract audio. A Kit service determines its own image dimension limits.

| Key | Default | Description |
|---|---|---|
| `enabled` | `false` | Turn on the visual lane. |
| `provider` | `voyage` | `voyage`, `kit`, or `vertex`. |
| `endpoint` | `https://api.voyageai.com/v1` | Voyage uses its pinned root. Kit and Vertex use the configured API root. |
| `api_key_env` | `VOYAGE_API_KEY` | Environment variable holding the key. Kit leaves it unset by default. |
| `model` | `voyage-multimodal-3.5` | Voyage requires this model. |
| `dimension` | `1024` | Visual search requires 1024. |
| `capabilities_file` | — | Voyage manifest written by `msgvault multimodal probe --seeds <dir> --out <file> --yes`. |
| `max_context_chars` | `4000` | Owning-message text sent with each Voyage attachment. Kit and Vertex ignore this setting. |
| `include_images` | `true` | Embed still images (JPEG, PNG, WebP). |
| `include_animated_gifs` | `false` | Embed animated GIFs; requires `include_images`; Voyage requires an authorizing manifest. Kit and Vertex do not support animated GIFs. |
| `include_video` | `true` | Embed direct-input MP4 video. |
| `allow_image_queries` | `true` | Allow `multimodal search --image`. |

For Vertex, set up Google Application Default Credentials and configure a supported location (`global`, `us`, or `eu`):

```toml
[vector.multimodal]
enabled = true
provider = "vertex"
endpoint = "https://aiplatform.googleapis.com/v1/projects/project-a/locations/global/publishers/google/models"
model = "gemini-embedding-2"
dimension = 1024
```

For a Kit embedding service, use its API root and an optional key:

```toml
[vector.multimodal]
enabled = true
provider = "kit"
endpoint = "https://embed.example.com/v1"
api_key_env = "EMBED_API_KEY"
model = "gemini-embedding-2"
dimension = 1024
```

`[vector.multimodal.scope]` accepts the same `message_types` and `accounts` keys
as `[vector.embed.scope]`, and `[vector.multimodal.schedule]` accepts the same
`cron` and `run_after_sync` keys as `[vector.embed.schedule]`. Running
`msgvault multimodal build --yes` records consent per generation.

### `[activity]`

Configure the dated activity projection and per-person contact state, which
stores first and last contact, last inbound and last outbound contact,
interaction count, and inferred channel. This deterministic projection powers
"when did we last talk" for every person and runs hourly by default inside
`msgvault serve`. To run it manually, use `msgvault activity build` (add
`--backstop` to rescan the entire archive).

Scheduled projection commits up to ten batches per pass, stops after the
current batch if other scheduled work has waited for a minute, and always stops
at two minutes. A pass that commits progress releases the operation gate and
resumes behind queued work without waiting for the next cron tick as long as
identity revisions stay stable. If an identity change happens after the
continuation chain begins, the pass finishes its bounded run and defers clean
continuation until the next scheduled or manual trigger. If a pass hits the
two-minute limit before committing any batch, it records an error and waits for
the next scheduled or manual trigger instead of retrying the same batch
immediately.

Identity reconciliation and changes to `timezone` or `max_direct_counterparts`
save their progress in the archive. Identity reconciliation keeps its position
across identity changes and rechecks earlier stale messages before finishing,
with completed batches staying committed. Manual builds are not limited to ten
batches or two minutes.

| Key | Default | Description |
|---|---|---|
| `schedule` | `17 * * * *` | 5-field cron used by `msgvault serve`. Empty disables the scheduled job. |
| `timezone` | `UTC` | IANA zone name for day bucketing. `Local` is rejected because the projection keys replay on the persisted zone name. |
| `max_direct_counterparts` | `25` | Largest conversation still projected as direct activity between its participants. |
| `batch_size` | `500` | Messages per projection batch. |

## Overriding the home directory

By default, msgvault stores everything under `~/.msgvault` on macOS and Linux
or `C:\Users\<you>\.msgvault` on Windows. To use a different directory, choose
either option:

- **Per command (`--home` flag).** Pass the path on the command line:
  ```bash
  msgvault sync --home /mnt/data/msgvault
  ```
- **Persistent (`MSGVAULT_HOME` environment variable).** Export the path in
  your shell:
  ```bash
  export MSGVAULT_HOME=/mnt/data/msgvault
  ```

Both options load `config.toml` and store all data (database, tokens, and
attachments) in the directory you specify. When both are set, `--home` takes
priority over `MSGVAULT_HOME`.

The home directory or `[data].data_dir` can be a symlink to an existing
directory; local daemon bookkeeping resolves the symlink and checks ownership
and permissions on the target directory.

## Environment variables

| Variable | Description |
|---|---|
| `MSGVAULT_HOME` | Base directory for all data (default: `~/.msgvault`) |
| `MSGVAULT_BIND_ADDR` | Server bind address or `iface:NAME`; `serve --bind` wins |
| `MSGVAULT_API_PORT` | Server port from `0` to `65535`; `serve --port` wins |
| `MSGVAULT_API_KEY` | Inline server key for this process |
| `MSGVAULT_API_KEY_FILE` | Mounted server key file |
| `MSGVAULT_API_KEY_ENV` | Name of the environment variable holding the server key |
| `MSGVAULT_ALLOW_INSECURE` | Allow unauthenticated non-loopback serving |
| `MSGVAULT_BACKUP_REPO` | Backup repository path |
| `MSGVAULT_CORS_ORIGINS` | Comma-separated browser origins; empty clears the list |
| `MSGVAULT_CORS_CREDENTIALS` | Whether browser CORS requests may use credentials |
| `MSGVAULT_TRUSTED_PROXIES` | Comma-separated proxy IP addresses or CIDRs; empty clears the list |
| `MSGVAULT_REMOTE_URL` | Remote daemon URL for all commands with remote support |
| `MSGVAULT_REMOTE_API_KEY` | Inline remote key for this process; `export-token --api-key` wins |
| `MSGVAULT_REMOTE_API_KEY_FILE` | Mounted remote key file |
| `MSGVAULT_REMOTE_API_KEY_ENV` | Name of the environment variable holding the remote key |
| `MSGVAULT_REMOTE_ALLOW_INSECURE` | Allow plaintext HTTP to the remote daemon |

Environment variables override `config.toml`. Within each server or remote
credential group, setting any of its three environment variables selects that
group's environment sources, with inline winning over file and file winning
over named environment. Passing an empty credential variable fails when that
destination is used. Boolean variables accept Go's `strconv.ParseBool` values
(`true`, `false`, `1`, `0`), and empty or invalid values fail. Origin and proxy
lists trim whitespace and skip empty entries, including trailing commas.
Explicit `export-token --to`, `--api-key`, and `--allow-insecure` flags are
saved even when they match an environment override, whereas environment-only
values are not saved. Passing `--allow-insecure=false` overrides both the
environment and saved config, requires HTTPS, and saves `false` after a
successful export. The setup wizard also saves new choices that match an
override, while keeping existing settings leaves them untouched on disk.

For example, you can start a supervised daemon without a config file:

```sh
MSGVAULT_HOME=/data MSGVAULT_BIND_ADDR=0.0.0.0 MSGVAULT_API_PORT=8080 msgvault serve
```

Persist `/data` across restarts so your archive and generated key survive. The
published stock image is `ghcr.io/kenn-io/msgvault`, which runs as UID/GID
`1000`, uses `/data` as its home, and needs no startup hook or entrypoint
wrapper.

## Usage reporting

msgvault can send general, anonymous usage telemetry, which stays off unless
you opt in or set `TELEMETRY_ENABLED=1`.

Any of these environment variables turns telemetry off, even after you opt in:

- `MSGVAULT_TELEMETRY_ENABLED=0`
- `TELEMETRY_ENABLED=0`
- `DO_NOT_TRACK` set to any value except `0` or `false`
- `CI` set to any value

Set the variable in the environment where `msgvault serve` runs, then restart
any running daemon. Development builds never send telemetry.

## File locations

The default home directory is `~/.msgvault` on macOS and Linux or
`C:\Users\<you>\.msgvault` on Windows. msgvault creates it automatically and
leaves existing directory permissions unchanged. `config.toml` lives in the
home directory unless you select another file with `--config`. The data paths
below live under `[data].data_dir` (which defaults to the home directory), and
`[log].dir` can override the log directory.

| File | Description |
|---|---|
| `<home>/config.toml` | Configuration file |
| `msgvault.db` | SQLite database (system of record when PostgreSQL is not configured) |
| `attachments/` | Content-addressed attachment files |
| `tokens/` | OAuth tokens and stored provider credentials |
| `tokens/server-api-key` | Persisted daemon API key, reused on later loopback and non-loopback starts |
| `logs/` | Structured log files (when [file logging](/docs/configuration/#log) is enabled) |
| `analytics/` | Parquet cache files for Web UI and TUI analytical views |
| `omi/` | Omi request pacing and provider cooldowns per backend and API key, shared by scheduled and manual syncs |

## Example configuration

Copy only the sections you need and replace example paths and credentials.

```toml
[data]
# Base data directory (default: ~/.msgvault)
data_dir = "/path/to/msgvault/data"

# User-requested exports (default: {data_dir}/exports)
export_dir = "/path/to/msgvault/exports"

# Database URL (default: {data_dir}/msgvault.db; PostgreSQL DSN supported)
database_url = "/path/to/msgvault.db"

# Keep attachment content as individual files instead of creating packs.
# loose_attachments = true

[oauth]
# Path to Google OAuth client secrets JSON for browser OAuth
client_secrets = "/path/to/client_secret.json"

# Google service account key for Workspace domain-wide delegation (optional)
# service_account_key = "/path/to/service-account.json"

# Named OAuth apps for Google Workspace orgs (optional)
[oauth.apps.acme]
client_secrets = "/path/to/acme_workspace_secret.json"
# service_account_key = "/path/to/acme_service_account.json"

[microsoft]
# Azure AD app registration client ID (required for M365)
client_id = "your-azure-app-client-id"
# redirect_uri = "http://localhost:8089/callback/microsoft"  # default
# tenant_id = "your-tenant-id"   # optional, default "common"

# Optional source-scoped Fastmail alias inventory.
[[fastmail]]
source_id = 14
api_token = "replace-with-a-Fastmail-API-token"
auto_confirm_identities = false

[discord]
# Per-attachment download cap (default: 50 MiB)
max_media_bytes = 52428800
# Skip attachments from rooms with more than this many participants
# (default: 20; 0 = no cap). Shared by [beeper], [inline], [slack], [teams],
# and [imessage].
media_max_participants = 20
# Trailing edit/delete/reaction repair window (default: seven days)
edit_rescan_window = "168h"

[discord.guilds."123456789012345678"]
# Channel, thread, and forum-post IDs; empty include means all accessible.
include = ["456789012345678901"]
exclude = ["567890123456789012"]

[log]
# Persistent structured file logging (opt-in)
enabled = true
# dir = "/path/to/logs"        # default: <data_dir>/logs
# level = "info"                # debug, info, warn, error
# sql_trace = false             # log every SQL query (verbose)
# sql_slow_ms = 100             # slow query threshold in ms

[sync]
# Gmail API rate limit (requests per second)
rate_limit_qps = 5

[server]
# API server settings (used by `msgvault serve` and `msgvault daemon`)
# api_port is optional; omit it (or set 0) to auto-select an open port that
# clients discover automatically. Set a fixed port for remote/NAS deployments.
api_port = 0
bind_addr = "127.0.0.1"
api_key = "your-secret-key"
daemon_idle_timeout = "20m" # automatic daemon idle timeout; "0s" disables
daemon_auto_restart = "newer" # newer, never, or always
daemon_auto_start = true # false when a supervisor runs msgvault serve

[analytics]
# Daemon-side analytics engine for Web UI, TUI, and aggregate HTTP views:
# "auto" starts on live SQL and switches to DuckDB after cache maintenance.
# "sql" always uses live SQL. "duckdb" requires a usable Parquet cache.
engine = "auto"
# Build a stale/missing cache during daemon startup and after scheduled syncs.
auto_build_cache = true
# Minimum age of a usable cache before a scheduled sync may rebuild it again.
# min_rebuild_interval = "6h"

[backup]
# Default repository for `msgvault backup`.
repo = "~/Backups/msgvault"
zstd_level = 0

[deletion]
# Durable consent for remote deletion execution. Opt in deliberately;
# defaults to false.
remote_enabled = false

[remote]
# Remote msgvault endpoint for CLI remote mode
url = "http://nas-ip:8080"
api_key = "remote-api-key"
allow_insecure = true

# Scheduled sync accounts
[[accounts]]
email = "you@gmail.com"
schedule = "0 * * * *"
enabled = true

[vector]
# Semantic and hybrid search (opt-in)
enabled = true
backend = "sqlite-vec"
# backend = "pgvector"  # with a PostgreSQL database_url and pgvector build

[vector.embeddings]
endpoint = "http://localhost:11434/v1"
model = "nomic-embed-text"
dimension = 768
document_prefix = "search_document: "
query_prefix = "search_query: "
eta_window = 10

[vector.preprocess]
strip_quotes = true
strip_signatures = true
strip_html = true
strip_base64 = true
strip_url_tracking = true
collapse_whitespace = true

[vector.embed.scope]
# Empty means embed the full archive. Set this for partial generations.
message_types = ["sms", "mms"]
# Use stable account identifiers, not numeric source IDs. This keeps a scoped
# generation usable after a daemon restart.
# accounts = ["you@work.example"]

[[synctech_sms.sources]]
name = "phone-backups"
enabled = true
backend = "drive"
folder_id = "google-drive-folder-id"
google_account = "you@gmail.com"
owner_phone = "+14155551234"
schedule = "30 4 * * *"

```

## Sender authentication

Pin the receiving mail server's authentication service ID for each email source:

```toml
[[sender_auth.sources]]
source_id = 1
trusted_authserv_ids = ["mx.google.com"]
```

Use the `source_id` from `msgvault list-accounts`. Source IDs must be positive
and unique across entries, and authentication service IDs must be nonblank with
no whitespace. Matching is case-insensitive and requires an exact ID. Restart
the daemon after changing these pins.

Single-message HTTP, CLI JSON, and MCP reads include `sender_auth` for pinned
email sources, evaluated from the topmost `Authentication-Results` header only.
`verified` means the pinned receiver reported `dmarc=pass` for the domain in
both the archived `From` header and the displayed sender address; it covers the
sender domain only. Empty results mean either the pinned receiver reported
nothing usable or msgvault could not read the archived headers. Sources without
pins and non-email messages omit the field.

Because a pin trusts the topmost header, a copy that never passed through the
pinned receiver can carry a forged one.

Receivers that omit the `authserv-id`, such as Exchange Online and Outlook.com
headers starting with `spf=...`, cannot verify. Quoted or spaced property
values also return no result.

### Relayed email senders

msgvault attributes GitHub notification emails to the author named in the
`X-GitHub-Sender` header while keeping the original `From` address available
for searches and authentication checks.

The header names a person without authenticating them, and handles can change.

To configure other relay addresses, add `[[relayed_senders]]` rules in
`config.toml`. A rule for an existing address replaces its built-in rule, and
each address can appear in only one rule:

```toml
[[relayed_senders]]
address = "relay@example.test"
header = "X-Author"
identifier_type = "example"
```

Each rule requires one mailbox address, a header name, and an identifier type.
If the header is missing, repeated, or empty, msgvault attributes the message
to its `From` address. Rules apply to new Gmail, IMAP, and Microsoft mail as
well as mbox, eml, emlx, and PST imports. GitHub bot handles are supported, and
the recorded `From` address stays visible on messages. Attribution for already
archived messages is separate future work.

### MCP Events

On unreleased `main`, enable owner subscriptions by adding:

```toml
[mcp_events]
enabled = true
```

The default is false. The daemon delivers Events while it is running, and
resumes from each subscription's saved position after a restart. Callback URLs and signing secrets are kept in `tokens/mcp-events-credentials.json` outside the archive.
A default restore resumes each subscription after its client refreshes its secret;
backups made with `--include-tokens` also restore the credentials. Older versions
ignore this separate file. Configured wake-ups
and Events subscriptions share a limit of 32 targets.
See [MCP Events](usage/chat.md#mcp-events) for subscription and verification.

### Wake-up webhooks

Wake-up configuration changes take effect after restarting the daemon.

Add a `[[wakeups]]` entry to notify an agent when matching incoming messages
are archived. The daemon checks every 15 seconds while it is running. Each
entry needs a unique lowercase name, an HTTPS URL and one secret source:

```toml
[[wakeups]]
name = "inbox"
url = "https://hooks.example.com/msgvault"
secret_env = "MSGVAULT_WAKEUP_SECRET"
# secret_file = "/run/secrets/msgvault-wakeup" # use instead of secret_env
# person_id = 12
# source_ids = [1, 2]
# conversation_ids = [3]
# incoming_only = true
# timeout = "10s"
# max_message_age = "72h"
```

The secret is a Standard Webhooks `whsec_` value containing 24–64 bytes in
canonical base64. HTTPS callbacks use port 443 or 8443. For an approved private
endpoint, `trusted_addresses` lists private IP addresses pinned to the URL's
origin; DNS is not used for that origin. At most 32 entries are allowed.
Names contain 1–48 lowercase letters, digits or hyphens. Selectors combine as
AND conditions; each ID list accepts at most 50 positive IDs. Without selectors,
all incoming messages qualify. Set `incoming_only = false` to include outgoing
messages. The timeout defaults to 10 seconds and cannot exceed 60 seconds.
The message age defaults to 72 hours.

See [Wake-up webhooks](api-server.md#wake-up-webhooks) for delivery and retry
behavior.
