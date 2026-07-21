<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { DomainSummary, PersonSummary } from '../../explore/models';
  import type { LinkOutcome } from '../../relationships/controller.svelte';
  import LinkIdentityDialog from './LinkIdentityDialog.svelte';

  const STALE_CACHE_MESSAGE =
    'Identity saved; the cache refresh failed — groupings may be stale until a rebuild. Retrying is safe.';

  interface Props {
    detail: PersonSummary | DomainSummary | null;
    loading?: boolean;
    filesOpen: boolean;
    onFilesToggle: (value: boolean) => void;
    client: APIClient;
    onLinkParticipants: (a: number, b: number) => Promise<LinkOutcome>;
    onUnlinkParticipants: (a: number, b: number) => Promise<LinkOutcome>;
  }

  let {
    detail,
    loading = false,
    filesOpen,
    onFilesToggle,
    client,
    onLinkParticipants,
    onUnlinkParticipants
  }: Props = $props();

  type LinkMutation = { kind: 'link' | 'unlink'; a: number; b: number };

  let dialogOpen = $state(false);
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

  function displayLabel(value: PersonSummary | DomainSummary): string {
    return isPersonDetail(value) ? value.display_label : value.domain;
  }

  function formatDate(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.valueOf()) ? value : date.toLocaleDateString();
  }

  /** Cluster members (from PersonCluster.member_ids) with no row in
   * `identifiers` at all — e.g. linked purely by a manual participant link
   * with no stored email/phone evidence. Without a fallback chip for these,
   * such a member has no detach control anywhere in the UI: the identifier
   * loop below never renders anything for it. */
  const unrepresentedMembers = $derived.by((): number[] => {
    if (!detail || !isPersonDetail(detail) || !detail.cluster) return [];
    const known = new Set((detail.identifiers ?? []).map((identifier) => identifier.participant_id));
    return (detail.cluster.member_ids ?? []).filter((id) => id !== detail.id && !known.has(id));
  });

  // Navigating to a different person must not leave behind a stale banner
  // (or its Retry) bound to the previous cluster's IDs, and must not leave
  // a pending unlink confirm open on a chip that no longer belongs to the
  // now-open detail.
  let lastPersonID: number | null = null;
  $effect(() => {
    const currentID = detail && isPersonDetail(detail) ? detail.id : null;
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
  function applyOutcome(outcome: LinkOutcome, kind: 'link' | 'unlink', a: number, b: number): void {
    if (!outcome.ok) return;
    if (outcome.cacheState === 'stale') {
      staleBanner = 'identity_cache_stale';
      lastMutation = { kind, a, b };
    } else {
      staleBanner = null;
      lastMutation = null;
    }
  }

  async function confirmLink(participantID: number): Promise<LinkOutcome> {
    if (!detail || !isPersonDetail(detail)) throw new Error('Link identity requires an open person cluster');
    const id = detail.id;
    const outcome = await onLinkParticipants(id, participantID);
    // If navigation replaced `detail` mid-flight, this outcome belongs to a
    // person that is no longer open — don't repopulate the banner/lastMutation
    // for whoever is showing now with a result that was never about them.
    if (currentPersonID() === id) applyOutcome(outcome, 'link', id, participantID);
    return outcome;
  }

  async function retryRefresh(): Promise<void> {
    if (!lastMutation || retrying) return;
    const id = currentPersonID();
    retrying = true;
    try {
      const { kind, a, b } = lastMutation;
      const outcome = kind === 'link' ? await onLinkParticipants(a, b) : await onUnlinkParticipants(a, b);
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
      for (const edge of incident) {
        const outcome = await onUnlinkParticipants(edge.participant_a, edge.participant_b);
        // Navigating away mid-loop means this and every remaining edge's
        // result belongs to a person that's no longer open — stop touching
        // this component's state (which the $effect above already reset
        // for whoever is open now) rather than writing over it.
        if (currentPersonID() !== id) return;
        applyOutcome(outcome, 'unlink', edge.participant_a, edge.participant_b);
        if (!outcome.ok) {
          unlinkError = outcome.message;
          return;
        }
      }
      confirmingParticipantID = null;
    } finally {
      unlinking = false;
    }
  }
</script>

<header class="relationship-header" aria-label="Relationship detail">
  {#if !detail}
    <p role="status">{loading ? 'Loading relationship…' : 'Select a person or domain to see activity.'}</p>
  {:else}
    <div class="title-row">
      <h2>{displayLabel(detail)}</h2>
      <div class="actions">
        <Button
          label={`Files ${detail.file_count}`}
          ariaLabel={`Files ${detail.file_count}`}
          surface={filesOpen ? 'solid' : 'outline'}
          tone={filesOpen ? 'info' : 'neutral'}
          ariaExpanded={filesOpen}
          onclick={() => onFilesToggle(!filesOpen)}
        />
        {#if isPersonDetail(detail)}
          <Button
            label="Link identity"
            ariaLabel="Link identity"
            surface="outline"
            onclick={() => (dialogOpen = true)}
          />
        {/if}
      </div>
    </div>
    {#if staleBanner === 'identity_cache_stale'}
      <section class="named-state" role="alert">
        <span>{STALE_CACHE_MESSAGE}</span>
        <Button label="Retry" surface="outline" size="sm" disabled={retrying} onclick={() => void retryRefresh()} />
      </section>
    {/if}
    <p class="counts">
      {detail.activity_count.toLocaleString()} items · {detail.file_count.toLocaleString()} files ·
      {formatDate(detail.first_at)} – {formatDate(detail.last_at)}
      {#if !isPersonDetail(detail)}
        · {detail.person_count.toLocaleString()} people
      {/if}
    </p>
    {#if isPersonDetail(detail)}
      <div class="identifiers" aria-label="Archive-wide identity evidence">
        {#each detail.identifiers ?? [] as identifier (`${identifier.participant_id}:${identifier.type}:${identifier.value}`)}
          {@const isOtherMember = !!detail.cluster && identifier.participant_id !== detail.id}
          <span class="chip" aria-label={`Identity evidence ${identifier.display_value || identifier.value}`}>
            {#if identifier.display_value?.trim() && identifier.display_value.trim() !== identifier.value}
              <span class="chip-display">{identifier.display_value}</span>
            {/if}
            <strong>{identifier.value}</strong>
            <small>
              {identifier.type} · {identifier.is_primary ? 'Primary' : 'Secondary'} · {identifier.provenance}
              {#if detail.cluster}
                · {isOtherMember ? `linked identity #${identifier.participant_id}` : 'this identity'}
              {/if}
            </small>
            {#if isOtherMember}
              {#if confirmingParticipantID === identifier.participant_id}
                <span class="chip-confirm" role="group" aria-label={`Confirm detaching identity #${identifier.participant_id}`}>
                  <span>Detach from cluster?</span>
                  <Button
                    label="Detach"
                    tone="danger"
                    surface="solid"
                    size="sm"
                    disabled={unlinking}
                    onclick={() => void confirmUnlink(identifier.participant_id)}
                  />
                  <Button label="Cancel" surface="soft" size="sm" disabled={unlinking} onclick={cancelUnlink} />
                </span>
              {:else}
                <button
                  type="button"
                  class="chip-unlink"
                  aria-label={`Detach identity #${identifier.participant_id} from this cluster`}
                  onclick={() => startUnlink(identifier.participant_id)}
                >
                  ×
                </button>
              {/if}
            {/if}
          </span>
        {/each}
        {#each unrepresentedMembers as memberID (memberID)}
          <span class="chip" aria-label={`Identity evidence for identity #${memberID}`}>
            <small>identity #{memberID} · No stored identifier evidence · linked identity #{memberID}</small>
            {#if confirmingParticipantID === memberID}
              <span class="chip-confirm" role="group" aria-label={`Confirm detaching identity #${memberID}`}>
                <span>Detach from cluster?</span>
                <Button
                  label="Detach"
                  tone="danger"
                  surface="solid"
                  size="sm"
                  disabled={unlinking}
                  onclick={() => void confirmUnlink(memberID)}
                />
                <Button label="Cancel" surface="soft" size="sm" disabled={unlinking} onclick={cancelUnlink} />
              </span>
            {:else}
              <button
                type="button"
                class="chip-unlink"
                aria-label={`Detach identity #${memberID} from this cluster`}
                onclick={() => startUnlink(memberID)}
              >
                ×
              </button>
            {/if}
          </span>
        {/each}
      </div>
      {#if unlinkError}
        <p class="unlink-error" role="alert">{unlinkError}</p>
      {/if}
    {/if}
    {#if dialogOpen && isPersonDetail(detail)}
      <LinkIdentityDialog
        {client}
        excludeID={detail.id}
        onConfirm={confirmLink}
        onClose={() => (dialogOpen = false)}
      />
    {/if}
  {/if}
</header>

<style>
  .relationship-header {
    display: flex;
    flex-direction: column;
    gap: var(--space-3);
    padding-bottom: var(--space-3);
    border-bottom: 1px solid var(--border-muted);
  }

  .title-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
  }

  h2 {
    overflow: hidden;
    margin: 0;
    font-size: var(--font-size-lg);
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .actions {
    display: flex;
    flex: none;
    gap: var(--space-2);
  }

  .counts {
    margin: 0;
    color: var(--text-muted);
    font-size: var(--font-size-xs);
  }

  .named-state {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--space-3);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-md);
    padding: var(--space-3);
    font-size: var(--font-size-sm);
  }

  .identifiers {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: var(--space-2);
  }

  .chip {
    position: relative;
    display: flex;
    flex-direction: column;
    gap: 2px;
    border: 1px solid var(--border-default);
    border-radius: var(--radius-sm);
    padding: var(--space-2);
  }

  .chip-display {
    color: var(--text-secondary);
    font-size: var(--font-size-xs);
  }

  .chip small {
    color: var(--text-muted);
    font-size: var(--font-size-2xs);
  }

  .chip-unlink {
    position: absolute;
    top: var(--space-1);
    right: var(--space-1);
    border: none;
    background: transparent;
    color: var(--text-muted);
    font-size: var(--font-size-sm);
    line-height: 1;
    cursor: pointer;
    padding: 2px 4px;
  }

  .chip-unlink:hover {
    color: var(--text-danger);
  }

  .chip-confirm {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--space-2);
    margin-top: var(--space-1);
    font-size: var(--font-size-2xs);
  }

  .unlink-error {
    margin: 0;
    color: var(--text-danger);
    font-size: var(--font-size-xs);
  }
</style>
