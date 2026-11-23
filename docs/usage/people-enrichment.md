---
last_edited: "2026-09-08"
title: External Person Enrichment
description: Look up public profile information with explicit provider policies, request limits, and opt-outs.
---

Use external enrichment to look up public information about tracked people and
add supported results to their profile fact history. Enrichment sends selected
identifiers, such as a name and current company, to Exa or Sixtyfour. It does
not send archived conversations;
[profile sweeps](/docs/usage/people-automation/) handle facts extracted from
your archive.

Enrichment is disabled by default, and each provider needs its own policy,
request limits, and consent. Existing
[fact pins](/docs/usage/people-automation/#understand-and-correct-automatic-facts)
also apply to enrichment results.

## Choose a provider and eligible identifiers

| Provider mode | Identity required in the saved profile | Result path |
|---|---|---|
| Exa `people` | A public profile URL, or both name and current company | One typed person result; supported fields include location and employment |
| Exa `deep` or `deep-reasoning` | A public profile URL | Structured results for the requested supported fields |
| Sixtyfour | Both name and current company | An asynchronous job, polled until completion or its age limit |

`allowed_identifiers` lists the exact identifier classes the provider may
receive: `name`, `email`, `phone`, `current_company`, and
`public_profile_url`. Having a field locally does not override that list. For
Sixtyfour, the allowed list must include both `name` and `current_company`.

Returned results still need to match the person and pass the fact resolver's
evidence checks. A provider response is a proposed claim, not an instruction to
overwrite the profile. An unsupported field or insufficient identity can stop
the request before it produces a useful result.

## Configure a bounded lookup

Before configuring a provider, create a
[saved profile](/docs/usage/people/#promote-a-durable-person) with the required
identity details and run `msgvault person track 7`. Then run
`msgvault person facts catalog` to see which fields automation can maintain;
configuration uses the catalog's **key**, not its slug.

Review the provider's retention and training terms, then add the provider
configuration to `config.toml` on the daemon host. The example below requests
only the shipped `location` field from Exa. Replace the posture strings with
your explicit assessment; msgvault records these assertions, and it does not
establish the provider's terms.

```toml
[people.enrichment]
enabled = true
schedule = "*/15 * * * *"
batch_size = 5
suppression_key_env = "MSGVAULT_ENRICHMENT_SUPPRESSION_KEY"

[[people.enrichment.providers]]
name = "public-profile"
kind = "exa"
enabled = true
api_key_env = "EXA_API_KEY"
mode = "people"
allowed_identifiers = ["name", "current_company"]
# Stable key for the seeded location field; verify with person facts catalog.
target_keys = ["2068efb0-9808-498b-ac3a-8e0a87d4513a"]
retention_posture = "provider-declared"
training_posture = "provider-declared"
refresh_interval = "720h"
max_requests_per_run = 5
max_requests_per_day = 20
```

Set `EXA_API_KEY` and `MSGVAULT_ENRICHMENT_SUPPRESSION_KEY` in the daemon's
environment, then restart the daemon. The suppression key must be a persistent
secret of at least 32 bytes. It creates keyed fingerprints for opt-outs; keep
it with your deployment secrets and backups. Replacing it without migrating the
saved suppression state causes a key-mismatch error.

By default, Exa uses `https://api.exa.ai/search`. Sixtyfour uses
`https://api.sixtyfour.ai/people-intelligence-async` and
`https://api.sixtyfour.ai/job-status`, requires an explicit `tier`, and uses
`SIXTYFOUR_API_KEY` by default. Its request and polling endpoints must share
the same origin. Neither provider accepts HTTP endpoints.

For Sixtyfour, the polling interval defaults to 30 seconds, the maximum job age
to 15 minutes, and the retry count to 5. Provider requests default to a
one-minute timeout. You must set `refresh_interval`, `target_keys`,
`max_requests_per_run`, and `max_requests_per_day` explicitly. Sensitive
targets also require `allow_sensitive_targets = true`.

Current provider adapters enforce request limits and reject positive hard
monetary caps because they cannot guarantee the provider's maximum charge.
Leaving the three `max_cost_usd_micros_*` settings at zero selects
request-limit accounting; it does not make requests free.

## Review the policy and grant consent

Restarting the daemon with a valid configuration registers the exact provider
policy, without granting consent:

```bash
msgvault daemon restart
msgvault person enrichment profiles --json
msgvault person enrichment status --json
```

Review the full policy and copy the intended profile's fingerprint. Then grant
consent and run a lookup for one person with a unique operation key:

```bash
msgvault person enrichment consent <fingerprint>
msgvault person enrichment run --person 7 --provider public-profile \
  --idempotency-key person-7-initial-lookup --json
```

If you retry that operation, reuse the same idempotency key. The command prints
the saved run state. A job waiting on a provider or retry delay can stay in
progress, and the daemon resumes queued and running work on later scheduled
wakes. Tracking, changed identity, expired claims, and the configured refresh
interval can also create maintenance work.

Any change to the consented policy creates a different fingerprint and requires
new consent. Sweep-provider consent and semantic-embedding consent do not cover
external enrichment.

## Stop lookups for a person or provider

To record an opt-out for all of a person's current identifiers, suppress the
person. This mode does not accept `--provider` or identifier metadata:

```bash
msgvault person enrichment suppress --person 7 --reason opt_out
```

`--reason` accepts `opt_out` or `data_subject_request`. To suppress a single
identifier without putting its value in a command argument, provide it on
standard input:

```bash
msgvault person enrichment suppress --provider public-profile \
  --identifier-class email --reason opt_out < identifier.txt
```

The suppression history stores keyed digests instead of raw identifiers, and
msgvault checks suppression before making provider requests. Keep the
suppression key available even if enrichment is disabled: deleting a profile
with enrichment history needs it to record suppression for that person's
identifiers.

To stop a particular policy, revoke its fingerprint. To stop all currently
consented enrichment policies, pass `--all`:

```bash
msgvault person enrichment revoke <fingerprint>
msgvault person enrichment revoke --all
msgvault person enrichment status --json
```

Revocation stops future authorized lookups. It does not erase previously
returned facts or ask an external provider to delete information. To review
saved results, use `person facts evidence`, `claims`, and `decisions`.
