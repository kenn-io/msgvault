<script lang="ts">
  import DetailSection from '../people/DetailSection.svelte';
  import EmptyLine from '../common/EmptyLine.svelte';
  import InlineNotice from '../common/InlineNotice.svelte';
  import ConflictNotice from '../common/ConflictNotice.svelte';
  import { employmentConflictDetails, employmentFacts } from '../../directory/conflict';
  import Dialog from '../common/Dialog.svelte';
  import type { DirectoryEntityController } from '../../directory/entity-controller.svelte';
  import type { Employment, Organization, Prepared } from '../../directory/models';
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
  const acting = $derived(removal.busy || primary.busy);
  let openedOrganizationRequestKey = $state(0);

  $effect(() => {
    if (!organizationRequest || organizationRequest.key === openedOrganizationRequestKey) return;
    openedOrganizationRequestKey = organizationRequest.key;
    void openRequestedOrganization(organizationRequest.id);
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
    if (primaryTarget?.id !== employment.id) primary.reset(); primaryTarget = employment;
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
  <DetailRow actionLabel={`${employmentLabel(employment)} at ${controller.organizationName(employment.organization_id)}`} description={[dateSpan(employment.start_date, employment.end_date, employment.is_current), employment.department, employment.is_current && employment.is_primary && currentJobs.length > 1 ? 'Primary' : ''].filter(Boolean).join(' · ')} menu={[
    { label: 'Edit…', onselect: () => employmentEditor = employment },
    ...(employment.is_current ? [{ label: 'End…', onselect: () => endingEmployment = employment }] : []),
    ...(employment.is_current && !employment.is_primary ? [{ label: 'Make primary', disabled: acting, onselect: () => void makePrimary(employment) }] : []),
    { label: 'Delete…', danger: true, onselect: () => openDelete(employment) }
  ]}>
    {employmentLabel(employment)} at <button type="button" data-link onclick={() => openRequestedOrganization(employment.organization_id)}>{controller.organizationName(employment.organization_id)}</button>
  </DetailRow>
  {#if primary.failed && primaryTarget?.id === employment.id}
    {#if primary.conflict}
      <ConflictNotice noun="job" saved={primary.conflict.data} record={employmentFacts(primary.conflict.data, controller, personID)} message={employmentConflictDetails(primary.conflict.data, primary.details)} actionLabel="Retry" onaction={() => { if (primaryTarget) void makePrimary(primaryTarget); }} />
    {:else}<InlineNotice message="Couldn't make this job primary." details={primary.details} actionLabel="Retry" onaction={() => { if (primaryTarget) void makePrimary(primaryTarget); }} />{/if}
  {/if}
{/snippet}

{#if endingEmployment}
  <EmploymentEndDialog {controller} employment={endingEmployment} onClose={() => endingEmployment = undefined} />
{/if}
{#if organizationEditor !== undefined}
  <OrganizationEditor controller={controller} organization={organizationEditor ?? undefined} {onOpenPerson}
    onDone={() => { organizationEditor = undefined; }} onClose={() => { organizationEditor = undefined; }} />
{/if}
{#if employmentEditor !== undefined}
  <EmploymentEditor controller={controller} {personID} employment={employmentEditor ?? undefined}
    onDone={() => { employmentEditor = undefined; }} onClose={() => { employmentEditor = undefined; }} />
{/if}
{#if deletingEmployment}
  <Dialog title={`Delete ${employmentLabel(removal.conflict?.data ?? deletingEmployment)} at ${controller.organizationName((removal.conflict?.data ?? deletingEmployment).organization_id)}?`} width="small" primaryLabel="Delete" primaryTone="danger" onclose={closeDelete} onprimary={() => void removeEmployment()} busy={acting}>
    <p>This permanently removes the job. {#if deletingEmployment.is_current}<button data-link type="button" disabled={acting} onclick={() => { endingEmployment = deletingEmployment; closeDelete(); }}>End it instead…</button>{/if}</p>
    {#snippet notice()}
      {#if removal.conflict}<ConflictNotice noun="job" saved={removal.conflict.data} record={employmentFacts(removal.conflict.data, controller, personID)} message={employmentConflictDetails(removal.conflict.data, removal.details)} actionLabel="Retry" onaction={() => void removeEmployment()} />
      {:else if removal.failed}<InlineNotice message="Couldn't delete this job." details={removal.details} actionLabel="Retry" onaction={() => void removeEmployment()} />{/if}
    {/snippet}
  </Dialog>
{/if}

<style>
  .organization-employment { display: grid; min-width: 0; }
  .records { display: grid; }
  p { margin: 0; font-size: var(--type-body-size); line-height: var(--type-body-line); }
  [role="status"] { padding-block: var(--row-pad-y); color: var(--text-muted); font-size: var(--type-meta-size); line-height: var(--type-meta-line); }
</style>
