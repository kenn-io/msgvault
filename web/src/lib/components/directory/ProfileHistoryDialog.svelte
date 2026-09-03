<script lang="ts">
  import { Button, Modal } from '@kenn-io/kit-ui';
  import { onMount } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';

  type PersonProfileHistory = components['schemas']['PersonProfileHistory'];
  type ValueEnvelope = components['schemas']['ValueEnvelope'];

  interface Props {
    client: APIClient;
    personID: number;
    onClose: () => void;
  }

  let { client, personID, onClose }: Props = $props();
  let history = $state<PersonProfileHistory>();
  let loading = $state(true);
  let error = $state<string | null>(null);

  onMount(() => void load());

  async function load(): Promise<void> {
    loading = true;
    error = null;
    try {
      const { data, error: responseError, response } = await client.GET('/api/v1/people/{id}/profile/history', {
        params: { path: { id: personID } }
      });
      if (data) {
        history = data;
        return;
      }
      error = messageFor(responseError, response.status);
    } catch (cause: unknown) {
      error = messageFor(cause, 0);
    } finally {
      loading = false;
    }
  }

  function provenance(envelope: ValueEnvelope): string {
    return [
      `Source: ${envelope.source}`,
      envelope.active_from ? `Valid from: ${envelope.active_from}` : undefined,
      envelope.active_until ? `Valid until: ${envelope.active_until}` : undefined,
      envelope.superseded_at ? `Superseded: ${envelope.superseded_at}` : undefined,
      `Updated: ${envelope.updated_at}`
    ].filter(Boolean).join(' · ');
  }

  function historical(envelope: ValueEnvelope): boolean {
    return !!envelope.active_until || !!envelope.superseded_at;
  }

  function providerContext(point: { service_slug?: string; scope_kind?: string; scope_value?: string }): string | null {
    const scope = point.scope_kind && point.scope_value
      ? `${point.scope_kind} / ${point.scope_value}`
      : point.scope_kind ?? point.scope_value;
    const details = [point.service_slug ? `Service: ${point.service_slug}` : undefined, scope ? `Scope: ${scope}` : undefined].filter(Boolean);
    return details.length ? details.join(' · ') : null;
  }

  function messageFor(value: unknown, status: number): string {
    if (typeof value === 'object' && value !== null && 'message' in value) {
      const message = (value as { message?: unknown }).message;
      if (typeof message === 'string' && message) return message;
    }
    return status ? `Unable to load profile history (${status})` : 'Unable to load profile history';
  }
</script>

<Modal title="Profile history" ariaLabel="Profile history" closeLabel="Close profile history" onclose={onClose} maxWidth="min(760px, calc(100vw - 32px))">
  <div class="profile-history">
    {#if loading}
      <p role="status">Loading profile history…</p>
    {:else if error}
      <p role="alert" class="history-error">{error}</p>
    {:else if history}
      <section>
        <h3>Names</h3>
        <ul>
          {#each (history.names ?? []).filter((row) => historical(row.envelope)) as row (row.envelope.id)}
            <li><strong>{row.formatted ?? row.original_value}</strong><small>{provenance(row.envelope)}</small></li>
          {:else}<li class="empty">No superseded names.</li>{/each}
        </ul>
      </section>
      <section>
        <h3>Contact points</h3>
        <ul>
          {#each (history.contact_points ?? []).filter((row) => historical(row.envelope)) as row (row.envelope.id)}
            <li><strong>{row.original_value}</strong>{#if providerContext(row)}<small>{providerContext(row)}</small>{/if}<small>{provenance(row.envelope)}</small></li>
          {:else}<li class="empty">No superseded contact points.</li>{/each}
        </ul>
      </section>
      <section>
        <h3>Addresses</h3>
        <ul>
          {#each (history.addresses ?? []).filter((row) => historical(row.envelope)) as row (row.envelope.id)}
            <li><strong>{row.original_value}</strong><small>{provenance(row.envelope)}</small></li>
          {:else}<li class="empty">No superseded addresses.</li>{/each}
        </ul>
      </section>
      <section>
        <h3>Dates</h3>
        <ul>
          {#each (history.dates ?? []).filter((row) => historical(row.envelope)) as row (row.envelope.id)}
            <li><strong>{row.label ?? row.date_kind}: {row.date_text ?? row.original_value}</strong><small>{provenance(row.envelope)}</small></li>
          {:else}<li class="empty">No superseded dates.</li>{/each}
        </ul>
      </section>
      <section>
        <h3>Categories</h3>
        <ul>
          {#each (history.categories ?? []).filter((row) => historical(row.envelope)) as row (row.envelope.id)}
            <li><strong>{row.original_value}</strong><small>{provenance(row.envelope)}</small></li>
          {:else}<li class="empty">No superseded categories.</li>{/each}
        </ul>
      </section>
      <section>
        <h3>Media metadata</h3>
        <ul>
          {#each (history.media ?? []).filter((row) => historical(row.envelope)) as row (row.envelope.id)}
            <li><strong>{row.original_value}</strong><small>{provenance(row.envelope)}</small></li>
          {:else}<li class="empty">No superseded media metadata.</li>{/each}
        </ul>
      </section>
      <section>
        <h3>Service observations</h3>
        <ul>
          {#each history.observations ?? [] as row (row.envelope.id)}
            <li><strong>{row.original_value}</strong>{#if providerContext(row)}<small>{providerContext(row)}</small>{/if}<small>{provenance(row.envelope)}{row.observed_at ? ` · Observed: ${row.observed_at}` : ''}</small></li>
          {:else}<li class="empty">No service observations.</li>{/each}
        </ul>
      </section>
    {/if}
  </div>
  {#snippet footer()}
    <Button label="Close" onclick={onClose} />
  {/snippet}
</Modal>

<style>
  .profile-history { display: grid; gap: var(--space-4); min-width: min(42rem, calc(100vw - 64px)); }
  section, li { display: grid; gap: var(--space-1); }
  h3, p, ul { margin: 0; }
  ul { padding-left: var(--space-5); }
  small, .empty { color: var(--text-muted); font-size: var(--font-size-sm); }
  .history-error { color: var(--text-danger); }
</style>
