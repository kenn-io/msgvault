<script lang="ts">
  import { listSourceIdentities as generatedListSourceIdentities } from '../../api/generated/api/api';
  import { SelectDropdown } from '@kenn-io/kit-ui';
  import { onDestroy } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { ExploreFilter, IdentityDirection, SourceIdentityResponse } from '../../explore/models';
  import { filterID, identityDirection, sameFilterID, singleSourceValue } from '../../explore/models';
  import { trimAttributeText } from '../../directory/attribute-text';
  let {
    client,
    filters,
    onChange,
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
  const storedIdentifier = $derived(selectedIdentity?.values[1] ?? '');
  const identifier = $derived(trimAttributeText(storedIdentifier));
  const direction = $derived(identityDirection(selectedIdentity?.values[2]) ?? 'any');
  const identityOptions = $derived([
    { value: '', label: 'Any confirmed identity' },
    ...(identifier && !loading && !identities.some((identity) => identity.identifier === identifier)
      ? [{ value: identifier, label: `${identifier} (unavailable)` }]
      : []),
    ...identities.map((identity) => ({ value: identity.identifier, label: identity.identifier })),
  ]);
  const directionOptions = [
    { value: 'any', label: 'Any' },
    { value: 'sender', label: 'Sent via' },
    { value: 'recipient', label: 'Received via' },
  ];
  $effect(() => {
    const currentFilters = filters;
    const currentSourceID = singleSourceID(currentFilters);
    const identity = currentFilters.find((filter) => filter.dimension === 'identity');
    if (identity && (currentSourceID === undefined || !sameFilterID(identity.values[0], currentSourceID))) {
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
    const bigSourceID = filterID(requestedSourceID);
    const numericSourceID = Number(bigSourceID);
    // The catalog client takes a JSON-number ID. Keep the restored filter
    // usable without requesting a different source through a rounded number.
    if (!Number.isSafeInteger(numericSourceID)) {
      loading = false;
      error = 'Unable to load confirmed identities.';
      return;
    }
    const generation = requestGeneration;
    const controller = new AbortController();
    requestController = controller;
    loading = true;
    void generatedListSourceIdentities(
      { sourceId: numericSourceID },
      {
        ...client,
        signal: controller.signal,
      },
    )
      .then(({ data }) => {
        if (generation !== requestGeneration || controller.signal.aborted) return;
        if (!data) {
          error = 'Unable to load confirmed identities.';
          return;
        }
        identities = data.identities;
      })
      .catch((cause: unknown) => {
        if (generation !== requestGeneration || controller.signal.aborted) return;
        error = cause instanceof Error && cause.message ? cause.message : 'Unable to load confirmed identities.';
      })
      .finally(() => {
        if (generation === requestGeneration) loading = false;
      });
  });
  onDestroy(() => {
    requestGeneration += 1;
    requestController?.abort();
  });
  function singleSourceID(currentFilters: ExploreFilter[]): string | undefined {
    const value = singleSourceValue(currentFilters);
    return filterID(value) === undefined ? undefined : value;
  }
  function validIdentityFilter(
    currentFilters: ExploreFilter[],
    currentSourceID: string | undefined,
  ): ExploreFilter | undefined {
    const identity = currentFilters.find((filter) => filter.dimension === 'identity');
    return identity?.values.length === 3 && sameFilterID(identity.values[0], currentSourceID) ? identity : undefined;
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
      <SelectDropdown
        title="Identity"
        value={identifier}
        options={identityOptions}
        disabled={loading || (Boolean(error) && !identifier)}
        onchange={(value) => replaceIdentity(value, identifier ? direction : 'any')}
      />
    </label>
    <label>
      <span>Direction</span>
      <SelectDropdown
        title="Identity direction"
        value={direction}
        options={directionOptions}
        disabled={!identifier}
        onchange={(value) => replaceIdentity(storedIdentifier, value as IdentityDirection)}
      />
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
</style>
