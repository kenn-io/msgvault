<script lang="ts">
  import XIcon from '@lucide/svelte/icons/x';
  import { Disclosure, Button, IconButton, copyToClipboard } from '@kenn-io/kit-ui';

  import type { DomainSummary, PersonSummary } from '../../explore/models';
  import type { LinkOutcome } from '../../relationships/controller.svelte';
  import { identityChipText } from '../../relationships/identity-chip';
  import DetailRow from './DetailRow.svelte';
  import DetailSection from './DetailSection.svelte';

  const STALE_CACHE_MESSAGE =
    'Identity saved; the cache refresh failed — groupings may be stale until a rebuild. Retrying is safe.';

  let { detail, onUnlinkParticipants, onAnnounce, listedInDetails = [] }:
    {
      detail: PersonSummary | DomainSummary | null;
      onUnlinkParticipants: (a: number, b: number) => Promise<LinkOutcome>;
      onAnnounce?: (message: string) => void;
      /** Details already lists this profile's own emails and phones, so they show once, there. */
      listedInDetails?: Pick<import('../../api/generated/models').PersonContactPoint, 'address_kind' | 'original_value'>[];
    } = $props();
  type LinkMutation = { kind: 'unlink'; a: number; b: number };

  let staleBanner = $state<'identity_cache_stale' | null>(null);
  let lastMutation = $state<LinkMutation | null>(null);
  let retrying = $state(false);
  let confirmingParticipantID = $state<number | null>(null);
  let unlinking = $state(false);
  let unlinkError = $state<string | null>(null);

  function isPersonDetail(value: PersonSummary | DomainSummary): value is PersonSummary {
    return 'identifiers' in value;
  }

  /** The open person's id, or null for a domain/empty detail. Read again
   * after every `await` in the mutation flows below instead of trusting a
   * value captured before the await: `detail` is a reactive prop, and
   * navigating to a different person (or a domain, or clearing the target)
   * while a link/unlink call is in flight replaces it out from under the
   * pending promise. */
  function currentPersonID(): number | null {
    return detail && isPersonDetail(detail) ? detail.id : null;
  }

  /** Cluster members (from PersonCluster.member_ids) with no row in
   * `identifiers` at all — e.g. linked purely by a manual participant link
   * with no stored email/phone evidence. Without a fallback chip for these,
   * such a member has no detach control anywhere in the UI: the identifier
   * loop below never renders anything for it. */
  const unrepresentedMembers = $derived.by((): number[] => {
    if (!detail || !isPersonDetail(detail) || !detail.cluster) return [];
    const known = new Set(identifiers.map((identifier) => identifier.participant_id));
    return (detail.cluster.member_ids ?? []).filter((id) => id !== detail.id && !known.has(id));
  });
  /** Each address shows once: with a saved profile, this person's own emails and phones are in Details,
   * and this section keeps only what it alone offers, linked profiles' addresses with Unlink and other identifiers. */
  const identifiers = $derived.by(() => {
    if (!detail || !isPersonDetail(detail)) return [];
    const all = detail.identifiers ?? [];
    const key = (kind: string, value: string) => `${kind}:${kind === 'phone' ? value.replace(/\D/g, '') : value.trim().toLowerCase()}`;
    const listed = new Set(listedInDetails.map(point => key(point.address_kind, point.original_value)));
    return all.filter(identifier => identifier.participant_id !== detail.id || !listed.has(key(identifier.type, identifier.value)));
  });
  const empty = $derived(identifiers.length === 0 && unrepresentedMembers.length === 0 && staleBanner === null && unlinkError === null);

  /** Evidence detail lives in the chip tooltip, in human words — internal
   * provenance identifiers never reach user-visible text. */
  function identifierTooltip(identifier: { type: string; is_primary: boolean; provenance: string }): string {
    const parts = [identifier.type, identifier.is_primary ? 'primary' : 'secondary'];
    if (identifier.provenance === 'participant_identifiers') parts.push('stored identifier');
    else if (identifier.provenance) parts.push(identifier.provenance.replaceAll('_', ' '));
    return parts.join(' · ');
  }

  function memberFor(participantID: number) {
    return detail && isPersonDetail(detail)
      ? detail.cluster?.members?.find((member) => member.participant_id === participantID) : undefined;
  }

  function edgesFor(participantID: number) {
    return detail && isPersonDetail(detail)
      ? (detail.cluster?.edges ?? []).filter((edge) => edge.participant_a === participantID || edge.participant_b === participantID)
      : [];
  }

  async function copyIdentifier(value: string): Promise<void> {
    const copied = await copyToClipboard(value);
    onAnnounce?.(copied ? 'Copied' : "Couldn't copy.");
  }

  // Navigating to a different person must not leave behind a stale banner
  // (or its Retry) bound to the previous cluster's IDs, and must not leave
  // a pending unlink confirm open on a chip that no longer belongs to the
  // now-open detail.
  let lastPersonID: number | null = null;
  $effect(() => {
    // Reloads briefly clear detail; keep the current person's UI state.
    if (!detail) return;
    const currentID = isPersonDetail(detail) ? detail.id : null;
    if (currentID === lastPersonID) return;
    lastPersonID = currentID;
    staleBanner = null;
    lastMutation = null;
    confirmingParticipantID = null;
    unlinkError = null;
  });

  // ok/ready clears any earlier stale banner; ok/stale (re)raises it and
  // remembers the mutation so Retry can safely re-invoke the identical,
  // idempotent link/unlink call.
  function applyOutcome(outcome: LinkOutcome, kind: 'unlink', a: number, b: number): void {
    if (!outcome.ok) return;
    if (outcome.cacheState === 'stale') {
      staleBanner = 'identity_cache_stale';
      lastMutation = { kind, a, b };
    } else {
      staleBanner = null;
      lastMutation = null;
    }
  }

  async function retryRefresh(): Promise<void> {
    if (!lastMutation || retrying) return;
    const id = currentPersonID();
    retrying = true;
    try {
      const { kind, a, b } = lastMutation;
      const outcome = await onUnlinkParticipants(a, b);
      if (currentPersonID() === id) applyOutcome(outcome, kind, a, b);
    } finally {
      retrying = false;
    }
  }

  function startUnlink(participantID: number): void {
    confirmingParticipantID = participantID;
    unlinkError = null;
  }

  function cancelUnlink(): void {
    confirmingParticipantID = null;
    unlinkError = null;
  }

  // Detaching one identity means removing every link edge that touches it,
  // not just one. A cluster built from hand-linked pairs can be a chain
  // rather than a star (a-b, b-c, c-d), so a member in the middle can be a
  // cut vertex joined to the rest through more than one edge; leaving any
  // incident edge in place would keep it joined via that edge even though
  // the user asked to detach it. Edges are removed sequentially and each
  // call is independently idempotent, so a retry after a partial failure
  // (network error mid-sequence) is always safe — already-removed edges
  // 200 as no-ops.
  async function confirmUnlink(participantID: number): Promise<void> {
    if (!detail || !isPersonDetail(detail) || !detail.cluster || unlinking) return;
    const id = detail.id;
    const incident = (detail.cluster.edges ?? []).filter(
      (edge) => edge.participant_a === participantID || edge.participant_b === participantID
    );
    if (incident.length === 0) {
      confirmingParticipantID = null;
      return;
    }
    unlinking = true;
    unlinkError = null;
    try {
      // The edge set was captured above, and the whole sequence runs to
      // completion even if the user navigates away mid-loop: stopping after
      // the current edge would persist a half-split cluster (some aliases
      // detached, others still merged) with no error anywhere. Navigating
      // away only makes the outcomes stale for THIS component's state
      // (which the $effect above already reset for whoever is open now), so
      // staleness suppresses the UI writes, never the mutations.
      for (const edge of incident) {
        const outcome = await onUnlinkParticipants(edge.participant_a, edge.participant_b);
        const stale = currentPersonID() !== id;
        if (!stale) applyOutcome(outcome, 'unlink', edge.participant_a, edge.participant_b);
        if (!outcome.ok) {
          // A genuine failure still ends the sequence — every call is
          // idempotent, so re-confirming retries the remaining edges safely.
          if (!stale) unlinkError = outcome.message;
          return;
        }
      }
      if (currentPersonID() === id) confirmingParticipantID = null;
    } finally {
      unlinking = false;
    }
  }
