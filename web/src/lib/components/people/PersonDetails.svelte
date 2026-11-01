<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import type { OperationStatusLane } from '../../operations/models';
  import { getSettingsPeopleInference } from '../../api/generated/api/api';
  import { formatContactDate } from '../../directory/labels';
  import PersonFields from './PersonFields.svelte';
  import PersonFacts from './PersonFacts.svelte';
  import OrganizationEmploymentTab from '../directory/OrganizationEmploymentTab.svelte';
  import RelationshipsTab from '../directory/RelationshipsTab.svelte';
  import PersonNetwork from '../directory/PersonNetwork.svelte';
  import PersonAgenda from '../directory/PersonAgenda.svelte';
  import PersonBriefCard from '../directory/PersonBriefCard.svelte';
  let { client, directory, personID, onOpenPerson = () => undefined, onAnnounce, onOpenSettings, operationLanes = [] }:
    { client: APIClient; directory: DirectoryController; personID: number; onOpenPerson?: (id: number) => void;
      onAnnounce?: (text: string) => void; onOpenSettings?: (category?: string) => void; operationLanes?: readonly OperationStatusLane[] } = $props();
  let organizationRequest = $state<{ id: number; key: number }>();
  let requestKey = 0;
  const profile = $derived(directory.selectedPersonID === personID ? directory.profile : null);
  const entity = $derived(directory.selectedPersonID === personID ? directory.entity : null);
  const bundle = $derived(directory.selectedPersonID === personID ? directory.detail : null);
  let providerEnabled = $state<boolean>();
  let providerError = $state('');
  let providerReload = $state(0);
  $effect(() => {
    void providerReload;
    const abort = new AbortController();
    providerError = '';
    void getSettingsPeopleInference({ ...client, signal: abort.signal }).then(({ data, error }) => {
      if (abort.signal.aborted) return;
      if (data && typeof data.running_enabled === 'boolean') providerEnabled = data.running_enabled;
      else providerError = error?.message ?? 'Could not load facts and briefs setup.';
    }).catch((cause) => { if (!abort.signal.aborted) providerError = cause instanceof Error ? cause.message : 'Could not load facts and briefs setup.'; });
    return () => abort.abort();
  });
  $effect(() => { const owner = profile; if (owner) untrack(() => void owner.reloadDefinitions()); });
</script>

<div class="details">
  {#if directory.detailLoading}<p role="status">Loading details…</p>{/if}
  {#if bundle}
    {#each Object.entries(bundle.errors) as [section, message]}
      <div role="alert">{section}: {message}<Button label="Retry" onclick={() => void directory.loadSelection(personID)} /></div>
    {/each}
  {/if}
  {#if profile}
    <PersonFields controller={profile} overdue={bundle?.contactState?.cadence_status === 'overdue'} />
    {#if bundle?.contactState}
      <p class="activity">{bundle.contactState.interaction_count} interactions{#if bundle.contactState.last_contact_at} · last contact {formatContactDate(bundle.contactState.last_contact_at)}{/if}</p>
    {/if}
    {#if entity}
      <OrganizationEmploymentTab controller={entity} {personID} {organizationRequest} />
      <RelationshipsTab {client} controller={entity} {personID} {onOpenPerson} />
      <PersonNetwork controller={entity} {onOpenPerson} onOpenOrganization={(id) => organizationRequest = { id, key: ++requestKey }} />
    {/if}
    <PersonAgenda {client} {personID} {onAnnounce} onSetUp={() => onOpenSettings?.('tasks')} />
    {#if providerError}<p role="alert">{providerError}<Button label="Retry setup" onclick={() => providerReload += 1} /></p>{/if}
    <PersonFacts {client} {personID} onSetUp={providerEnabled === false ? () => onOpenSettings?.('enrichment') : undefined} />
    <PersonBriefCard {client} {personID} {onAnnounce} onSetUp={providerEnabled === false ? () => onOpenSettings?.('enrichment') : undefined} />
  {/if}
</div>
<style>
  .details { display: flex; max-width: 720px; min-width: 0; flex-direction: column; gap: var(--section-gap); }
  .activity { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
