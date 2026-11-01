<script lang="ts">
  import { appShortcuts, Button, EmptyState, Modal, IconButton, Menu, MenuContent, MenuItem, MenuTrigger } from '@kenn-io/kit-ui';

  import Pencil from '@lucide/svelte/icons/pencil';
  import X from '@lucide/svelte/icons/x';
  import Ellipsis from '@lucide/svelte/icons/ellipsis';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, Organization } from '../../directory/models';
  import EmploymentCurrentData from './EmploymentCurrentData.svelte';
  import EmploymentEditor from './EmploymentEditor.svelte';
  import OrganizationEditor from './OrganizationEditor.svelte';

  interface Props {
    controller: DirectoryEntityController;
    personID: number;
    organizationRequest?: { id: number; key: number };
  }

  let { controller, personID, organizationRequest = undefined }: Props = $props();
  let organizationEditor = $state<Organization | null | undefined>(undefined);
  let employmentEditor = $state<Employment | null | undefined>(undefined);
  let employmentAction = $state<'edit' | 'end'>('edit');
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
    return controller.organizations.find((item) => item.id === id)?.name
      ?? (controller.employmentProjection?.organization_id === id ? controller.employmentProjection.organization_name : undefined)
      ?? `Organization ${id}`;
  }

  function organizationRecord(id: number): Organization | undefined {
    return controller.organizations.find((item) => item.id === id);
  }

  function employmentLabel(employment: Employment): string {
    return employment.title ?? employment.role ?? 'Employment';
  }

  function dateText(value: { year?: number; month?: number; day?: number } | undefined): string {
    if (!value) return '';
    return [value.year?.toString().padStart(4, '0'), value.month?.toString().padStart(2, '0'), value.day?.toString().padStart(2, '0')].filter(Boolean).join('-');
  }

  async function openRequestedOrganization(id: number): Promise<void> {
    actionMessage = '';
    try {
      const profile = await controller.prepareOrganizationMutation(id);
      organizationEditor = profile.organization;
    } catch (cause: unknown) {
      actionMessage = cause instanceof Error ? cause.message : 'Unable to open the organization.';
    }
  }

  async function addEmployment(): Promise<void> {
    actionMessage = '';
    actionConflictCurrent = undefined;
    if (controller.organizations.length === 0) await controller.refreshOrganizations();
    employmentAction = 'edit';
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
      {#if !controller.employmentsLoading}<EmptyState title="No work yet" />{/if}
    {:else}
      <ul class="records">
        {#each controller.employments as employment (employment.id)}
          <li>
            <div><strong>{employmentLabel(employment)}</strong> · <button type="button" class="org-link" onclick={() => void openRequestedOrganization(employment.organization_id)}>{organizationName(employment.organization_id)}</button>
              {#if employment.is_primary} <small>Primary</small>{/if}{#if employment.is_current} <small>Current</small>{/if}
              {#if employment.start_date || employment.end_date}<span class="dates">{dateText(employment.start_date)} – {dateText(employment.end_date) || 'present'}</span>{/if}
            </div>
            <div class="row-actions">
              <IconButton size="sm" ariaLabel="Edit employment" onclick={() => { employmentAction = 'edit'; employmentEditor = employment; }}><Pencil size={14} /></IconButton>
              {#if employment.is_current}<Button size="sm" label="End employment" shortLabel="End" onclick={() => { employmentAction = 'end'; employmentEditor = employment; }} />{/if}
              {#if employment.is_current && !employment.is_primary}<Button size="sm" label="Make primary employment" shortLabel="Make primary" disabled={acting} onclick={() => void makePrimary(employment)} />{/if}
              <IconButton size="sm" ariaLabel="Delete employment" onclick={() => openDelete(employment)}><X size={14} /></IconButton>
            </div>
          </li>
        {/each}
      </ul>
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

{#if organizationEditor !== undefined}
  <OrganizationEditor controller={controller} organization={organizationEditor ?? undefined}
    onDone={() => { organizationEditor = undefined; }} onClose={() => { organizationEditor = undefined; }} />
{/if}
{#if employmentEditor !== undefined}
  <EmploymentEditor controller={controller} {personID} employment={employmentEditor ?? undefined} action={employmentAction}
    onDone={() => { employmentEditor = undefined; }} onClose={() => { employmentEditor = undefined; }} />
{/if}
{#if deletingEmployment}
  <Modal title="Delete employment" ariaLabel="Delete employment" closeLabel="Close delete employment" onclose={closeDelete}>
    <p>Delete {employmentLabel(deletingEmployment)} at {organizationName(deletingEmployment.organization_id)}?</p>
    {#if deleteMessage}
      <div role="alert">
        <p>{deleteMessage}</p>
        {#if deleteConflictCurrent}
          <EmploymentCurrentData employment={deleteConflictCurrent} organization={organizationRecord(deleteConflictCurrent.organization_id)} />
        {/if}
      </div>
    {/if}
    {#snippet footer()}<Button label="Cancel" disabled={acting} onclick={closeDelete} /><Button tone="danger" surface="solid" label="Confirm delete employment" disabled={acting} onclick={() => void removeEmployment()} />{/snippet}
  </Modal>
{/if}

<style>
  .organization-employment, section, .refresh-error { display: grid; gap: var(--field-gap); min-width: 0; }
  header, .row-actions { display: flex; gap: var(--control-gap); align-items: center; justify-content: space-between; flex-wrap: wrap; }
  h3, p, ul { margin: 0; }
  h3 { font-size: var(--font-size-md); }
  .dates, small { color: var(--text-muted); font-size: var(--font-size-sm); }
  .records { list-style: none; padding: 0; }
  .records li { display: flex; align-items: center; justify-content: space-between; gap: var(--control-gap); padding: var(--space-2) 0; border-bottom: 1px solid var(--border-muted); flex-wrap: wrap; }
  .row-actions { justify-content: flex-end; opacity: 0; }
  li:hover .row-actions, li:focus-within .row-actions { opacity: 1; }
  .dates { display: block; }
  small { margin-left: var(--control-gap); }
  .org-link { padding: 0; border: 0; background: none; color: var(--link-ink); font: inherit; cursor: pointer; }
  [role="alert"] { color: var(--text-danger); }
  @media (hover: none) { .row-actions { opacity: 1; } }
</style>
