# Signal-based people tiers: implementation record

Approved scope: 2026-10-17. Archive schema 11; HTTP API schema 3.24.0.
This record preserves engineering decisions. Current operation belongs to
[profile automation](../usage/people-automation.md), exact settings to
[configuration](../configuration.md#people-tiers), and transport contracts to
the [API reference](../api-server.md#people-maintenance-tiers).

## Why maintenance uses tiers

Manual tracking alone makes every profile a separate setup task. A blanket
“track everyone” switch treats an imported address book as permission to run
sweeps, briefs, and scheduled lookups. Tiers instead infer the smallest useful
maintenance grant from archived interaction metadata. They do not measure a
person's value, infer emotional closeness, or authorize contacting them.

The approved boundary is resolved participants only. Signal follows
`activity_event_persons` bindings to saved profiles. Names in text, subjects,
snippets, transcripts, message bodies, and FTS are outside this feature. The
unlinked candidate list is observational: it returns a participant approximation
and an explicit promotion next step without creating, merging, or linking anyone.

## Evidence and exclusions

The activity spine supplies owner-relative direction, strongest participant
role and evidence, channel, occurrence time, and local calendar date. Tier
queries page people before finding inbound audiences. Both email recipients and
chat members count distinct participants, excluding confirmed identities for the
message's source. Email uses `to`, `cc`, and `bcc`; chat uses conversation
membership. This closes the spine classifier's inbound-broadcast gap without
changing its existing relationship calculations.

Message exchanges require valid direct roles: inbound senders and outbound
addressed recipients or members. Co-authors are not outbound recipients.
Meetings supply distinct days and channel breadth, not exchanged messages.
Distinct meeting dates avoid inflating evidence when two imports describe a
meeting on the same day. Future local dates are excluded. Direct recency keeps
all direct projected evidence through the evaluation date, including events
excluded from tier counts, with no age limit.

Owner profiles, automated-address-only profiles without phone or chat IDs, and
calendar-resource addresses are hard exclusions. List and large-audience
exclusions are counted for explanations. Pattern matching stays deliberately
small: exact lower-case match or a trailing-star prefix. Calendar resources
remain an exclusion while upstream resource classification is incomplete; the
rule can remain harmless once resources stop becoming saved people.

The candidate path reuses the activity loader and classifier to preserve
source-native ownership, authoritative From envelopes, owner-person bindings,
and membership fallback. It counts participant audiences before grouping by
canonical identity, and drops a group if any member is already saved. That
case belongs to identity review, not another profile promotion.

## Authority, observation, and manual choices

`person_tier_state` alone carries applied authority and saved enrichment lanes.
Every evaluated person retains a row, including Tier 3, because its
`run_started_at` rejects older continuations. `person_tier_observations` has a
separate shadow tier and grace history. Turning apply off freezes authority;
observational shadow state never seeds a later applied demotion.

`person_tracking` and `person_brief_enrollments` retain row-presence semantics
and add provenance. Existing rows are manual by definition; an unknown legacy
actor stays unknown. Positive human choices live in manual rows. Negative
choices live in durable lane opt-outs. Automation touches only its own rows.
Manual transitions and apply use the same enrichment authority mutation gate.
Apply re-reads manual grants, opt-outs, and exact manual work before committing.

A demotion needs a continuously weaker computed tier for the configured calendar
grace period. A weaker target may move between tiers without restarting that
period; returning to the applied tier clears it. Rule or semantic configuration
changes restart grace. Incomplete activity and open exact manual enrichment
work defer demotion. Promotions can use incomplete activity.

Readiness checks the pending activity queue, active timezone and direct-limit
transitions, reconciled identity revisions, and message watermark against the
maximum live message ID. A watermark is an ID, so its wall-clock age cannot
prove freshness. Tier jobs never run projection to make themselves ready.

## Frozen runs and stale continuations

Run records use `people_tiers_run` and `people_tiers_manual_run` in
`archive_metadata`. Each stores its run ID, mode, evaluation clock, complete
semantic tier configuration and fingerprint, projector timezone,
direct-counterpart limit, starting timestamp, and committed cursor.
Operational settings are excluded from the semantic fingerprint; changes take
effect on the next run. A page validates its frozen fields against the stored
record before changing anything.

Two guards solve different races. Applied rows reject an older run for that
person. The global `people_tiers_apply_fence` rejects an entire old run after a
manual apply or reset, including people whose state reset just removed.

`people_tiers_run_clock` is a runtime metadata key, not another archive schema
change. Under the authority mutation gate, every new run and reset allocates a
UTC microsecond timestamp strictly later than both the saved clock and apply
fence. Only manual apply and reset advance the apply fence. This keeps ordering
correct when an injected clock stays fixed or the wall clock moves backward;
otherwise a scheduled run could share a newer manual run's start and pass an
older-than fence check. The clock survives completed run-record removal.

Reset retains an existing scheduled run as a stale marker after advancing the
fence, and clears the manual run record. The scheduled callback rejects and
clears that marker before waiting for the next cron trigger. Deleting the marker
during reset would let an already queued follow-up mistake the empty slot for a
new run and recreate automatic grants immediately.

The scheduler uses the media scheduler that does not hold the operation gate
around a whole job. Each callback waits for the labeled gate, processes one page,
and releases the gate before requesting a continuation. The job is preemptible
and has a two-minute work budget that starts after gate admission, so time queued
behind an active operation does not consume the pass. Because this scheduler
does not hold the gate, the tier callback supplies its own waiter cancellation:
it checks for interactive request waiters on admission and while the page runs.
A clean yield requests a continuation; an independent failure remains visible.
For scheduled runs, each committed person's decision and next cursor are saved
in the same transaction. A partly processed page resumes after its last
committed person. Manual callers keep their page-level cursor response.

After the final page, the callback stops that watcher before the small retention
step, which remains inside the work budget. A late waiter must not cause a new
scan after the old run was cleared. Successful EOF returns the scheduler's bare
next-trigger deferral, so finalization cannot queue a new scan. Retention
failures use `scheduler.DeferError`: the complete error stays visible, including
joined independent failures, while the job waits for its next trigger.
Superseded scheduled work ends until the next cron trigger; it does not
immediately compete with a manual apply. Retention runs only after the last
page.

## Provider work and audit

Tiers grant eligibility, not consent or budget. Scheduled enrichment requires
manual tracking or persisted tier eligibility, and no enrichment opt-out. Every
existing provider policy, suppression, fact pin, and cap still applies.

Exact manual enrichment targets remain a separate authority path tied to run,
person, and profile fingerprint. Scheduled cancellation preserves those targets
and attempts and removes only scheduled trigger bits. Removing automatic
tracking is deferred while such work needs it. Identity-boundary removals keep
their stronger invalidation behavior.

`person_tier_events` preserves explainable transitions. Signal and lane payloads
are deterministic JSON with no identifier inputs or message text. Automatic
presence rows name `people-tiers`; manual apply audit names its request actor.
Lane changes take deterministic precedence over blocked-lane annotations.
Scheduled preview events have configurable retention; applied events remain
until permanent person deletion.

A per-person transaction failure does not prevent the page from continuing.
Three consecutive failing runs record `apply_failed`. Success clears the streak;
permanent deletion removes failure metadata, and the failure writer rechecks
existence so it cannot recreate that metadata after deletion.

Reset removes automatic grants and applied state, cancels lost scheduled
eligibility, and preserves manual grants, opt-outs, and audit. It reports open
manual work that prevents removal. Merge unions opt-outs, preserves manual
provenance, reconciles automatic rows, and carries audit to the survivor with its
origin. Split restores curated choices without treating derived observations as
historical authority. Subsets preserve the tier tables and provenance columns.

## Deliberate limits

This feature adds no provider, consent flow, automatic profile creation, or
name-mention signal. It does not change relationship scoring. Participant counts
can overcount a person with several addresses, making audience exclusion more
conservative. The candidate list is approximate until a user resolves identity.
A Directory tier badge is separate UI work. Any future change to these
boundaries needs its own reviewed behavior and tests.
