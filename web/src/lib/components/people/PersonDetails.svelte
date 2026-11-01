<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { onDestroy, untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import type { OperationStatusLane } from '../../operations/models';
  import { OPERATION_SETUP } from '../../operations/labels';
  import { formatContactDate } from '../../directory/labels';
  import { PendingUndo } from '../../people/pending-undo.svelte';
  import PersonFields from './PersonFields.svelte';
  import PersonFacts from './PersonFacts.svelte';
  import OrganizationEmploymentTab from '../directory/OrganizationEmploymentTab.svelte';
  import RelationshipsTab from '../directory/RelationshipsTab.svelte';
  import PersonNetwork from '../directory/PersonNetwork.svelte';
  import PersonAgenda from '../directory/PersonAgenda.svelte';
  import PersonBriefCard from '../directory/PersonBriefCard.svelte';
  import UndoToast from '../common/UndoToast.svelte';
  let { client, directory, personID, onOpenPerson = () => undefined, onAnnounce, onOpenSettings, operationLanes = [] }:
    { client: APIClient; directory: DirectoryController; personID: number; onOpenPerson?: (id: number) => void;
      onAnnounce?: (text: string) => void; onOpenSettings?: (category?: string) => void; operationLanes?: readonly OperationStatusLane[] } = $props();
  const pending = new PendingUndo();
  let organizationRequest = $state<{ id: number; key: number }>();
  let requestKey = 0;
  const profile = $derived(directory.selectedPersonID === personID ? directory.profile : null);
  const entity = $derived(directory.selectedPersonID === personID ? directory.entity : null);
  const bundle = $derived(directory.selectedPersonID === personID ? directory.detail : null);
  const factsOff = $derived(operationLanes.flatMap((lane) => lane.kinds).find((kind) => kind.kind === 'person_sweep')?.configured === false);
  const setup = OPERATION_SETUP.person_sweep;
  $effect(() => { const owner = profile; if (owner) untrack(() => void owner.reloadDefinitions()); });
  onDestroy(() => { void pending.flush(); });
</script>

<div class="details">
  {#if directory.detailLoading}<p role="status">Loading details…</p>{/if}
  {#if bundle}
    {#each Object.entries(bundle.errors) as [section, message]}
      <div role="alert">{section}: {message}<Button label="Retry" onclick={() => void directory.loadSelection(personID)} /></div>
    {/each}
  {/if}
  {#if profile}
    <PersonFields controller={profile} {pending} overdue={bundle?.contactState?.cadence_status === 'overdue'} />
    {#if bundle?.contactState}
      <p class="activity">{bundle.contactState.interaction_count} interactions{#if bundle.contactState.last_contact_at} · last contact {formatContactDate(bundle.contactState.last_contact_at)}{/if}</p>
    {/if}
    {#if entity}
      <OrganizationEmploymentTab controller={entity} {personID} {organizationRequest} />
      <RelationshipsTab {client} controller={entity} {personID} {onOpenPerson} />
      <PersonNetwork controller={entity} {onOpenPerson} onOpenOrganization={(id) => organizationRequest = { id, key: ++requestKey }} />
    {/if}
    <PersonAgenda {client} {personID} {onAnnounce} onSetUp={() => onOpenSettings?.('tasks')} />
    <PersonFacts {client} {personID} onSetUp={factsOff && setup?.kind === 'settings' ? () => onOpenSettings?.(setup.target.settingsCategory) : undefined} />
    <PersonBriefCard {client} {personID} {onAnnounce} onSetUp={factsOff ? () => onOpenSettings?.('enrichment') : undefined} />
  {/if}
</div>
{#if pending.current}
  {#key pending.current.id}<UndoToast message={pending.current.message} onUndo={() => pending.undo()} onExpire={() => void pending.flush()} />{/key}
{/if}
<style>
  .details { display: flex; max-width: 720px; min-width: 0; flex-direction: column; gap: var(--section-gap); }
  .activity { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
</style>
