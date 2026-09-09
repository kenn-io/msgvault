---
last_edited: "2026-09-08"
title: Profile Automation
description: Maintain tracked people's profile facts from archive evidence and inspect why a value changed.
---

A people sweep reads a limited amount of archived text, asks your chosen model
for supported facts, and updates eligible profile fields. Each update retains
its evidence and decision history. Use it to keep locations, interests,
employment, and other profile details current for people you choose to track.

Two other features have separate controls:

- [Person briefs](/docs/usage/people-briefs/) summarize recent conversations.
  They use the sweep provider but need enrollment for each person.
- [External enrichment](/docs/usage/people-enrichment/) looks up public
  information from selected identifiers. It has its own providers and consent.

## Choose people to maintain

Create a [saved profile](/docs/usage/people/#promote-a-durable-person), then turn
tracking on for that person:

```bash
msgvault person track 7
msgvault person untrack 7
```

Tracking makes a profile eligible for automatic maintenance. It does not itself
grant provider consent, enable a provider, or enroll a person in briefs.
Untracking stops that person from being selected for future maintenance and
keeps their saved profile.

## Configure a provider

For the standard provider setup, start with [Recommended
Configuration](/docs/usage/recommended-configuration/). To configure one named
profile yourself, follow the steps below. A profile must pass a synthetic check
(a request containing fixed example text) and receive your separate consent
before it can send archive text.

Add a profile with explicit custom values when you do not want catalog
discovery. This path does not contact models.dev:

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

`--allow-sensitive` is required for real sweeps: any packet with seed or
context evidence is marked sensitive, so a profile without this flag fails on
every real sweep. The flag permits sending that verbatim archive text to the
selected provider; without it only the synthetic capability check, which
sends no archive text, can run.

Omit `--custom` to allow interactive onboarding to consult models.dev for
discovery hints. Models.dev is used only by `provider add`; it is not a runtime
dependency and never receives archive content or credentials. A catalog
suggestion never chooses where a credential is sent either: onboarding pairs a
credential only with an endpoint you passed explicitly via `--endpoint` or
with the first-party API hosts compiled into msgvault. The add command
checks the selected endpoint with fixed synthetic input and saves the exact
negotiated protocol behavior. It does not grant consent to send archive text.

Review the saved policy, then consent explicitly and run a bounded sweep:

```bash
msgvault person provider status glm
msgvault person provider consent glm --yes
msgvault person provider use glm
msgvault daemon restart
msgvault person sweep run --limit 5
msgvault person sweep status
```

`provider consent <name>` can grant the checked profile while scheduling is
still disabled. `provider use <name>` selects that profile and enables people
sweeps, so the following manual run and future scheduled runs use it.
`provider add` never switches the active selection or enables sweeps: it only
publishes the profile and records its synthetic check. A profile can exist
without being selected as long as people sweeps stay disabled.

`add`, `set`, `use`, `remove`, `check`, `consent`, `reverify`, and `status`
accept `--json`.
`status <name> --json` reports the profile, its recorded `check`, and its
`consent` state. These explain the provider gates without parsing prose;
tracking, eligible evidence, and remaining budget still apply to each run.

`provider add`, `provider set`, `provider use`, and `provider remove` edit
`config.toml` where you run them. Run these commands on the daemon host.
They refuse when a remote daemon is selected, because that daemon reads its
own configuration. A running daemon keeps the people sweep configuration it started
with. Run `msgvault daemon restart` after changing the selected provider or
enabling sweeps so scheduled sweeps and `person brief generate` use the new
configuration. The `person sweep run` command reads the current configuration
separately.

Use `--api-key-stdin` during `provider add` to store a profile-specific key
outside `config.toml`, or `--credential-env NAME` to store only an environment
variable name. Never put the secret value in a command argument. Stored keys
are supported on Linux and macOS only; on other platforms `provider add`
refuses a stored credential before reading it, so pass `--credential-env`
there. Custom local gateways can use `--custom`; their synthetic check still
calls the configured endpoint.

Supported HTTP protocols are `openai_chat`, `openai_responses`,
`anthropic_messages`, and `google_generate_content`. A gateway uses the
protocol it exposes; a provider name is just your profile label. A gateway may
forward data to another operator, so review the full routing path and its
privacy terms. Use subscription and
logged-in endpoints only as their terms allow. The `codex_app_server`
protocol is present but release-gated: every Codex operation currently fails
closed until its executable isolation gate ships, and `provider add` cannot
create it; see [Codex app-server profiles](/docs/configuration/#codex-app-server-profiles).

Msgvault never switches providers automatically. A locally invalid response
may receive one repair call on the same resolved profile, credential, endpoint,
and model. Changes to the checked policy need a fresh check and consent.

## Change or recover a provider

Use `set` to update a named profile's model, reasoning settings, privacy
assertions, source classes, date bounds, sensitive-text permission, or request
timeout. It preserves the endpoint, protocol, authentication, and credential
source. To change those connection details, add another profile.

```bash
msgvault person provider set glm --source-since 2026-06-01 --yes
msgvault person provider status glm
msgvault person provider consent glm --yes
msgvault daemon restart
```

`set` negotiates and checks the new profile. It revokes the old policy's
consent; `--yes` on `set` does not consent to archive-text transmission.
Unspecified settings stay as they were, but a supplied `--source` list replaces
the previous list. If a saved-policy update fails and configuration is rolled
back, consent remains revoked.

After a msgvault upgrade, `status` can report that an earlier check or consent
used a different extraction program. Review the current disclosure, then renew
both gates with one command:

```bash
msgvault person provider status glm --json
msgvault person provider reverify glm --yes
```

`reverify` sends fixed synthetic input, records the check, and grants consent
only after that check succeeds. It does not send archive text or choose a
new active profile. JSON status includes `stale_program_check` and
`stale_program_consent`, so automation can distinguish an outdated grant from
a profile that has never been approved.

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

```bash
msgvault person sweep run --person 7 --limit 1
msgvault person sweep status --json
msgvault person sweep history --person 7 --limit 10 --json
```

A run processes a bounded number of tracked people. `--limit` defaults to 25,
reduced to the configured work-batch size when that is smaller. The daemon also
runs sweeps on the configured schedule. See
[people sweep configuration](/docs/configuration/#people-sweep-inference) for source,
input, timing, and budget controls.

New, edited, deleted, and reassigned archive evidence can change the facts that
automation considers supported. The sweep retains earlier evidence and records
its current support status. It also revisits older material in bounded passes;
`person sweep run --backstop --limit 5` explicitly requests that path.

Status and history are redacted operational records. Use them to inspect
progress, failures, and usage without printing message packets. A failed
provider call does not authorize a switch to another provider.

## Understand and correct automatic facts

The fact ledger separates three things:

1. **Evidence** identifies the source that supports a statement.
2. **Claims** record proposed values about the person.
3. **Decisions** record whether the resolver applied, retained, rejected, or
   superseded a claim.

The resolver checks the target field, attribution, evidence, and competing
claims before changing a profile. User-declared values receive pins that keep
automation from replacing them. An explicit pin also protects an empty field;
unpinning allows the resolver to reconsider retained claims.

```bash
msgvault person facts catalog
msgvault person facts evidence 7 --limit 20
msgvault person facts claims 7 --limit 20
msgvault person facts decisions 7 --limit 20
msgvault person facts evidence-status 7
msgvault person facts pins 7
```

Use `facts catalog` to obtain the field's **kind**, **key**, and **revision**.
Attribute keys are stable universal IDs, not display labels or slugs. Copy the
key into `pin` or `unpin`; for example, employment has a fixed key:

```bash
msgvault person facts pin 7 employment system:employment
msgvault person facts unpin 7 employment system:employment
```

Evidence, claims, and decisions accept `--target` in the exact form
`kind:key:sha256:<64 lowercase hex characters>`. History commands accept
`--limit` (1–200), `--offset`, and `--json`. `facts catalog --include-sensitive`
includes sensitive targets for inspection; it does not grant provider consent.

When inspecting an unsupported fact, check `evidence-status` before changing
it. A deleted, edited, or reassigned source can invalidate earlier support.
The ledger keeps that history while the resolver updates eligible automatic
values. Corrections belong in the current profile field; the history explains
how it reached that state.
