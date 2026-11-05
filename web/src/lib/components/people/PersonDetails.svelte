<script lang="ts">
  import { Button } from '@kenn-io/kit-ui';
  import { untrack } from 'svelte';
  import type { APIClient } from '../../api/client';
  import type { DirectoryController } from '../../directory/controller.svelte';
  import { formatContactDate } from '../../directory/labels';
  import PersonFields from './PersonFields.svelte';
  import DetailRow from './DetailRow.svelte';
  import MeetingPanel from '../meetings/MeetingPanel.svelte';
  import PersonFacts from './PersonFacts.svelte';
  import OrganizationEmploymentTab from '../directory/OrganizationEmploymentTab.svelte';
  import RelationshipsTab from '../directory/RelationshipsTab.svelte';
  import PersonNetwork from '../directory/PersonNetwork.svelte';
  import PersonAgenda from '../directory/PersonAgenda.svelte';
  import PersonBriefCard from '../directory/PersonBriefCard.svelte';
  let { client, directory, personID, onOpenPerson = () => undefined, onAnnounce, onOpenSettings, sweepConfigured, onOpenMeeting }:
    { client: APIClient; directory: DirectoryController; personID: number; onOpenPerson?: (id: number) => void;
      onOpenMeeting?: (meeting: import('../../api/generated/models').MeetingRef) => void; sweepConfigured?: boolean; onAnnounce?: (text: string) => void; onOpenSettings?: (category?: string) => void } = $props();
  let organizationRequest = $state<{ id: number; key: number }>();
  let requestKey = 0;
  const profile = $derived(directory.selectedPersonID === personID ? directory.profile : null);
  const entity = $derived(directory.selectedPersonID === personID ? directory.entity : null);
  const bundle = $derived(directory.selectedPersonID === personID ? directory.detail : null);
  $effect(() => { const owner = profile, collection = entity; if (owner) untrack(() => { void owner.reloadDefinitions(); void collection?.load(); }); });
</script>

<div class="details">
  {#if directory.detailLoading}<p role="status">Loading details…</p>{/if}
  {#if bundle}
    {#each Object.entries(bundle.errors).filter(([section]) => section !== 'attributes' || !profile) as [, message]}
      <div role="alert">{message}<Button label="Retry" onclick={() => void (profile ? profile.reload() : directory.loadSelection(personID))} /></div>
    {/each}
  {/if}
  {#if profile}
    {@const contact = bundle?.contactState}
    <PersonFields controller={profile} overdue={contact?.cadence_status === 'overdue'}>
      {#snippet summary()}
        {#if contact && (contact.interaction_count > 0 || contact.last_contact_at)}
          <DetailRow label="Last contact">
            <span>{#if contact.last_contact_at}{formatContactDate(contact.last_contact_at)} · {/if}{contact.interaction_count.toLocaleString()} {contact.interaction_count === 1 ? 'interaction' : 'interactions'}</span>
          </DetailRow>
        {/if}
      {/snippet}
    </PersonFields>
    {#if entity}
      <OrganizationEmploymentTab controller={entity} {personID} {organizationRequest} />
      <RelationshipsTab {client} controller={entity} {personID} personName={profile.person?.display_name} {onOpenPerson} />
      <PersonNetwork controller={entity} {onOpenPerson} onOpenOrganization={(id) => organizationRequest = { id, key: ++requestKey }} />
    {/if}
    <PersonAgenda {client} {personID} refreshKey={String(profile.person?.revision)} {onAnnounce} onSetUp={() => onOpenSettings?.('tasks')} />
    <PersonFacts {client} {personID} refreshKey={profile.person} onSetUp={sweepConfigured === false ? () => onOpenSettings?.('enrichment') : undefined} />
    <PersonBriefCard {client} {personID} refreshKey={String(profile.person?.revision)} {onAnnounce} onSetUp={sweepConfigured === false ? () => onOpenSettings?.('enrichment') : undefined} />
    {#if profile.person && profile.person.participant_ids.length > 0}<MeetingPanel {client} scope={{ kind: 'direct', scope: { person_id: personID } }} refreshKey={String(profile.person.revision)} {onOpenMeeting} />{/if}
  {/if}
</div>
<style>
  .details { display: flex; max-width: 720px; min-width: 0; flex-direction: column; gap: var(--space-7); }
</style>
