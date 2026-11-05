<script lang="ts">
  import { appShortcuts, Button, Modal, IconButton } from '@kenn-io/kit-ui';

  import Pencil from '@lucide/svelte/icons/pencil';
  import X from '@lucide/svelte/icons/x';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, Organization } from '../../directory/models';
  import EmploymentCurrentData from './EmploymentCurrentData.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import EmploymentEditor from './EmploymentEditor.svelte';
  import EmploymentEndPopover from './EmploymentEndPopover.svelte';
  import OrganizationEditor from './OrganizationEditor.svelte';

  interface Props {
    controller: DirectoryEntityController;
    personID: number;
    organizationRequest?: { id: number; key: number };
  }

  let { controller, personID, organizationRequest = undefined }: Props = $props();
  let organizationEditor = $state<(Pick<Organization, 'id'> & Partial<Organization>) | null | undefined>(undefined);
  let employmentEditor = $state<Employment | null | undefined>(undefined);
  let endingEmployment = $state<{ employment: Employment; trigger: HTMLElement }>();
  let deletingEmployment = $state<Employment>();
  let deleteConflictCurrent = $state<Employment>();
  let deleteMessage = $state('');
  let actionMessage = $state('');
  let actionConflictCurrent = $state<Employment>();
  let acting = $state(false);
  let openedOrganizationRequestKey = $state(0);

  $effect(() => {
    if (!organizationRequest || organizationRequest.key === openedOrganizationRequestKey) return;
    openedOrganizationRequestKey = organizationRequest.key;
    void openRequestedOrganization(organizationRequest.id);
  });

  $effect(() => {
    if (!deletingEmployment) return;
    return appShortcuts.pushScope('directory-employment-delete');
  });

  function organizationName(id: number): string {
    return controller.organizationRecords.get(id)?.organization.name ?? controller.organizations.find((item) => item.id === id)?.name
      ?? (controller.employmentProjection?.organization_id === id ? controller.employmentProjection.organization_name : undefined)
      ?? `Organization ${id}`;
  }

  function organizationRecord(id: number): Organization | undefined {
    return controller.organizations.find((item) => item.id === id);
  }

  function employmentLabel(employment: Employment): string {
    return employment.title ?? employment.role ?? 'Employment';
  }

  const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  // "Mar 2024 – present", or "2019 – 2021".
  function periodText(employment: Employment): string {
    const when = (value: { year?: number; month?: number } | undefined) => value?.year ? [value.month ? MONTHS[value.month - 1] : '', value.year].filter(Boolean).join(' ') : '';
    const start = when(employment.start_date);
    const end = when(employment.end_date) || (employment.is_current ? 'present' : '');
    return start && end ? `${start} – ${end}` : start ? `Since ${start}` : end && end !== 'present' ? `Until ${end}` : '';
  }

  function openRequestedOrganization(id: number): void {
    actionMessage = '';
    organizationEditor = { id };
  }

  async function addEmployment(): Promise<void> {
    actionMessage = '';
    actionConflictCurrent = undefined;
    if (controller.organizations.length === 0) await controller.refreshOrganizations();
    employmentEditor = null;
  }

  async function makePrimary(employment: Employment): Promise<void> {
    if (acting) return;
    acting = true;
    actionMessage = '';
    actionConflictCurrent = undefined;
    try {
      const result = await controller.makeEmploymentPrimary(employment.id);
      if (!result.ok) {
        if (result.kind === 'conflict') {
          actionConflictCurrent = result.current;
          actionMessage = `This employment changed elsewhere. ${result.message}`;
        } else actionMessage = result.message;
      }
    } finally {
      acting = false;
    }
  }

  async function removeEmployment(): Promise<void> {
    if (!deletingEmployment || acting) return;
    acting = true;
    deleteMessage = '';
    deleteConflictCurrent = undefined;
    const target = deletingEmployment;
    try {
      const result = await controller.deleteEmployment(target.id);
      if (result.ok) closeDelete();
      else if (result.kind === 'conflict') {
        deleteConflictCurrent = result.current;
        deleteMessage = `This employment changed elsewhere. ${result.message}`;
      } else deleteMessage = result.message;
    } finally {
      acting = false;
    }
  }

  function openDelete(employment: Employment): void {
    deleteMessage = '';
    deleteConflictCurrent = undefined;
    deletingEmployment = employment;
  }

  function closeDelete(): void {
    if (acting) return;
    deletingEmployment = undefined;
    deleteConflictCurrent = undefined;
    deleteMessage = '';
  }
</script>

