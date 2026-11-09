<script lang="ts">
  import { appShortcuts } from '@kenn-io/kit-ui';

  import DetailSection from '../people/DetailSection.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import Dialog from '../common/Dialog.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, Organization, Prepared } from '../../directory/models';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { employmentConflictDetails, employmentFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';
  import DetailRow from '../people/DetailRow.svelte';
  import { dateSpan } from '../../directory/labels';
  import EmploymentEditor from './EmploymentEditor.svelte';
  import EmploymentEndDialog from './EmploymentEndDialog.svelte';
  import OrganizationEditor from './OrganizationEditor.svelte';

  interface Props {
    controller: DirectoryEntityController;
    personID: number;
    organizationRequest?: { id: number; key: number };
    onOpenPerson?: (personID: number) => void;
  }

  let { controller, personID, organizationRequest = undefined, onOpenPerson = undefined }: Props = $props();
  let organizationEditor = $state<(Pick<Organization, 'id'> & Partial<Organization>) | null | undefined>(undefined);
  let employmentEditor = $state<Employment | null | undefined>(undefined);
  let endingEmployment = $state<Employment>();
  const currentJobs = $derived(controller.employments.filter(job => job.is_current));
  // The server lists jobs most recent first.
  const pastJobs = $derived(controller.employments.filter(job => !job.is_current));
  let deletingEmployment = $state<Employment>();
  const removal = new WriteAttempt<Prepared<Employment>>();
  const primary = new WriteAttempt<Prepared<Employment>>();
  let primaryTarget = $state<Employment>();
  let primaryPreparing = $state(false);
  const acting = $derived(removal.busy || primary.busy || primaryPreparing);
  let openedOrganizationRequestKey = $state(0);
  function holderJobs(organizationID: number, holderID: number): { label: string; open: () => void }[] {
    const jobs = holderID === personID ? controller.employments.filter(item => item.organization_id === organizationID) : [];
    return jobs.map(job => ({ label: jobs.length === 1 ? 'Delete job…' : `Delete ${employmentLabel(job)}…`, open: () => { openDelete(job); } }));
  }

  $effect(() => {
    if (!organizationRequest || organizationRequest.key === openedOrganizationRequestKey) return;
    openedOrganizationRequestKey = organizationRequest.key;
    void openRequestedOrganization(organizationRequest.id);
  });

  $effect(() => {
    if (!deletingEmployment) return;
    return appShortcuts.pushScope('directory-employment-delete');
  });


  function employmentLabel(employment: Employment): string {
    return employment.title ?? employment.role ?? 'Job';
  }

  function openRequestedOrganization(id: number): void {
    primary.reset();
    organizationEditor = { id, name: controller.organizationName(id) };
  }

  async function addEmployment(): Promise<void> {
    primary.reset();
    if (controller.organizations.length === 0) await controller.refreshOrganizations();
    employmentEditor = null;
  }

  async function makePrimary(employment: Employment): Promise<void> {
    if (acting) return;
    if (primaryTarget?.id !== employment.id) primary.reset();
    primaryTarget = employment;
    primaryPreparing = true;
    try {
      if (!primary.prepared && !await primary.prepare(() => controller.prepareEmploymentMutation(employment.id))) return;
      await primary.run(current => controller.makeEmploymentPrimary(employment.id, current));
    } finally {
      primaryPreparing = false;
    }
  }

  async function removeEmployment(): Promise<void> {
    if (!deletingEmployment || acting || !removal.prepared) return;
    const target = deletingEmployment;
    if (await removal.run(current => controller.deleteEmployment(target.id, current))) closeDelete();
  }

  function openDelete(employment: Employment): void {
    removal.reset();
    deletingEmployment = employment;
    void prepareDelete();
  }

  async function prepareDelete(): Promise<void> {
    if (!deletingEmployment) return;
    const current = await removal.prepare(() => controller.prepareEmploymentMutation(deletingEmployment!.id));
    if (current) deletingEmployment = current.data;
  }

  function closeDelete(): void {
    if (acting) return;
    if (removal.hadConflict) void controller.acceptSavedEmployment();
    deletingEmployment = undefined;
    removal.reset();
  }
</script>

<div class="organization-employment">
  <DetailSection title="Work" addLabel="Add job…" onadd={() => void addEmployment()}
    history={pastJobs.length ? pastRows : undefined} historyLabel={`${pastJobs.length} past ${pastJobs.length === 1 ? 'job' : 'jobs'}`}>
    {#if controller.employmentsLoading}<p role="status">Loading jobs…</p>{/if}
    {#if !controller.employments.length && !controller.employmentsLoading && !controller.errors.employments}<EmptyLine message="No work yet" />{/if}
    <div class="records">{#each currentJobs as employment (employment.id)}{@render jobRow(employment)}{/each}</div>
    {#if controller.errors.employments}<InlineNotice message="Couldn't load jobs." details={controller.errors.employments} actionLabel="Retry" onaction={() => void controller.refreshEmployments()} />{/if}
  </DetailSection>
</div>

{#snippet pastRows()}<div class="records">{#each pastJobs as employment (employment.id)}{@render jobRow(employment)}{/each}</div>{/snippet}
{#snippet jobRow(employment: Employment)}
  <!-- The organization is a link in the row's text, so the row itself takes no tap: Edit… is in ⋯, and a tap on a phone never lands on the wrong target. -->
  <DetailRow actionLabel={`${employmentLabel(employment)} at ${controller.organizationName(employment.organization_id)}`} description={[dateSpan(employment.start_date, employment.end_date, employment.is_current), employment.title && employment.role?.trim() !== employment.title.trim() ? employment.role : '', employment.department, employment.is_current && employment.is_primary && currentJobs.length > 1 ? 'Primary' : ''].filter(Boolean).join(' · ')} menu={[
    { label: 'Edit…', onselect: () => employmentEditor = employment },
    ...(employment.is_current ? [{ label: 'End…', onselect: () => endingEmployment = employment }] : []),
    ...(employment.is_current && !employment.is_primary ? [{ label: 'Make primary', disabled: acting, onselect: () => void makePrimary(employment) }] : []),
    { label: 'Delete…', danger: true, onselect: () => openDelete(employment) }
  ]}>
    {employmentLabel(employment)} at <button type="button" data-link onclick={() => openRequestedOrganization(employment.organization_id)}>{controller.organizationName(employment.organization_id)}</button>
  </DetailRow>
  {#if primary.conflict && primaryTarget?.id === employment.id}
    <ConflictNotice noun="job" saved={primary.conflict.data} record={employmentFacts(primary.conflict.data, controller, controller.personID)} message={employmentConflictDetails(primary.conflict.data, primary.details)} actionLabel="Keep yours" onaction={() => void makePrimary(employment)} />
    <button type="button" data-link disabled={acting} onclick={() => { primary.reset(); primaryTarget = undefined; void controller.acceptSavedEmployment(); }}>Use saved</button>
  {:else if primary.details && primaryTarget?.id === employment.id}
    <InlineNotice message="Couldn't make this job primary." details={primary.details} actionLabel="Retry" onaction={() => { if (primaryTarget) void makePrimary(primaryTarget); }} />
  {/if}
{/snippet}

{#if endingEmployment}
  <EmploymentEndDialog {controller} employment={endingEmployment} onClose={() => endingEmployment = undefined} />
{/if}
{#if organizationEditor !== undefined}
  {@const organizationID = organizationEditor?.id}
  <OrganizationEditor controller={controller} organization={organizationEditor ?? undefined} {onOpenPerson}
    holderJobs={organizationID === undefined ? undefined : (holderID) => holderJobs(organizationID, holderID)}
    onDone={() => { organizationEditor = undefined; }} onClose={() => { organizationEditor = undefined; }} />
{/if}
{#if employmentEditor !== undefined}
  <EmploymentEditor controller={controller} {personID} employment={employmentEditor ?? undefined}
    onDone={() => { employmentEditor = undefined; }} onClose={() => { employmentEditor = undefined; }} />
{/if}
{#if deletingEmployment}
  <Dialog title={`Delete ${employmentLabel(deletingEmployment)} at ${controller.organizationName(deletingEmployment.organization_id)}?`} width="small" primaryLabel="Delete" primaryTone="danger" primaryDisabled={!removal.prepared} onclose={closeDelete} onprimary={() => void removeEmployment()} busy={acting}>
    <p>This permanently removes the job. {#if deletingEmployment.is_current}<button data-link type="button" disabled={acting} onclick={() => { endingEmployment = deletingEmployment; closeDelete(); }}>End it instead…</button>{/if}</p>
    {#snippet notice()}
      {#if removal.conflict && deletingEmployment}
        <!-- Delete again removes the saved version the notice shows. -->
        <ConflictNotice noun="job" saved={removal.conflict.data} record={employmentFacts(removal.conflict.data, controller, controller.personID)} message={employmentConflictDetails(removal.conflict.data, removal.details)} />
      {:else if removal.details}<InlineNotice message="Couldn't delete this job." details={removal.details} actionLabel="Retry" onaction={() => { if (removal.prepared) void removeEmployment(); else void prepareDelete(); }} />{/if}
    {/snippet}
  </Dialog>
{/if}

<style>
  .organization-employment { display: grid; min-width: 0; }
  .records { display: grid; }
  p { margin: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  [role="status"] { padding-block: var(--row-pad-y); color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