</script>

{#if detail && isPersonDetail(detail) && !empty}
<DetailSection title="Linked addresses">
  <div class="addresses" role="group" aria-label="Known addresses">
    {#if staleBanner === 'identity_cache_stale'}
      <section class="named-state" role="alert">
        <span>{STALE_CACHE_MESSAGE}</span>
        <Button label="Retry" surface="outline" size="sm" disabled={retrying} onclick={() => void retryRefresh()} />
      </section>
    {/if}
      {#key detail.id}
      <div class="identities">
        <div class="identifiers" role="group" aria-label="Linked identities">
        {#each identifiers as identifier (`${identifier.participant_id}:${identifier.type}:${identifier.value}`)}
          {@const isOtherMember = !!detail.cluster && identifier.participant_id !== detail.id}
          {@const opaque = identifier.type !== 'email' && identifier.type !== 'phone'}
          {@const edges = isOtherMember ? edgesFor(identifier.participant_id) : []}
          {@const member = memberFor(identifier.participant_id)}
          {@const text = identityChipText(identifier, member, edges)}
          {@const memberName = identifier.participant_display_name || member?.display_name}
          {@const chipName = opaque
            ? `${text.title} identifier for ${memberName ? `${memberName} (profile ${identifier.participant_id})` : `profile ${identifier.participant_id}`}`
            : identifier.display_value?.trim() || identifier.value}
          {@const relation = isOtherMember ? (edges.some((edge) => edge.link_origin) ? '' : 'linked') : 'this profile'}
          {@const label = identifier.service_label || (identifier.type === 'email' ? 'Email' : identifier.type === 'phone' ? 'Phone' : 'Address')}
          <DetailRow label={label === text.title ? undefined : label}>
            <div role="group" aria-label={`Identity ${chipName}`}><Disclosure variant="plain">{#snippet summary()}{text.title}{/snippet}<p>{[text.subtitle, relation, text.detail, identifierTooltip(identifier)].filter(Boolean).join(' · ')}</p></Disclosure></div>
            {#snippet actions()}
              <Button label="Copy" ariaLabel={`Copy ${opaque ? chipName : identifier.value}`} size="sm" onclick={() => void copyIdentifier(identifier.value)} />
              {#if isOtherMember && confirmingParticipantID !== identifier.participant_id}<Button label="Unlink" ariaLabel={`Unlink ${chipName}`} size="sm" onclick={() => startUnlink(identifier.participant_id)} />{/if}
            {/snippet}
            {#if isOtherMember && confirmingParticipantID === identifier.participant_id}
              <span class="chip-confirm" role="group" aria-label={`Confirm unlinking ${chipName}`}>
                <span>Unlink this address?</span>
                <Button label="Unlink" tone="danger" surface="solid" size="sm" disabled={unlinking} onclick={() => void confirmUnlink(identifier.participant_id)} />
                <Button label="Cancel" size="sm" disabled={unlinking} onclick={cancelUnlink} />
              </span>
            {/if}
          </DetailRow>
        {/each}
        {#each unrepresentedMembers as memberID (memberID)}
          {@const member = memberFor(memberID)}
          {@const text = identityChipText(undefined, member, edgesFor(memberID))}
          {@const chipName = `profile ${member?.display_name ? `${text.title} (${memberID})` : member?.email || member?.phone || memberID}`}
          <DetailRow label="Person"><span aria-label={`Linked ${chipName}`}>{text.title}<small>{text.subtitle}</small></span>
            {#if confirmingParticipantID === memberID}
              <span class="chip-confirm" role="group" aria-label={`Confirm unlinking ${chipName}`}>
                <span>Not the same person?</span>
                <Button
                  label="Unlink"
                  tone="danger"
                  surface="solid"
                  size="sm"
                  disabled={unlinking}
                  onclick={() => void confirmUnlink(memberID)}
                />
                <Button label="Cancel" surface="soft" size="sm" disabled={unlinking} onclick={cancelUnlink} />
              </span>
            {:else}
              <IconButton
                class="chip-unlink"
                size="sm"
                tone="danger"
                ariaLabel={`Unlink ${chipName}`}
                onclick={() => startUnlink(memberID)}
              >
                <XIcon size="12" aria-hidden="true" />
              </IconButton>
            {/if}
          </DetailRow>
        {/each}
        </div>
        {#if unlinkError}
          <p class="unlink-error" role="alert">{unlinkError}</p>
        {/if}
      </div>
      {/key}
  </div>
</DetailSection>
{/if}

<style>
  .addresses { display: grid; gap: var(--field-gap); }
  p { margin: 0; }
  .identifiers { display: grid; }
  .named-state, .chip-confirm { display: flex; flex-wrap: wrap; align-items: center; gap: var(--control-gap); }
  .unlink-error { color: var(--text-danger); }
  .addresses :global(.kit-disclosure p), small { color: var(--text-muted); font-size: var(--font-size-sm); overflow-wrap: anywhere; }
</style>