<section class="organization-employment" aria-label="Organizations and employment">
  <header><h3>Work</h3><Button size="sm" label="Add work" onclick={() => void addEmployment()} /></header>
  <section aria-label="Employment history">
    {#if controller.errors.employments}
      <div class="refresh-error">
        <p role="alert">{controller.errors.employments}</p>
        <Button label="Refresh employment records" onclick={() => void controller.refreshEmployments()} />
      </div>
    {/if}
    {#if controller.employmentsLoading}<p role="status">Loading employment records…</p>{/if}
    {#if controller.employments.length === 0}
      {#if !controller.employmentsLoading}<p class="empty">No work yet</p>{/if}
    {:else}
      <div class="records">
        {#each controller.employments as employment (employment.id)}
          <DetailRow description={[periodText(employment), employment.department, employment.is_primary && controller.employments.filter((item) => item.is_current).length > 1 ? 'Primary' : ''].filter(Boolean).join(' · ')}>
            {employmentLabel(employment)} at <button type="button" class="org-link" onclick={() => void openRequestedOrganization(employment.organization_id)}>{organizationName(employment.organization_id)}</button>
            {#snippet actions()}
              {#if employment.is_current && !employment.is_primary}<Button size="sm" label="Make primary employment" shortLabel="Make primary" disabled={acting} onclick={() => void makePrimary(employment)} />{/if}
              {#if employment.is_current}<Button size="sm" label="End employment" shortLabel="End" ariaExpanded={endingEmployment?.employment.id === employment.id} onclick={(event) => { endingEmployment = { employment, trigger: event.currentTarget as HTMLButtonElement }; }} />{/if}
              <IconButton size="sm" ariaLabel="Edit employment" onclick={() => { employmentEditor = employment; }}><Pencil size={14} /></IconButton>
              <IconButton size="sm" ariaLabel="Delete employment" onclick={() => openDelete(employment)}><X size={14} /></IconButton>
            {/snippet}
          </DetailRow>
        {/each}
      </div>
    {/if}
    {#if actionMessage}
      <div role="alert">
        <p>{actionMessage}</p>
        {#if actionConflictCurrent}
          <EmploymentCurrentData employment={actionConflictCurrent} organization={organizationRecord(actionConflictCurrent.organization_id)} />
        {/if}
      </div>
    {/if}
  </section>


</section>

{#if endingEmployment}
  <EmploymentEndPopover {controller} employment={endingEmployment.employment} trigger={endingEmployment.trigger} onClose={() => { endingEmployment = undefined; }} />
{/if}
{#if organizationEditor !== undefined}
  <OrganizationEditor controller={controller} organization={organizationEditor ?? undefined}
    onDone={() => { organizationEditor = undefined; }} onClose={() => { organizationEditor = undefined; }} />
{/if}
{#if employmentEditor !== undefined}
  <EmploymentEditor controller={controller} {personID} employment={employmentEditor ?? undefined}
    onDone={() => { employmentEditor = undefined; }} onClose={() => { employmentEditor = undefined; }} />
{/if}
{#if deletingEmployment}
  <Modal closable={true} title="Delete employment" ariaLabel="Delete employment" closeLabel="Close delete employment" onclose={closeDelete}>
    <div class="delete-body">
    <p>Delete {employmentLabel(deletingEmployment)} at {organizationName(deletingEmployment.organization_id)}?</p>
    <p>This permanently removes the employment record.</p>
    {#if deletingEmployment.is_current}<p>End employment to keep it as past work.</p>{/if}
    {#if deleteMessage}
      <div role="alert">
        <p>{deleteMessage}</p>
        {#if deleteConflictCurrent}
          <EmploymentCurrentData employment={deleteConflictCurrent} organization={organizationRecord(deleteConflictCurrent.organization_id)} />
        {/if}
      </div>
    {/if}
    </div>
    {#snippet footer()}<Button label="Cancel" disabled={acting} onclick={closeDelete} /><Button tone="danger" surface="solid" label="Confirm delete employment" disabled={acting} onclick={() => void removeEmployment()} />{/snippet}
  </Modal>
{/if}

<style>
  .organization-employment { display: grid; gap: var(--space-2); min-width: 0; }
  section, .refresh-error { display: grid; gap: var(--field-gap); min-width: 0; }
  header { display: flex; gap: var(--control-gap); align-items: center; justify-content: space-between; min-height: var(--control-h); }
  h3, p { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  .records { display: grid; }
  .delete-body { display: grid; gap: var(--space-2); }
  .empty { color: var(--text-muted); font-size: var(--font-size-sm); }
  .org-link { padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; cursor: pointer; }
  .org-link:hover { text-decoration: underline; }
  [role="alert"] { color: var(--text-danger); }
</style>
