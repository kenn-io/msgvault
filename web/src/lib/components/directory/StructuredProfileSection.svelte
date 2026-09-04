<script lang="ts">
  import { Button, TextInput } from '@kenn-io/kit-ui';

  import type { APIClient } from '../../api/client';
  import type { components } from '../../api/generated/schema';
  import type { PersonProfilePatchRequest } from '../../directory/models';
  import type { DirectoryProfileController } from '../../directory/profile-controller.svelte';
  import ProfileHistoryDialog from './ProfileHistoryDialog.svelte';
  import StructuredProfileEditor, { type StructuredProfileRecord, type StructuredProfileSectionName } from './StructuredProfileEditor.svelte';

  type ValueEnvelope = components['schemas']['ValueEnvelope'];
  type PersonContactPoint = components['schemas']['PersonContactPoint'];
  type ParticipantContactObservation = components['schemas']['ParticipantContactObservation'];

  interface Props {
    client: APIClient;
    controller: DirectoryProfileController;
    personID: number;
  }

  let { client, controller, personID }: Props = $props();
  let editing = $state<{ section: StructuredProfileSectionName; current?: StructuredProfileRecord }>();
  let confirming = $state<{ section: StructuredProfileSectionName; current: StructuredProfileRecord }>();
  let historyOpen = $state(false);
  let renaming = $state(false);
  let renameValue = $state('');
  let confirmingDelete = $state(false);
  let observations = $state<ParticipantContactObservation[]>([]);
  let observationsError = $state<string | null>(null);

  $effect(() => {
    const signature = (controller.structuredProfile?.contact_points ?? []).map((point) => [
      point.address_kind, point.normalized_value, point.normalization, point.normalization_version,
      point.service_slug, point.scope_kind, point.scope_value
    ].join('\u0000')).join('\u0001');
    if (!signature) {
      observations = [];
      observationsError = null;
      return;
    }
    const abort = new AbortController();
    void loadObservations(abort.signal);
    return () => abort.abort();
  });

  const sections: Array<{ section: StructuredProfileSectionName; title: string; singular: string }> = [
    { section: 'names', title: 'Names', singular: 'name' },
    { section: 'contact_points', title: 'Contact points', singular: 'contact point' },
    { section: 'addresses', title: 'Addresses', singular: 'address' },
    { section: 'dates', title: 'Dates', singular: 'date' },
    { section: 'categories', title: 'Categories', singular: 'category' },
    { section: 'media', title: 'Media metadata', singular: 'media metadata' }
  ];

  function rows(section: StructuredProfileSectionName): StructuredProfileRecord[] {
    const result = [...(controller.structuredProfile?.[section] ?? [])] as StructuredProfileRecord[];
    if (section === 'contact_points') {
      result.sort((left, right) => contactService(left).localeCompare(contactService(right)) || value(section, left).localeCompare(value(section, right)));
    }
    return result;
  }

  function contactService(record: StructuredProfileRecord): string {
    return (record as PersonContactPoint).service_slug?.trim() || 'other';
  }

  function sameObservation(point: PersonContactPoint, observation: ParticipantContactObservation): boolean {
    return point.address_kind === observation.address_kind &&
      point.normalized_value === observation.normalized_value &&
      point.normalization === observation.normalization &&
      point.normalization_version === observation.normalization_version &&
      (point.service_slug ?? '') === (observation.service_slug ?? '') &&
      (point.scope_kind ?? '') === (observation.scope_kind ?? '') &&
      (point.scope_value ?? '') === (observation.scope_value ?? '');
  }

  function backingObservation(record: StructuredProfileRecord): ParticipantContactObservation | undefined {
    const point = record as PersonContactPoint;
    return observations
      .filter((candidate) => sameObservation(point, candidate))
      .sort((left, right) => (right.observed_at ?? right.envelope.updated_at).localeCompare(left.observed_at ?? left.envelope.updated_at))[0];
  }

  function observationText(record: StructuredProfileRecord): string | null {
    const observation = backingObservation(record);
    if (!observation) return null;
    const when = observation.observed_at ?? observation.envelope.updated_at;
    return `Observed ${when}${observation.source_id === undefined ? '' : ` · Source ${observation.source_id}`}`;
  }

  async function loadObservations(signal: AbortSignal): Promise<void> {
    try {
      const response = await client.GET('/api/v1/people/{id}/profile/history', {
        params: { path: { id: personID } }, signal
      });
      if (signal.aborted) return;
      if (response.data) {
        observations = response.data.observations ?? [];
        observationsError = null;
        return;
      }
      observationsError = `Unable to load contact observations (${response.response.status})`;
    } catch (cause: unknown) {
      if (!signal.aborted) observationsError = cause instanceof Error ? cause.message : 'Unable to load contact observations';
    }
  }

  function value(section: StructuredProfileSectionName, record: StructuredProfileRecord): string {
    if (section === 'names') {
      const name = record as components['schemas']['PersonName'];
      return name.formatted ?? ([name.given_name, name.family_name].filter(Boolean).join(' ') || name.original_value);
    }
    if (section === 'dates') {
      const date = record as components['schemas']['PersonDate'];
      return `${date.label ?? date.date_kind}: ${date.date_text ?? date.original_value}`;
    }
    return record.original_value;
  }

  function providerContext(record: StructuredProfileRecord): string | null {
    const point = record as components['schemas']['PersonContactPoint'];
    const scope = point.scope_kind && point.scope_value
      ? `${point.scope_kind} / ${point.scope_value}`
      : point.scope_kind ?? point.scope_value;
    const details = [point.service_slug ? `Service: ${point.service_slug}` : undefined, scope ? `Scope: ${scope}` : undefined].filter(Boolean);
    return details.length ? details.join(' · ') : null;
  }

  function actionValue(section: StructuredProfileSectionName, record: StructuredProfileRecord): string {
    const base = value(section, record);
    if (section !== 'contact_points') return base;
    const point = record as components['schemas']['PersonContactPoint'];
    const scope = point.scope_kind && point.scope_value ? `${point.scope_kind} / ${point.scope_value}` : point.scope_kind ?? point.scope_value;
    const context = [point.service_slug, scope].filter(Boolean);
    return context.length ? `${base} — ${context.join(' — ')}` : base;
  }

  function isInlineMedia(section: StructuredProfileSectionName, record: StructuredProfileRecord): boolean {
    if (section !== 'media') return false;
    const media = record as components['schemas']['PersonMedia'];
    return media.has_data;
  }

  function provenance(envelope: ValueEnvelope): string {
    return [
      `Source: ${envelope.source}`,
      envelope.active_from ? `Valid from: ${envelope.active_from}` : undefined,
      envelope.active_until ? `Valid until: ${envelope.active_until}` : undefined,
      `Updated: ${envelope.updated_at}`
    ].filter(Boolean).join(' · ');
  }

  function closePatch(section: StructuredProfileSectionName, id: number): PersonProfilePatchRequest {
    switch (section) {
      case 'names': return { names: { supersede: [id] } };
      case 'contact_points': return { contact_points: { supersede: [id] } };
      case 'addresses': return { addresses: { supersede: [id] } };
      case 'dates': return { dates: { supersede: [id] } };
      case 'categories': return { categories: { supersede: [id] } };
      case 'media': return { media: { supersede: [id] } };
    }
  }

  async function closeCurrent(): Promise<void> {
    if (!confirming || !controller.canWriteProfile) return;
    const pending = confirming;
    const result = await controller.patchProfile(closePatch(pending.section, pending.current.envelope.id));
    if (result === undefined && controller.draft === null) confirming = undefined;
  }

  async function reload(): Promise<void> {
    const result = await controller.reload();
    if (result.ok && controller.draft === null) {
      confirming = undefined;
      confirmingDelete = false;
    }
  }

  function beginRename(): void {
    renameValue = controller.person?.display_name ?? '';
    renaming = true;
    confirmingDelete = false;
  }

  async function saveRename(): Promise<void> {
    const result = await controller.rename(renameValue.trim() || null);
    if (result === undefined && controller.draft === null) renaming = false;
  }

  async function deletePerson(): Promise<void> {
    const activeController = controller;
    const result = await activeController.deletePerson();
    if (result === undefined && activeController.draft === null) confirmingDelete = false;
  }

  function cancelEditor(): void {
    const localOnly = controller.draft === null && !controller.mutationPending && !controller.reloadPending;
    if (localOnly || controller.discardProfileDraft()) editing = undefined;
  }

