<script lang="ts">
  import { appShortcuts } from '@kenn-io/kit-ui';

  import DetailSection from '../people/DetailSection.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import Dialog from '../common/Dialog.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, Organization, Prepared } from '../../directory/models';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { conflict, employmentFacts } from '../../directory/conflict';
  import { WriteAttempt } from '../../directory/write-attempt.svelte';
  import { latestDateKey } from '../../directory/dates';
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
  // Most recent first: the latest end, then the latest start.
  const pastJobs = $derived(controller.employments.filter(job => !job.is_current)
    .sort((a, b) => latestDateKey(b.end_date) - latestDateKey(a.end_date) || latestDateKey(b.start_date) - latestDateKey(a.start_date)));
  let deletingEmployment = $state<Employment>();
  const removal = new WriteAttempt<Prepared<Employment>>();
  const primary = new WriteAttempt<Prepared<Employment>>();
  let primaryTarget = $state<Employment>();
  const acting = $derived(removal.busy || primary.busy);
  let openedOrganizationRequestKey = $state(0);
  function holderJob(organizationID: number, holderID: number): { label: string; open: () => void } | undefined {
    const job = holderID === personID ? controller.employments.find(item => item.organization_id === organizationID) : undefined;
    return job ? { label: 'Delete job…', open: () => { openDelete(job); } } : undefined;
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
    await primary.run(current => controller.makeEmploymentPrimary(employment.id, current));
  }

  async function removeEmployment(): Promise<void> {
    if (!deletingEmployment || acting) return;
    const target = deletingEmployment;
    if (await removal.run(current => controller.deleteEmployment(target.id, current))) closeDelete();
  }

  function openDelete(employment: Employment): void {
    removal.reset();
    deletingEmployment = employment;
  }

  function closeDelete(): void {
    if (acting) return;
    if (removal.conflict) void controller.refreshEmployments();
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
  <DetailRow actionLabel={`${employmentLabel(employment)} at ${controller.organizationName(employment.organization_id)}`} description={[dateSpan(employment.start_date, employment.end_date, employment.is_current), employment.title ? employment.role : '', employment.department, employment.is_current && employment.is_primary && currentJobs.length > 1 ? 'Primary' : ''].filter(Boolean).join(' · ')} menu={[
    { label: 'Edit…', onselect: () => employmentEditor = employment },
    ...(employment.is_current ? [{ label: 'End…', onselect: () => endingEmployment = employment }] : []),
    ...(employment.is_current && !employment.is_primary ? [{ label: 'Make primary', disabled: acting, onselect: () => void makePrimary(employment) }] : []),
    { label: 'Delete…', danger: true, onselect: () => openDelete(employment) }
  ]}>
    {employmentLabel(employment)} at <button type="button" data-link onclick={() => openRequestedOrganization(employment.organization_id)}>{controller.organizationName(employment.organization_id)}</button>
  </DetailRow>
  {#if primary.details && primaryTarget?.id === employment.id}
    {#if primary.conflict}
      <ConflictNotice noun="job" saved={primary.conflict.data} {...conflict(employmentFacts(primary.conflict.data, id => controller.organizationName(id)), employmentFacts(primaryTarget, id => controller.organizationName(id)), { 'Primary job': 'Yes' })} message={primary.details} actionLabel="Reload"
        onaction={() => void controller.refreshEmployments().then(() => { if (!controller.errors.employments) primary.reset(); })} />
    {:else}<InlineNotice message="Couldn't make this job primary." details={primary.details} actionLabel="Retry" onaction={() => { if (primaryTarget) void makePrimary(primaryTarget); }} />{/if}
  {/if}
{/snippet}

{#if endingEmployment}
  <EmploymentEndDialog {controller} employment={endingEmployment} onClose={() => endingEmployment = undefined} />
{/if}
{#if organizationEditor !== undefined}
  {@const organizationID = organizationEditor?.id}
  <OrganizationEditor controller={controller} organization={organizationEditor ?? undefined} {onOpenPerson}
    holderJob={organizationID === undefined ? undefined : (holderID) => holderJob(organizationID, holderID)}
    onDone={() => { organizationEditor = undefined; }} onClose={() => { organizationEditor = undefined; }} />
{/if}
{#if employmentEditor !== undefined}
  <EmploymentEditor controller={controller} {personID} employment={employmentEditor ?? undefined}
    onDone={() => { employmentEditor = undefined; }} onClose={() => { employmentEditor = undefined; }} />
{/if}
{#if deletingEmployment}
  <Dialog title={`Delete ${employmentLabel(deletingEmployment)} at ${controller.organizationName(deletingEmployment.organization_id)}?`} width="small" primaryLabel="Delete" primaryTone="danger" onclose={closeDelete} onprimary={() => void removeEmployment()} busy={acting}>
    <p>This permanently removes the job. {#if deletingEmployment.is_current}<button data-link type="button" disabled={acting} onclick={() => { endingEmployment = deletingEmployment; closeDelete(); }}>End it instead…</button>{/if}</p>
    {#snippet notice()}
      {#if removal.conflict && deletingEmployment}
        <!-- Delete again removes the saved version the notice shows. -->
        <ConflictNotice noun="job" saved={removal.conflict.data} {...conflict(employmentFacts(removal.conflict.data, id => controller.organizationName(id)), employmentFacts(deletingEmployment, id => controller.organizationName(id)), employmentFacts(deletingEmployment, id => controller.organizationName(id)))} message={removal.details} />
      {:else if removal.details}<InlineNotice message="Couldn't delete this job." details={removal.details} actionLabel="Retry" onaction={() => void removeEmployment()} />{/if}
    {/snippet}
  </Dialog>
{/if}

<style>
  .organization-employment { display: grid; min-width: 0; }
  .records { display: grid; }
  p { margin: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  [role="status"] { padding-block: var(--row-pad-y); color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
