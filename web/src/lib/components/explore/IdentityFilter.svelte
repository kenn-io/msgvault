<script lang="ts">
  import { onDestroy } from 'svelte';

  import type { APIClient } from '../../api/client';
  import type {
    ExploreFilter,
    IdentityDirection,
    SourceIdentityResponse
  } from '../../explore/models';
  import { isValidSourceID } from '../../explore/models';

  let {
    client,
    filters,
    onChange
  }: {
    client: APIClient;
    filters: ExploreFilter[];
    onChange: (filters: ExploreFilter[]) => void;
  } = $props();

  let identities = $state<SourceIdentityResponse[]>([]);
  let loading = $state(false);
  let error = $state('');
  let requestGeneration = 0;
  let requestController: AbortController | undefined;

  const sourceID = $derived(singleSourceID(filters));
  const selectedIdentity = $derived(validIdentityFilter(filters, sourceID));
  const identifier = $derived(selectedIdentity?.values[1] ?? '');
  const direction = $derived(identityDirection(selectedIdentity?.values[2]));

  $effect(() => {
    const currentFilters = filters;
    const currentSourceID = singleSourceID(currentFilters);
    const identity = currentFilters.find((filter) => filter.dimension === 'identity');
    if (identity && (currentSourceID === undefined || identity.values[0] !== currentSourceID)) {
      onChange(withoutIdentity(currentFilters));
    }
  });

  $effect(() => {
    const requestedSourceID = sourceID;
    requestGeneration += 1;
    requestController?.abort();
    requestController = undefined;
    identities = [];
    error = '';
    if (requestedSourceID === undefined) {
      loading = false;
      return;
    }

    const numericSourceID = Number(requestedSourceID);
    const generation = requestGeneration;
    const controller = new AbortController();
    requestController = controller;
    loading = true;
    void client.GET('/api/v1/sources/{source_id}/identities', {
      params: { path: { source_id: numericSourceID } },
      signal: controller.signal
    }).then(({ data }) => {
      if (generation !== requestGeneration || controller.signal.aborted) return;
      if (!data) {
        error = 'Unable to load confirmed identities.';
        return;
      }
      identities = data.identities;
    }).catch((cause: unknown) => {
      if (generation !== requestGeneration || controller.signal.aborted) return;
      error = cause instanceof Error && cause.message
        ? cause.message
        : 'Unable to load confirmed identities.';
    }).finally(() => {
      if (generation === requestGeneration) loading = false;
    });
  });

  onDestroy(() => {
    requestGeneration += 1;
    requestController?.abort();
  });

  function singleSourceID(currentFilters: ExploreFilter[]): string | undefined {
    const source = currentFilters.find((filter) => filter.dimension === 'source');
    const value = source?.values.length === 1 ? source.values[0] : undefined;
    return isValidSourceID(value) ? value : undefined;
  }

  function validIdentityFilter(
    currentFilters: ExploreFilter[],
    currentSourceID: string | undefined
  ): ExploreFilter | undefined {
    const identity = currentFilters.find((filter) => filter.dimension === 'identity');
    return identity?.values.length === 3 && identity.values[0] === currentSourceID
      ? identity
      : undefined;
  }

  function identityDirection(value: string | undefined): IdentityDirection {
    return value === 'sender' || value === 'recipient' ? value : 'any';
  }

  function withoutIdentity(currentFilters: ExploreFilter[]): ExploreFilter[] {
    return currentFilters.filter((filter) => filter.dimension !== 'identity');
  }

  function replaceIdentity(nextIdentifier: string, nextDirection: IdentityDirection): void {
    const next = withoutIdentity(filters);
    if (sourceID !== undefined && nextIdentifier) {
      next.push({ dimension: 'identity', values: [sourceID, nextIdentifier, nextDirection] });
    }
    onChange(next);
  }
</script>

{#if sourceID !== undefined}
  <div class="identity-filter">
    <label>
      <span>Identity</span>
      <select
        aria-label="Identity"
        value={identifier}
        disabled={loading || (Boolean(error) && !identifier)}
        onchange={(event) => replaceIdentity(
          event.currentTarget.value,
          identifier ? direction : 'any'
        )}
      >
        <option value="">Any confirmed identity</option>
        {#if identifier && !loading && !identities.some((identity) => identity.identifier === identifier)}
          <option value={identifier}>{identifier} (unavailable)</option>
        {/if}
        {#each identities as identity (identity.identifier)}
          <option value={identity.identifier}>{identity.identifier}</option>
        {/each}
      </select>
    </label>
    <label>
      <span>Direction</span>
      <select
        aria-label="Identity direction"
        value={direction}
        disabled={!identifier}
        onchange={(event) => replaceIdentity(
          identifier,
          event.currentTarget.value as IdentityDirection
        )}
      >
        <option value="any">Any</option>
        <option value="sender">Sent via</option>
        <option value="recipient">Received via</option>
      </select>
    </label>
    {#if loading}
      <span role="status">Loading identities…</span>
    {:else if error}
      <span role="alert">{error}</span>
    {:else if identities.length === 0}
      <span role="status">No confirmed identities for this source.</span>
    {/if}
  </div>
{/if}

<style>
  .identity-filter,
  .identity-filter label {
    display: flex;
    align-items: center;
    gap: var(--space-2);
  }

  .identity-filter {
    flex-wrap: wrap;
  }

  .identity-filter label > span,
  .identity-filter > span {
    color: var(--text-muted);
  }

  .identity-filter select {
    min-height: var(--control-height);
    border: 1px solid var(--control-border);
    border-radius: var(--radius-sm);
    background: var(--control-bg);
    color: var(--text-primary);
  }
</style>
