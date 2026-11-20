---
last_edited: "2026-10-30"
title: Profile Automation
description: Maintain tracked people's profile facts from archive evidence and inspect why a value changed.
---

Keep profile details such as locations, interests, and employment up to date
for the people you choose to track. A people sweep reads a limited amount of
archived text, asks your chosen model for supported facts, and updates eligible
profile fields. Each update keeps its evidence and decision history.

Two related features have separate controls:

- [Person briefs](/docs/usage/people-briefs/) summarize recent conversations.
  They use the sweep provider, and you enroll each person individually.
- [External enrichment](/docs/usage/people-enrichment/) looks up public
  information from selected identifiers. It has its own providers and consent.

## Choose people to maintain

Before automation can maintain a profile, create a
[saved profile](/docs/usage/people/#promote-a-durable-person) and turn tracking
on for that person. In the Web UI, open their People page and choose
**More actions → Keep this profile updated**.

You can also track or untrack one or more people from the CLI by passing their
person IDs:

```bash
msgvault person track 7
msgvault person track 7 12 31
msgvault person untrack 7
```

If you are not sure who to track, list the saved people you have exchanged
messages or met with:

```bash
msgvault person tiers preview
msgvault person tiers explain 7
```

By default, preview lists two tiers:

- **Tier 1.** At least two messages each way and six days of contact in the
  last 90 days, or at least two messages each way and three days plus either
  two of email, chat, and calls or repeated meetings.
- **Tier 2.** Messages both ways and three days of contact in the last year, or
  repeated meetings.

An answered Synctech or Google Voice call of at least two minutes counts as one
message each way, so someone you mostly phone appears here too; `explain` shows
those calls on their own line. Each preview row shows the rule that matched the
person, whether they are already tracked, and the latest day that counted
toward the tier, and a header line names the timezone and direct limit used for
the counts. To see why someone is missing, run `explain`, which shows every
count against its threshold.

If the activity projection is still catching up, or was built under different
`[activity]` settings than your current configuration, both commands warn that
counts may be out of date. While the projection rebuilds after a change to
`[activity] timezone` or `max_direct_counterparts`, both commands ask you to
retry once it finishes. You can adjust the windows and thresholds in
[`[people.tiers]`](/docs/configuration/#people-tiers).

Running preview changes nothing. To track the people you want, pass their IDs
to `person track`. For example, to track everyone in tier 1 who is not tracked
yet:

```bash
msgvault person tiers preview --json \
  | jq -r '.people[] | select(.tier == 1 and (.tracked | not)) | .person_id' \
  | xargs msgvault person track
```

`person track` validates every ID before making changes, then tracks each
person in order. If the daemon rejects a person, such as a deleted profile,
the command stops there and everyone before it stays tracked.

Agents can read the same preview and explanations through the read-only
`preview_person_tiers` and `explain_person_tier`
[MCP tools](/docs/usage/chat/#available-tools) and the
[HTTP API](/docs/api-server/#get-apiv1peopletiers). Neither tracks anyone.

msgvault keeps no record of `person untrack`, so anyone you untracked earlier
appears as untracked in preview output. Read the list and pick the IDs
yourself.

Tracking makes a profile eligible for automatic maintenance. It does not grant
provider consent, enable a provider, or enroll a person in briefs. Untracking
stops msgvault from selecting that person for future maintenance while keeping
their saved profile intact.

## Configure a provider

For the standard provider setup, start with
[Recommended Configuration](/docs/usage/recommended-configuration/). To set up
a named profile yourself, follow the steps below. Every profile must pass a
synthetic check (a request containing fixed example text) and receive your
separate consent before it can send archive text.

If you do not want catalog discovery, add a profile with explicit custom
values. This path does not contact models.dev:

```bash
export ZAI_API_KEY="..."
msgvault person provider add glm --custom \
  --protocol openai_chat \
  --endpoint https://api.z.ai/api/paas/v4 \
  --model glm-5.3 \
  --auth bearer \
  --credential-env ZAI_API_KEY \
  --retention-posture provider-declared \
  --training-posture provider-declared \
  --source conversation_text \
  --source meeting_text \
  --source-since 2026-01-01 \
  --allow-sensitive \
  --reasoning-effort max \
  --yes
```

Include `--allow-sensitive` for real sweeps. Any packet with seed or context
evidence is marked sensitive, so a profile without this flag fails on every
real sweep. The flag permits sending that verbatim archive text to the selected
provider; without it, only the synthetic capability check, which sends no
archive text, can run.

To let interactive onboarding consult models.dev for discovery hints, omit
`--custom`. `provider add` is the only command that contacts models.dev; it is
not a runtime dependency and never receives archive content or credentials. A
catalog suggestion never chooses where a credential goes either. Onboarding
pairs a credential only with an endpoint you pass explicitly with `--endpoint`
or with the first-party API hosts compiled into msgvault. The add command tests
the selected endpoint with fixed synthetic input and saves the exact negotiated
protocol behavior, without granting consent to send archive text.

Next, review the saved policy, grant consent explicitly, and run a bounded
sweep:

```bash
msgvault person provider status glm
msgvault person provider consent glm --yes
msgvault person provider use glm
msgvault daemon restart
msgvault person sweep run --limit 5
msgvault person sweep status
```

You can grant consent with `provider consent <name>` while scheduling is still
disabled. Running `provider use <name>` selects that profile and enables people
sweeps, so the next manual run and future scheduled runs use it. By contrast,
`provider add` never switches the active selection or enables sweeps; it only
publishes the profile and records its synthetic check. A profile can exist
without being selected as long as people sweeps stay disabled.

The `add`, `set`, `use`, `remove`, `check`, `consent`, `reverify`, and `status`
subcommands accept `--json`. Running `status <name> --json` reports the profile
along with its recorded `check` and `consent` state, letting you inspect
provider gates without parsing prose. Tracking, eligible evidence, and remaining
budget still apply to each run.

`provider add`, `provider set`, `provider use`, and `provider remove` edit
`config.toml` where you run them. Run them on the daemon host; they refuse to
run when a remote daemon is selected, because that daemon reads its own
configuration. A running daemon keeps the people sweep configuration it started
with. After changing the selected provider or enabling sweeps, run
`msgvault daemon restart` so scheduled sweeps and `person brief generate` use
the new configuration. `person sweep run` reads the current configuration
separately.

To keep secrets out of `config.toml`, pass `--api-key-stdin` during
`provider add` to store a profile-specific key, or pass `--credential-env NAME`
to store only an environment variable name. Never put a secret value in a
command argument. Stored keys work on every platform and live in
`tokens/provider-credentials.json` alongside the other provider keys. Each key
is sent only to the endpoint origin (scheme, host, and port) of the profile it
was saved for. Keys stored on Linux or macOS by an older release move into that
file the first time msgvault uses the profile. Custom local gateways can use
`--custom`, and their synthetic check still calls the configured endpoint.

msgvault supports four HTTP protocols: `openai_chat`, `openai_responses`,
`anthropic_messages`, and `google_generate_content`. A gateway uses whichever
protocol it exposes, and a provider name is only your profile label. A gateway
may forward data to another operator; review the full routing path and its
privacy terms. Use subscription and logged-in endpoints only as their terms
allow. The `codex_app_server` protocol is present but release-gated: every Codex
operation currently fails closed until its executable isolation gate ships, and
`provider add` cannot create it. See
[Codex app-server profiles](/docs/configuration/#codex-app-server-profiles) for
details.

msgvault never switches providers automatically. If a response is locally
invalid, msgvault may make one repair call on the same resolved profile,
credential, endpoint, and model. Any change to the checked policy requires a
fresh check and consent.

## Change or recover a provider

To update a named profile's model, reasoning settings, privacy assertions,
source classes, date bounds, sensitive-text permission, or request timeout, use
`set`. It keeps the existing endpoint, protocol, authentication, and credential
source. If you need to change those connection details, add another profile.

```bash
msgvault person provider set glm --source-since 2026-06-01 --yes
msgvault person provider status glm
msgvault person provider consent glm --yes
msgvault daemon restart
```

`set` negotiates and checks the updated profile and revokes the old policy's
consent. Passing `--yes` to `set` does not consent to sending archive text.
Settings you leave out stay as they were, while a supplied `--source` list
replaces the previous list. If a saved-policy update fails and msgvault rolls
back the configuration, consent remains revoked.

After a msgvault upgrade, `status` can report that an earlier check or consent
used a different extraction program. Review the current disclosure, then renew
both gates in one step:

```bash
msgvault person provider status glm --json
msgvault person provider reverify glm --yes
```

`reverify` sends fixed synthetic input, records the check, and grants consent
only after that check succeeds. It does not send archive text or change the
active profile. JSON status includes `stale_program_check` and
`stale_program_consent`, so automation can distinguish an outdated grant from a
profile that has never been approved.

| Task | Command |
|---|---|
| List configured profiles | `msgvault person provider list` |
| Inspect one exact policy and gates | `msgvault person provider status glm --json` |
| Test without granting consent | `msgvault person provider check glm` |
| Revoke the selected policy's consent | `msgvault person provider revoke glm` |
| Revoke all stored sweep consents | `msgvault person provider revoke --all` |
| Read recent attempts for one profile | `msgvault person provider history glm --person 7` |

Revoking sweep consent does not revoke the separate consent for semantic
person embeddings or external enrichment.

## Run and inspect a sweep

Run a manual sweep or check recent sweep activity with these commands:

```bash
msgvault person sweep run --person 7 --limit 1
msgvault person sweep status --json
msgvault person sweep history --person 7 --limit 10 --json
```

Each run processes a bounded number of tracked people. `--limit` defaults to
25, reduced to the configured work-batch size when that is smaller. The daemon
also runs sweeps on the configured schedule; see
[people sweep configuration](/docs/configuration/#people-sweep-inference) for
source, input, timing, and budget controls.

New, edited, deleted, and reassigned archive evidence can change the facts that
automation considers supported. Sweeps retain earlier evidence and record its
current support status. They also revisit older material in bounded passes; to
request that pass explicitly, run `person sweep run --backstop --limit 5`.

Status and history are redacted operational records. Use them to inspect
progress, failures, and usage without printing message packets. A failed
provider call does not authorize a switch to another provider.

## Understand and correct automatic facts

Sweeps pick fields from the archive's current attribute definitions, including
eligible custom fields, plus employment, rather than using a fixed allowlist of
seeded fields. To be eligible, an attribute must be active, API-editable, and
non-derived, with a supported value type and a description of 1 to 280
characters. Attributes that link to another record are excluded. To inspect the
current set, run `msgvault person facts catalog --include-sensitive`.

Setting `allow_sensitive = true` permits both private archive evidence and
sensitive targets, including religion, politics, and Notes. There is no
separate switch for those two permissions. A claim that passes the evidence and
resolution rules can update the profile automatically; enabling a sweep does
not route every proposed change through manual approval. Pins and competing
evidence still control which values can change.

The fact ledger separates three records:

1. **Evidence** identifies the source that supports a statement.
2. **Claims** record proposed values about the person.
3. **Decisions** record whether the resolver applied, retained, rejected, or
   superseded a claim.

Before changing a profile, the resolver checks the target field, attribution,
evidence, and competing claims. Values you declare receive pins that prevent
automation from replacing them. You can also place an explicit pin to protect
an empty field, and unpinning lets the resolver reconsider retained claims.

```bash
msgvault person facts catalog
msgvault person facts evidence 7 --limit 20
msgvault person facts claims 7 --limit 20
msgvault person facts decisions 7 --limit 20
msgvault person facts evidence-status 7
msgvault person facts pins 7
```

Run `facts catalog` to look up a field's **kind**, **key**, and **revision**.
Attribute keys are stable universal IDs, not display labels or slugs. Copy the
key into `pin` or `unpin`; for example, employment has a fixed key:

```bash
msgvault person facts pin 7 employment system:employment
msgvault person facts unpin 7 employment system:employment
```

Evidence, claims, and decisions accept `--target` in the exact form
`kind:key:sha256:<64 lowercase hex characters>`. History commands accept
`--limit` (1 to 200), `--offset`, and `--json`. Running
`facts catalog --include-sensitive` shows sensitive targets for inspection; it
does not grant provider consent.

If a fact looks unsupported, check `evidence-status` before changing it. A
deleted, edited, or reassigned source can invalidate earlier support, and the
ledger keeps that history while the resolver updates eligible automatic values.
Make corrections directly in the current profile field; the history explains
how it reached that state.