</script>

<section class="structured-profile" aria-label="Structured profile">
  <header class="profile-header">
    <h3>Structured profile</h3>
    <div class="record-actions">
      <Button label="Rename person" size="sm" disabled={!controller.canWritePerson} onclick={beginRename} />
      <Button label="View profile history" size="sm" onclick={() => { historyOpen = true; }} />
      <Button label="Delete person" size="sm" tone="danger" disabled={!controller.canWritePerson}
        onclick={() => { confirmingDelete = true; renaming = false; }} />
    </div>
  </header>

  {#if renaming}
    <div class="person-action" role="group" aria-label="Rename person">
      <label>Display name<TextInput bind:value={renameValue} ariaLabel="Display name" block disabled={controller.mutationPending} /></label>
      <div class="record-actions">
        <Button label="Cancel rename" size="sm" disabled={controller.mutationPending} onclick={() => { renaming = false; }} />
        <Button label={controller.mutationPending ? 'Renaming…' : 'Save display name'} size="sm" disabled={!controller.canWritePerson} onclick={() => void saveRename()} />
      </div>
    </div>
  {/if}
  {#if confirmingDelete}
    <div class="person-action close-confirm" role="group" aria-label="Confirm deleting person">
      <span>Permanently delete this durable person profile?</span>
      <div class="record-actions">
        <Button label="Cancel delete" size="sm" disabled={controller.mutationPending} onclick={() => { confirmingDelete = false; }} />
        <Button label={controller.mutationPending ? 'Deleting…' : 'Confirm delete person'} size="sm" tone="danger" surface="solid" disabled={!controller.canWritePerson} onclick={() => void deletePerson()} />
      </div>
    </div>
  {/if}

  {#each sections as descriptor (descriptor.section)}
    <section class="profile-group">
      <header class="group-header">
        <h4>{descriptor.title}</h4>
        <Button label={`Add ${descriptor.singular}`} size="sm" disabled={!controller.canWriteProfile}
          onclick={() => { editing = { section: descriptor.section }; confirming = undefined; }} />
      </header>
      <ul>
        {#each rows(descriptor.section) as record, index (record.envelope.id)}
          {#if descriptor.section === 'contact_points' && (index === 0 || contactService(record) !== contactService(rows(descriptor.section)[index - 1]!))}
            <li class="service-heading"><h5>{contactService(record)}</h5></li>
          {/if}
          <li>
            <div class="record-copy">
              <strong>{value(descriptor.section, record)}</strong>
              {#if descriptor.section === 'contact_points' && providerContext(record)}<small>{providerContext(record)}</small>{/if}
              {#if descriptor.section === 'contact_points' && observationText(record)}<small>{observationText(record)}</small>{/if}
              {#if isInlineMedia(descriptor.section, record)}<small>Inline content is read-only here because metadata editing cannot preserve its stored bytes.</small>{/if}
              <small>{provenance(record.envelope)}</small>
            </div>
            <div class="record-actions">
              {#if !isInlineMedia(descriptor.section, record)}
                <Button label={`Edit ${descriptor.singular} ${actionValue(descriptor.section, record)}`} shortLabel="Edit" size="sm" disabled={!controller.canWriteProfile}
                  onclick={() => { editing = { section: descriptor.section, current: record }; confirming = undefined; }} />
              {/if}
              <Button label={`Close ${descriptor.singular} ${actionValue(descriptor.section, record)}`} shortLabel="Close" size="sm" tone="danger" disabled={!controller.canWriteProfile}
                onclick={() => { confirming = { section: descriptor.section, current: record }; editing = undefined; }} />
            </div>
            {#if confirming?.section === descriptor.section && confirming.current.envelope.id === record.envelope.id}
              <div class="close-confirm" role="group" aria-label={`Confirm closing ${descriptor.singular} ${actionValue(descriptor.section, record)}`}>
                <span>Close this current fact while keeping it in history?</span>
                <Button label="Cancel" size="sm" onclick={() => { confirming = undefined; }} />
                <Button label={`Confirm close ${descriptor.singular}`} size="sm" tone="danger" surface="solid" disabled={!controller.canWriteProfile} onclick={() => void closeCurrent()} />
              </div>
            {/if}
          </li>
        {:else}
          <li class="empty">No current {descriptor.title.toLowerCase()}.</li>
        {/each}
      </ul>
      {#if descriptor.section === 'contact_points' && observationsError}<p class="observation-error" role="status">{observationsError}</p>{/if}
      {#if editing?.section === descriptor.section}
        {#key editing.current?.envelope.id ?? 'new'}
          <StructuredProfileEditor controller={controller} section={descriptor.section} current={editing.current}
            onDone={() => { editing = undefined; }} onCancel={cancelEditor} />
        {/key}
      {/if}
    </section>
  {/each}

  {#if controller.conflict && !editing}
    <div class="profile-error" role="alert">
      {controller.conflict.code === 'person_revision_conflict' ? 'This person changed elsewhere. Reload and retry.' : controller.conflict.message}
      <Button label="Reload profile" size="sm" disabled={!controller.canReload} onclick={() => void reload()} />
    </div>
  {:else if !controller.structuredProfileETag && !editing}
    <div class="profile-error" role="status">
      <span>Profile revision unavailable. Reload to edit.</span>
      <Button label="Reload profile" size="sm" disabled={!controller.canReload} onclick={() => void reload()} />
    </div>
  {/if}
</section>

{#if historyOpen}
  <ProfileHistoryDialog {client} {personID} onClose={() => { historyOpen = false; }} />
{/if}

<style>
  .structured-profile, .profile-group, li, .record-copy, .person-action { display: grid; gap: var(--space-2); }
  .profile-header, .group-header, .record-actions, .close-confirm { display: flex; align-items: center; gap: var(--space-2); flex-wrap: wrap; }
  .profile-header, .group-header { justify-content: space-between; }
  h3, h4, h5, ul { margin: 0; }
  h4, h5 { color: var(--text-secondary); font-size: var(--font-size-sm); }
  ul { display: grid; gap: var(--space-2); padding: 0; list-style: none; }
  li { padding: var(--space-2); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); }
  .service-heading { padding: var(--space-1) 0 0; border: 0; }
  .record-actions { justify-content: flex-end; }
  small, .empty { color: var(--text-muted); font-size: var(--font-size-sm); }
  .close-confirm, .profile-error { padding: var(--space-2); background: var(--bg-inset); color: var(--text-secondary); font-size: var(--font-size-sm); }
  .person-action { padding: var(--space-3); border: 1px solid var(--border-muted); border-radius: var(--radius-sm); }
  .observation-error { margin: 0; color: var(--text-muted); font-size: var(--font-size-sm); }
  .profile-error { display: flex; align-items: center; justify-content: space-between; gap: var(--space-3); }
</style>
